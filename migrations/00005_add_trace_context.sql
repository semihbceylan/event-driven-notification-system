-- +goose Up
-- W3C traceparent of the API request that created the notification. The
-- worker continues that trace, so one trace covers the request and every
-- delivery attempt even though they run in different processes.
ALTER TABLE notifications ADD COLUMN trace_parent TEXT;

-- +goose Down
ALTER TABLE notifications DROP COLUMN trace_parent;
