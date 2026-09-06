# Rates Service — сервис котировок валютных курсов

Тестовое задание Plata (Go Engineer): асинхронный сервис котировок на Go.

[![CI](https://github.com/Crivoru4e4ka/rates-service/actions/workflows/ci.yml/badge.svg)](https://github.com/Crivoru4e4ka/rates-service/actions/workflows/ci.yml)

Пользователь запрашивает обновление котировки (`POST /quotes`), сервис сразу
возвращает идентификатор запроса и **в фоновом режиме** получает цену у
внешнего источника и сохраняет её в PostgreSQL. Позже клиент может узнать
статус запроса (`GET /quotes/requests/{id}`) или получить последнее значение
котировки (`GET /quotes?pair=EUR/MXN`).

## Возможности

- HTTP API в формате JSON (стандартный `net/http`, роутинг Go 1.22+)
- Сменные провайдеры котировок: **frankfurter.dev** (курсы ЕЦБ, без ключа,
  по умолчанию) и **exchangeratesapi.io** (с ключом) — выбор через `RATES_PROVIDER`
- Resilience-слой вокруг провайдера: повторы с экспоненциальным backoff и
  джиттером для 429/403/5xx, уважение `Retry-After`, глобальный rate-limiter
- Фоновое обновление: пул воркеров + очередь в памяти, повторный подбор
  pending-запросов (устойчиво к переполнению очереди и рестарту)
- Идемпотентность: заголовок `Idempotency-Key` и дедупликация pending-запросов
  по паре — обе на unique-индексах PostgreSQL, т.е. безопасны под гонками
- PostgreSQL (pgx/v5) + встроенные идемпотентные миграции при старте
- Наблюдаемость: `/healthz` (пинг БД), `/metrics` (формат Prometheus),
  `/version`, `X-Request-ID` в ответах и логах, `/debug/pprof` по флагу
- Graceful shutdown, структурированные логи (`slog`)
- Docker + docker-compose с healthcheck'ами, OpenAPI 3.0.3 + Swagger UI
- Unit-тесты + интеграционные тесты хранилища; CI (golangci-lint, `go test -race`
  с Postgres-сервис-контейнером, сборка образа)

## Быстрый старт (Docker)

```bash
cp .env.example .env    # и укажите свой ключ RATES_API_KEY
docker compose up -d --build
```

После запуска:

- API: http://localhost:8080
- Swagger UI: http://localhost:8080/swagger/
- OpenAPI-спека: http://localhost:8080/openapi.yaml
- Метрики: http://localhost:8080/metrics
- Версия: http://localhost:8080/version
- Smoke-тест: `./scripts/smoke.ps1`

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
| `RATES_PROVIDER` | `frankfurter` | провайдер котировок: `frankfurter` или `exchangeratesapi` |
| `RATES_API_URL` | по провайдеру | базовый URL API (`https://api.frankfurter.dev/v1` / `https://api.exchangeratesapi.io/v1`) |
| `RATES_API_KEY` | — | access_key (нужен только для `exchangeratesapi`) |
| `RATES_PROVIDER_TIMEOUT` | `5s` | таймаут одного вызова провайдера |
| `PROVIDER_MAX_ATTEMPTS` | `3` | попыток вызова провайдера (включая первую) |
| `PROVIDER_RETRY_BASE_DELAY` | `500ms` | базовая задержка между попытками (экспоненциальный backoff) |
| `PROVIDER_RETRY_MAX_DELAY` | `10s` | потолок задержки между попытками |
| `PROVIDER_MIN_INTERVAL` | `1s` | минимальный интервал между вызовами провайдера (глобально) |
| `SUPPORTED_CURRENCIES` | `USD,EUR,MXN` | допустимые валюты пар |
| `WORKERS` | `4` | число фоновых воркеров |
| `QUEUE_SIZE` | `1024` | размер очереди обновлений |
| `RESYNC_INTERVAL` | `30s` | период повторной постановки pending-запросов |
| `SHUTDOWN_TIMEOUT` | `15s` | время на graceful shutdown |
| `PPROF_ENABLED` | `false` | поднимать `/debug/pprof` |
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

## Наблюдаемость

- `GET /healthz` — пинг БД (для healthcheck'ов compose/K8s)
- `GET /version` — версия сборки (задаётся ldflags при сборке образа)
- `GET /metrics` — счётчики в формате Prometheus: HTTP-запросы по route/коду,
  итоги обновлений (completed/failed), вызовы провайдера (ok/error), суммарная
  длительность вызовов провайдера, длина очереди
- `X-Request-ID` — эхо в каждом ответе и поле `request_id` в логах
  (принимается от клиента или генерируется)
- `/debug/pprof` — поднимается при `PPROF_ENABLED=1`

## Архитектурные решения и trade-offs

**Каналы вместо очереди в БД.** Для single-instance — минимум инфраструктуры
и полный контроль backpressure. Слабое место in-memory очереди (потеря задач
при рестарте) закрыто: источник истины — таблица `update_requests`, при старте
и по тикеру pending-запросы переотправляются в очередь. При нескольких
инстансах очередь в памяти — первое, что нужно заменить
(`FOR UPDATE SKIP LOCKED` или брокер); идемпотентность при этом не пострадает —
она на unique-индексах БД.

**Кросс-курсы от анкора.** Бесплатный тариф exchangeratesapi.io фиксирует
базовую валюту, поэтому цена произвольной пары считается из курсов против
базы ответа: `price(BASE/QUOTE) = R[QUOTE] / R[BASE]`. Формула не зависит от
того, какую базу вернул API, и покрыта тестами. Trade-off — лишнее деление
(погрешность незначима для отображения котировок). Именно поэтому дефолтным
провайдером стал **frankfurter.dev**: открытый API референсных курсов ЕЦБ
без ключа и агрессивных лимитов, а exchangeratesapi.io остался опцией —
он назван в ТЗ, и переключение делается переменной окружения.

**Идемпотентность на уровне БД.** `Idempotency-Key` (unique-индекс) защищает
от повторов клиента; частичный unique-индекс «одна pending-задача на пару» —
от дублирования работы. Обе гарантии держатся даже при гонках и нескольких
инстансах, потому что проверяет их СУБД, а не приложение.

**422 вместо 400 для неподдерживаемой пары.** 400 — запрос некорректен по
форме (битый JSON, кривой формат пары); 422 — форма верна, но нарушено
бизнес-правило (валюта вне whitelist). Клиент может программно различить
«чинить формат» и «чинить список валют».

**Resilience-слой вокруг провайдера.** Внешние API котировок имеют лимиты и
WAF-защиту: обёртка `rates/resilient` добавляет глобальное ограничение частоты
(`PROVIDER_MIN_INTERVAL`), повторные попытки с экспоненциальным backoff и
джиттером для 429/403/5xx и уважение `Retry-After`. Запрос переводится в
`failed` только после исчерпания попыток — клиент видит честную причину.

**Чего сознательно нет.** Брокеров сообщений, кэшей, gRPC, OpenTelemetry —
на этом масштабе они решают несуществующие проблемы и усложняют проверку
задания. Точки роста описаны выше.

## Диаграммы

Асинхронное обновление котировки:

```mermaid
sequenceDiagram
    participant C as Клиент
    participant A as API
    participant D as PostgreSQL
    participant W as Воркер
    participant P as Внешний API

    C->>A: POST /quotes {"pair":"EUR/MXN"}
    A->>D: INSERT update_requests (pending)
    A-->>C: 202 {"id":"..."} + Location
    A->>W: задача в канал очереди
    W->>P: GET /latest?base=EUR&symbols=MXN
    P-->>W: {"rates":{"MXN":19.62}}
    W->>D: UPSERT quotes + UPDATE запрос (completed)
    C->>A: GET /quotes/requests/{id}
    A-->>C: 200 {"status":"completed","price":19.62}
```

Модель данных:

```mermaid
erDiagram
    quotes {
        text pair PK
        numeric price
        timestamptz updated_at
    }
    update_requests {
        uuid id PK
        text pair
        text status "pending | completed | failed"
        numeric price
        text error
        text idempotency_key UK
        timestamptz created_at
        timestamptz updated_at
    }
```

## Тесты

```bash
go test ./...
```

Unit-тесты интеграционные пропускают без `TEST_DATABASE_URL`; CI поднимает
Postgres-сервис-контейнер и гоняет их с `-race`.

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
