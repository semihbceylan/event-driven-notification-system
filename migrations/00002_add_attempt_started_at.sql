-- +goose Up
-- created_at is when the result was saved, after the provider call returned.
-- started_at is when the request was sent, which is what the per-channel
-- rate limit applies to and what latency should be measured from.
ALTER TABLE delivery_attempts
    ADD COLUMN started_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Rows from before this migration only have the save time; use it as the
-- closest estimate.
UPDATE delivery_attempts SET started_at = created_at;

-- +goose Down
ALTER TABLE delivery_attempts DROP COLUMN started_at;
