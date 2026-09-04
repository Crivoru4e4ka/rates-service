# Rates Service — сервис котировок валютных курсов

Тестовое задание Plata (Go Engineer): асинхронный сервис котировок на Go.

Пользователь запрашивает обновление котировки (`POST /quotes`), сервис сразу
возвращает идентификатор запроса и **в фоновом режиме** получает цену у
внешнего источника (exchangeratesapi.io) и сохраняет её в PostgreSQL.
Позже клиент может узнать статус запроса (`GET /quotes/requests/{id}`)
или получить последнее значение котировки (`GET /quotes?pair=EUR/MXN`).

## Возможности

- HTTP API в формате JSON (стандартный `net/http`, роутинг Go 1.22+)
- Фоновое обновление: пул воркеров + очередь в памяти, повторный подбор
  pending-запросов (устойчиво к переполнению очереди и рестарту)
- Идемпотентность: заголовок `Idempotency-Key` и дедупликация pending-запросов по паре
- PostgreSQL (pgx/v5) + встроенные идемпотентные миграции при старте
- Кросс-курсы: запрос к внешнему API выполняется с `base=EUR`, цена произвольной
  пары вычисляется из ответа — работает даже на тарифах с фиксированной базовой валютой
- Graceful shutdown, структурированные логи (`slog`), `/healthz` с пингом БД
- Docker + docker-compose, OpenAPI 3.0.3 + Swagger UI
- Unit-тесты + интеграционные тесты хранилища

## Быстрый старт (Docker)

```bash
cp .env.example .env    # и укажите свой ключ RATES_API_KEY
docker compose up -d --build
```

После запуска:

- API: http://localhost:8080
- Swagger UI: http://localhost:8080/swagger/
- OpenAPI-спека: http://localhost:8080/openapi.yaml

Postgres проброшен на хост как `localhost:5433` (5432 часто занят локальной
службой PostgreSQL).

## Локальный запуск без Docker

Нужен запущенный PostgreSQL 13+ (например, `docker compose up -d postgres`).

```bash
# Windows PowerShell
$env:DATABASE_URL = "postgres://rates:rates@localhost:5433/rates?sslmode=disable"
$env:RATES_API_KEY = "<ваш ключ>"
go run ./cmd/server

# Linux/macOS
DATABASE_URL=postgres://... RATES_API_KEY=... go run ./cmd/server
```

Миграции применяются автоматически при старте.

## Переменные окружения

| Переменная | По умолчанию | Описание |
|---|---|---|
| `HTTP_ADDR` | `:8080` | адрес HTTP-сервера |
| `DATABASE_URL` | `postgres://rates:rates@localhost:5432/rates?sslmode=disable` | подключение к PostgreSQL |
| `RATES_API_URL` | `https://api.exchangeratesapi.io/v1` | базовый URL внешнего API |
| `RATES_API_KEY` | — | access_key для exchangeratesapi.io |
| `RATES_PROVIDER_TIMEOUT` | `5s` | таймаут обращения к внешнему API |
| `SUPPORTED_CURRENCIES` | `USD,EUR,MXN` | допустимые валюты пар |
| `WORKERS` | `4` | число фоновых воркеров |
| `QUEUE_SIZE` | `1024` | размер очереди обновлений |
| `RESYNC_INTERVAL` | `30s` | период повторной постановки pending-запросов |
| `SHUTDOWN_TIMEOUT` | `15s` | время на graceful shutdown |
| `LOG_LEVEL` | `info` | debug / info / warn / error |
| `LOG_FORMAT` | `text` | text / json |

## API

### 1. Запросить обновление котировки

```bash
curl -X POST http://localhost:8080/quotes \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: my-key-1" \
  -d '{"pair":"EUR/MXN"}'
```

Ответ (`202`, а при повторе с тем же ключом — `200`):

```json
{"id":"5f0c9e2a-1b7c-4a5e-9d3f-2c6b8a1e4d7f","pair":"EUR/MXN","status":"pending","price":null,"error":null,"created_at":"...","updated_at":"..."}
```

### 2. Статус запроса по идентификатору

```bash
curl http://localhost:8080/quotes/requests/5f0c9e2a-...
```

- `status: "pending"` — обновление ещё выполняется
- `status: "completed"` — готово, заполнены `price` и `updated_at`
- `status: "failed"` — ошибка внешнего API, заполнено `error`

### 3. Последнее значение котировки

```bash
curl "http://localhost:8080/quotes?pair=EUR/MXN"
```

```json
{"pair":"EUR/MXN","price":19.617941,"updated_at":"2026-09-05T12:34:56Z"}
```

### Ошибки

```json
{"error":{"code":"unsupported_pair","message":"unsupported currency pair: EUR/GBP (поддерживаются: EUR,MXN,USD)"}}
```

Коды: `invalid_json`, `invalid_request`, `invalid_pair`, `invalid_id` (400),
`unsupported_pair` (422), `not_found` (404), `internal` (500).

## Идемпотентность обновления

1. **`Idempotency-Key`** — повторный POST с тем же заголовком возвращает тот же
   запрос (значение хранится в БД, unique-индекс).
2. **Дедупликация pending** — если у пары уже есть незавершённый запрос,
   возвращается он, дубликат не создаётся (частичный unique-индекс
   `ON update_requests (pair) WHERE status = 'pending'`).

## Архитектура

```
cmd/server                      — точка входа: конфиг, БД, миграции, HTTP, graceful shutdown
internal/config                 — конфигурация из переменных окружения
internal/domain                 — Pair, Quote, UpdateRequest, статусы, ошибки
internal/httpapi                — роуты, JSON-хендлеры, middleware (лог, recovery), OpenAPI + Swagger UI
internal/service                — бизнес-логика: очередь, пул воркеров, resync pending-запросов
internal/rates                  — интерфейс провайдера цен
internal/rates/exchangeratesapi — реализация провайдера (кросс-курсы от анкора)
internal/storage/postgres       — pgx-репозиторий + встроенные миграции
```

Поток обновления: `POST /quotes` → строка в `update_requests` (pending) →
очередь (канал) → воркер вызывает внешний API → upsert в `quotes` →
запрос переводится в `completed`/`failed`. При переполнении очереди или
рестарте pending-запросы переотправляются в очередь (`RESYNC_INTERVAL`,
а также при старте сервиса).

## Тесты

```bash
go test ./...    # unit-тесты (интеграционные пропускаются без TEST_DATABASE_URL)
```

Интеграционные тесты хранилища на реальном PostgreSQL:

```bash
docker compose up -d postgres
# Windows PowerShell:
$env:TEST_DATABASE_URL = "postgres://rates:rates@localhost:5433/rates?sslmode=disable"
go test ./internal/storage/postgres -v
```

## Заметки

- Валюты ограничены списком `SUPPORTED_CURRENCIES` (по умолчанию USD, EUR, MXN)
  — по условию задания достаточно небольшого набора.
- Цена хранится в `NUMERIC(20,10)`, в JSON отдаётся числом.
- Время — UTC (RFC 3339).
