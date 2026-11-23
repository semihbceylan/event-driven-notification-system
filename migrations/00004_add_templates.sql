-- +goose Up
-- Reusable message templates with {{variable}} placeholders.
CREATE TABLE templates (
    name       TEXT        PRIMARY KEY,
    channel    TEXT        NOT NULL CHECK (channel IN ('sms', 'email', 'push')),
    content    TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Which template a notification was rendered from. The rendered text is
-- stored in content, so editing or deleting a template never changes a
-- notification that was already created. This column is for reporting only,
-- hence no foreign key.
ALTER TABLE notifications ADD COLUMN template_name TEXT;

-- +goose Down
ALTER TABLE notifications DROP COLUMN template_name;
DROP TABLE templates;
