-- +goose Up
-- The notifications table is both the source of truth and the work queue.
-- Workers claim rows with SELECT ... FOR UPDATE SKIP LOCKED, so several
-- workers can pull from it concurrently without picking the same row.
CREATE TABLE notifications (
    id                  UUID PRIMARY KEY,
    batch_id            UUID,
    idempotency_key     TEXT,
    correlation_id      TEXT,

    channel             TEXT        NOT NULL CHECK (channel IN ('sms', 'email', 'push')),
    recipient           TEXT        NOT NULL,
    content             TEXT        NOT NULL,
    -- 0 = high, 1 = normal, 2 = low. Stored as a number so ORDER BY priority
    -- gives the right processing order.
    priority            SMALLINT    NOT NULL DEFAULT 1 CHECK (priority BETWEEN 0 AND 2),

    status              TEXT        NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'processing', 'sent', 'failed', 'cancelled')),
    attempts            INT         NOT NULL DEFAULT 0,
    max_attempts        INT         NOT NULL DEFAULT 5,
    -- When the row becomes eligible for processing. Used for scheduled sends
    -- (set to the scheduled time) and for retry backoff (pushed forward).
    next_attempt_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Set when a worker claims the row. If the worker crashes, a reaper puts
    -- rows whose lock has expired back to 'pending'.
    locked_until        TIMESTAMPTZ,

    provider_message_id TEXT,
    last_error          TEXT,

    scheduled_at        TIMESTAMPTZ,
    sent_at             TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Prevents duplicate sends when a client retries the same request.
CREATE UNIQUE INDEX notifications_idempotency_key_uidx
    ON notifications (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- The worker's claim query: "next pending rows for channel X, highest priority
-- first, that are due". A partial index keeps it small, because finished rows
-- (the vast majority over time) are not in it.
CREATE INDEX notifications_claim_idx
    ON notifications (channel, priority, next_attempt_at)
    WHERE status = 'pending';

-- Finds stuck rows whose worker died.
CREATE INDEX notifications_stuck_idx
    ON notifications (locked_until)
    WHERE status = 'processing';

CREATE INDEX notifications_batch_id_idx
    ON notifications (batch_id)
    WHERE batch_id IS NOT NULL;

-- List endpoint: newest first, with keyset pagination on (created_at, id).
CREATE INDEX notifications_created_at_idx
    ON notifications (created_at DESC, id DESC);

-- One row per delivery try, so every attempt is auditable.
CREATE TABLE delivery_attempts (
    id              BIGSERIAL   PRIMARY KEY,
    notification_id UUID        NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    attempt_number  INT         NOT NULL,
    status_code     INT,
    error           TEXT,
    duration_ms     INT         NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX delivery_attempts_notification_id_idx
    ON delivery_attempts (notification_id);

-- +goose Down
DROP TABLE delivery_attempts;
DROP TABLE notifications;
