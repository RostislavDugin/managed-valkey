-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM valkey_instances
        WHERE host_ro IS NULL
          AND host NOT LIKE slug || '.%'
    ) THEN
        RAISE EXCEPTION 'cannot derive host_ro from stored slug and host';
    END IF;
END;
$$;
-- +goose StatementEnd

UPDATE valkey_instances
SET host_ro = slug || '-ro' || substring(host FROM char_length(slug) + 1)
WHERE host_ro IS NULL;

ALTER TABLE valkey_instances
    ALTER COLUMN host_ro SET NOT NULL,
    ADD CONSTRAINT valkey_instances_host_ro_not_empty CHECK (host_ro <> '');

-- +goose Down
ALTER TABLE valkey_instances
    DROP CONSTRAINT valkey_instances_host_ro_not_empty,
    ALTER COLUMN host_ro DROP NOT NULL;

UPDATE valkey_instances
SET host_ro = NULL
WHERE mode = 'single';
