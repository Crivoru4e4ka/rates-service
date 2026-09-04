-- Котировки: последнее известное значение по каждой паре.
CREATE TABLE IF NOT EXISTS quotes (
    pair       TEXT PRIMARY KEY,
    price      NUMERIC(20,10) NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Запросы на фоновое обновление котировок.
CREATE TABLE IF NOT EXISTS update_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pair            TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'completed', 'failed')),
    price           NUMERIC(20,10),
    error           TEXT,
    idempotency_key TEXT UNIQUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Идемпотентность: у пары не может быть больше одного незавершённого запроса.
CREATE UNIQUE INDEX IF NOT EXISTS uq_update_requests_pending_pair
    ON update_requests (pair) WHERE status = 'pending';
