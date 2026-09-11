-- +goose Up
ALTER TABLE valkey_instances DROP COLUMN IF EXISTS prefix;

-- +goose Down
SELECT 1;
