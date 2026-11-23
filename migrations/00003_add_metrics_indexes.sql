-- +goose Up
-- Indexes for the /v1/metrics queries, which look at a recent time window.
-- Without them each query would scan every notification ever sent.

-- Provider latency and attempt success rate over the last N minutes.
CREATE INDEX delivery_attempts_started_at_idx
    ON delivery_attempts (started_at);

-- Sent count and delivery latency over the last N minutes.
CREATE INDEX notifications_sent_at_idx
    ON notifications (sent_at)
    WHERE status = 'sent';

-- Notifications that gave up in the last N minutes.
CREATE INDEX notifications_failed_updated_at_idx
    ON notifications (updated_at)
    WHERE status = 'failed';

-- +goose Down
DROP INDEX notifications_failed_updated_at_idx;
DROP INDEX notifications_sent_at_idx;
DROP INDEX delivery_attempts_started_at_idx;
