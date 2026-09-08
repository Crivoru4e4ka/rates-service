# Rates Service — сервис котировок валют

Тестовое задание Plata: асинхронный сервис котировок на Go.

[![CI](https://github.com/Crivoru4e4ka/rates-service/actions/workflows/ci.yml/badge.svg)](https://github.com/Crivoru4e4ka/rates-service/actions/workflows/ci.yml)

Сервис принимает запрос на обновление котировки (`POST /quotes`), сразу возвращает ID задачи и в фоне получает курс от внешнего провайдера, сохраняя результат в PostgreSQL. Клиент может опрашивать статус задачи (`GET /quotes/requests/{id}`) или получить последнюю сохраненную цену (`GET /quotes?pair=EUR/MXN`).

## Возможности

- HTTP API в JSON (стандартный `net/http`, роутинг Go 1.22+)
- Сменные провайдеры котировок: **frankfurter.dev** (курсы ЕЦБ, без ключа,
  по умолчанию) и **exchangeratesapi.io** (тоже ЕЦБ, но с ключом)
- Отказоустойчивость провайдеров: автоматический фолбэк на резервного провайдера, если недоступен основной, ретраи с экспоненциальным backoff и джиттером для 429/5xx, поддержка заголовка `Retry-After`, глобальный rate-limiter
- TTL-кэш ответов провайдера в памяти (`PROVIDER_CACHE_TTL`): повторные запросы
  одной пары не тратят квоту внешнего API; hit/miss в `/metrics`
- Фоновое обновление: worker pool + очередь в памяти, повторный подбор
  pending-запросов (защита от потери задач при рестарте)
- Идемпотентность: заголовок `Idempotency-Key` и дедупликация pending-запросов
  по паре — обе на unique-индексах PostgreSQL, т.е. безопасны под гонками
- PostgreSQL (pgx/v5) + встроенные идемпотентные миграции при старте
- Наблюдаемость: `/healthz` (пинг БД), `/metrics` (формат Prometheus),
  `/version`, `X-Request-ID` в ответах и логах, `/debug/pprof` по флагу
- Graceful shutdown, структурированные логи (`slog`)
- Docker + docker-compose с healthcheckами, OpenAPI 3.0.3 + Swagger UI
- Unit-тесты + интеграционные тесты хранилища; CI (golangci-lint, `go test -race`
  с Postgres-сервис-контейнером, сборка образа)

## Быстрый старт (Docker)

```bash
cp .env.example .env    # свой ключ RATES_API_KEY
docker compose up -d --build
```

После запуска:

- API: http://localhost:8080
- Swagger UI: http://localhost:8080/swagger/
- OpenAPI-спека: http://localhost:8080/openapi.yaml
- Метрики: http://localhost:8080/metrics
- Версия: http://localhost:8080/version
- Smoke-тест: `./scripts/smoke.ps1`

Postgres проброшен на хост как `localhost:5433`, чтобы избежать конфликтов с локальной службой PostgresSQL

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
| `PROVIDER_RETRY_BASE_DELAY` | `500ms` | начальная задержка между попытками |
| `PROVIDER_RETRY_MAX_DELAY` | `10s` | максимальная задержка ретрая |
| `PROVIDER_MIN_INTERVAL` | `1s` | минимальный интервал между вызовами провайдера (глобально) |
| `PROVIDER_CACHE_TTL` | `60s` | TTL кэша ответов провайдера (`0` — выключить) |
| `SUPPORTED_CURRENCIES` | `USD,EUR,MXN` | список разрешенных валют |
| `WORKERS` | `4` | число фоновых воркеров |
| `QUEUE_SIZE` | `1024` | размер очереди обновлений |
| `RESYNC_INTERVAL` | `30s` | интервал добора зависших pending-запросов |
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
internal/metrics                — сбор и регистрация Prometheus метрик
internal/service                — бизнес-логика: очередь, пул воркеров, resync pending-запросов
internal/rates                  — интерфейс провайдера цен
internal/rates/frankfurter      — реализация основного провайдера
internal/rates/exchangeratesapi — реализация доп провайдера (кросс-курсы от базовой валюты)
internal/rates/cached           — декоратор TTL кэширования в памяти
internal/rates/resilient        — декоратор устойчивости (ретраи и rate-limiter)
internal/rates/fallback        — декоратор автоматического переключения на доп провайдера
internal/storage/postgres       — pgx-репозиторий + встроенные миграции
```

Поток обновления: `POST /quotes` → строка в `update_requests` (pending) →
очередь (канал) → воркер вызывает внешний API → upsert в `quotes` →
запрос переводится в `completed`/`failed`. При переполнении очереди или
рестарте pending-запросы переотправляются в очередь (`RESYNC_INTERVAL`,
а также при старте сервиса).

## Наблюдаемость

- `GET /healthz` — пинг БД (для healthcheckов compose/K8s)
- `GET /version` — версия сборки (задаётся ldflags при сборке образа)
- `GET /metrics` — счётчики в формате Prometheus: HTTP-запросы по route/коду,
  итоги обновлений (completed/failed), вызовы провайдера (ok/error), суммарная
  длительность вызовов провайдера, длина очереди
- `X-Request-ID` — эхо в каждом ответе и поле `request_id` в логах
  (принимается от клиента или генерируется)
- `/debug/pprof` — поднимается при `PPROF_ENABLED=1`

## Архитектурные решения и trade-offs

**Каналы вместо очереди в БД.** Для работы сервиса в один инстанс связка Go-канала и PostgreSQL дает минимальную задержку без усложнения инфраструктуры. Риск потери задач при аварийном рестарте закрыт (фоновый процесс (RESYNC_INTERVAL) периодически проверяет и добирает зависшие pending-задачи из БД).

**Кросс-курсы от анкора.** Бесплатный тариф exchangeratesapi.io отдает курсы только с базой EUR, поэтому цена произвольной пары считается из курсов против
базы ответа: `price(BASE/QUOTE) = R[QUOTE] / R[BASE]`. Формула не зависит от
того, какую базу вернул API, и покрыта тестами. Trade-off — лишнее деление
(погрешность незначима для отображения котировок). Именно поэтому дефолтным
провайдером стал **frankfurter.dev**: открытый API референсных курсов ЕЦБ
без ключа и агрессивных лимитов, а exchangeratesapi.io остался опцией —
он назван в ТЗ, и переключение делается переменной окружения.

**Идемпотентность на уровне БД.** Защита от дублей арантируется уникальным индексом по idempotency_key и частичным уникальным индексом на активные задачи WHERE status = 'pending'. Это исключает дублирование даже при одновременных параллельных запросах.

**422 вместо 400 для неподдерживаемой пары.** 400 — запрос некорректен по
форме (битый JSON, кривой формат пары); 422 — форма верна, но нарушено
бизнес-правило (валюта вне whitelist). Клиент может программно различить
«чинить формат» и «чинить список валют».

**Resilience-слой вокруг провайдера.** Внешние API котировок имеют лимиты и
WAF-защиту: обёртка `rates/resilient` добавляет глобальное ограничение частоты
(`PROVIDER_MIN_INTERVAL`), повторные попытки с экспоненциальным backoff и
джиттером для 429/403/5xx и уважение `Retry-After`. Запрос переводится в
`failed` только после исчерпания попыток — клиент видит причину.

**TTL-кэш ответов провайдера.** Повторные POST одной пары съедали квоту
внешнего API за те же данные: кэш в памяти (`PROVIDER_CACHE_TTL`, по умолчанию
60с) возвращает сохранённый ответ без вызова API — N запросов пары в пределах
TTL = 1 платный вызов. Ошибки не кэшируются; попадания/промахи видны в
`/metrics` (`rates_provider_cache_total`). На hit'е возвращается исходный
`FetchedAt` — время реального получения данных.

**Фолбэк между провайдерами (всегда включён).** Основной провайдер задаётся
`RATES_PROVIDER`; резервный — «другой», если применим (для exchangeratesapi
нужен непустой ключ). После исчерпания ретраев основного запрос уходит к
резервному — оба отдают курсы ЕЦБ, переключение семантически прозрачно и
видно в метриках (`rates_fallback_total`). При неправильно настроенном
основном сервис не падает, а продолжает отвечать через резервный, логируя
warning на каждый фолбэк. `RATES_API_URL` переопределяет URL только
основного провайдера.

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
