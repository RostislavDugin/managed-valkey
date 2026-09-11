-- +goose Up
UPDATE user_quotas
SET max_vcpu = 4,
    max_ram_gb = 12;

ALTER TABLE user_quotas
    ALTER COLUMN max_vcpu SET DEFAULT 4,
    ALTER COLUMN max_ram_gb SET DEFAULT 12;

-- +goose Down
UPDATE user_quotas
SET max_ram_gb = 16
WHERE max_vcpu = 4
  AND max_ram_gb = 12;

ALTER TABLE user_quotas
    ALTER COLUMN max_vcpu SET DEFAULT 4,
    ALTER COLUMN max_ram_gb SET DEFAULT 16;
