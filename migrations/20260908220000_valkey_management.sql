-- +goose Up
CREATE TABLE valkey_instances (
    id UUID DEFAULT uuidv7() NOT NULL,
    user_id UUID NOT NULL,
    name TEXT NOT NULL,
    slug TEXT NOT NULL,
    mode TEXT NOT NULL,
    vcpu INTEGER NOT NULL,
    ram_gb INTEGER NOT NULL,
    desired_generation INTEGER DEFAULT 1 NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    configuration_requested_at TIMESTAMPTZ NOT NULL,
    app_password_hash TEXT NOT NULL,
    password_prefix TEXT NOT NULL,
    password_version INTEGER DEFAULT 1 NOT NULL,
    host TEXT NOT NULL,
    host_ro TEXT,
    port INTEGER NOT NULL,
    is_whitelist_enabled BOOLEAN DEFAULT FALSE NOT NULL,
    whitelist_cidrs TEXT[] DEFAULT '{}'::TEXT[] NOT NULL,
    maintenance_dow SMALLINT,
    maintenance_hour_utc SMALLINT,
    maintenance_duration_min SMALLINT,
    deletion_requested_at TIMESTAMPTZ,
    phase TEXT DEFAULT 'provisioning' NOT NULL,
    phase_reason TEXT,
    observed_generation INTEGER DEFAULT 0 NOT NULL,
    observed_at TIMESTAMPTZ,
    is_recovery_required BOOLEAN DEFAULT FALSE NOT NULL,
    network_verification_status TEXT DEFAULT 'pending' NOT NULL,
    network_verified_at TIMESTAMPTZ,
    applied_password_version INTEGER DEFAULT 0 NOT NULL,
    applied_vcpu INTEGER DEFAULT 0 NOT NULL,
    applied_ram_gb INTEGER DEFAULT 0 NOT NULL,
    deleted_at TIMESTAMPTZ,
    CONSTRAINT valkey_instances_pkey PRIMARY KEY (id),
    CONSTRAINT valkey_instances_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT,
    CONSTRAINT valkey_instances_name_format CHECK (name ~ '^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$'),
    CONSTRAINT valkey_instances_slug_format CHECK (slug ~ '^[a-z0-9](?:[a-z0-9-]{1,62}[a-z0-9])$'),
    CONSTRAINT valkey_instances_mode_valid CHECK (mode IN ('single', 'ha')),
    CONSTRAINT valkey_instances_size_valid CHECK (
        vcpu IN (1, 2, 4, 8, 16)
        AND ram_gb IN (1, 2, 4, 8, 16, 32, 64, 128)
        AND vcpu <= ram_gb
        AND ram_gb <= 16 * vcpu
    ),
    CONSTRAINT valkey_instances_desired_generation_positive CHECK (desired_generation >= 1),
    CONSTRAINT valkey_instances_password_hash_valid CHECK (app_password_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT valkey_instances_password_prefix_valid CHECK (password_prefix ~ '^[A-Za-z0-9_-]{4}$'),
    CONSTRAINT valkey_instances_password_version_positive CHECK (password_version >= 1),
    CONSTRAINT valkey_instances_port_valid CHECK (port BETWEEN 1 AND 65535),
    CONSTRAINT valkey_instances_maintenance_complete CHECK (
        (maintenance_dow IS NULL AND maintenance_hour_utc IS NULL AND maintenance_duration_min IS NULL)
        OR (
            maintenance_dow BETWEEN 0 AND 6
            AND maintenance_hour_utc BETWEEN 0 AND 23
            AND maintenance_duration_min BETWEEN 1 AND 1440
        )
    ),
    CONSTRAINT valkey_instances_phase_valid CHECK (
        phase IN ('provisioning', 'running', 'updating', 'degraded', 'unavailable', 'error')
    ),
    CONSTRAINT valkey_instances_observed_generation_valid CHECK (
        observed_generation >= 0 AND observed_generation <= desired_generation
    ),
    CONSTRAINT valkey_instances_network_verification_status_valid CHECK (
        network_verification_status IN ('pending', 'verified', 'unknown')
    ),
    CONSTRAINT valkey_instances_applied_password_version_valid CHECK (
        applied_password_version >= 0 AND applied_password_version <= password_version
    ),
    CONSTRAINT valkey_instances_applied_resources_valid CHECK (
        (applied_vcpu = 0 AND applied_ram_gb = 0)
        OR (applied_vcpu > 0 AND applied_ram_gb > 0)
    ),
    CONSTRAINT valkey_instances_deleted_after_request CHECK (deleted_at IS NULL OR deletion_requested_at IS NOT NULL)
);

CREATE UNIQUE INDEX valkey_instances_slug_key ON valkey_instances (slug);
CREATE UNIQUE INDEX valkey_instances_user_name_active_key
    ON valkey_instances (user_id, name)
    WHERE deleted_at IS NULL;
CREATE INDEX valkey_instances_user_created_id_idx
    ON valkey_instances (user_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;

CREATE TABLE billing_periods (
    id UUID DEFAULT uuidv7() NOT NULL,
    user_id UUID NOT NULL,
    service TEXT NOT NULL,
    resource_id UUID NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ,
    mode TEXT NOT NULL,
    vcpu INTEGER NOT NULL,
    ram_gb INTEGER NOT NULL,
    node_count INTEGER NOT NULL,
    price_coins_per_hour BIGINT NOT NULL,
    started_reason TEXT NOT NULL,
    ended_reason TEXT,
    CONSTRAINT billing_periods_pkey PRIMARY KEY (id),
    CONSTRAINT billing_periods_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT,
    CONSTRAINT billing_periods_service_valid CHECK (service = 'valkey'),
    CONSTRAINT billing_periods_mode_valid CHECK (mode IN ('single', 'ha')),
    CONSTRAINT billing_periods_resources_positive CHECK (vcpu > 0 AND ram_gb > 0),
    CONSTRAINT billing_periods_node_count_valid CHECK (node_count IN (1, 3)),
    CONSTRAINT billing_periods_price_nonnegative CHECK (price_coins_per_hour >= 0),
    CONSTRAINT billing_periods_started_reason_valid CHECK (started_reason IN ('created', 'resized')),
    CONSTRAINT billing_periods_ended_reason_valid CHECK (ended_reason IS NULL OR ended_reason IN ('resized', 'deleted')),
    CONSTRAINT billing_periods_end_complete CHECK ((ended_at IS NULL) = (ended_reason IS NULL)),
    CONSTRAINT billing_periods_end_not_before_start CHECK (ended_at IS NULL OR ended_at >= started_at)
);

CREATE UNIQUE INDEX billing_periods_open_resource_key
    ON billing_periods (service, resource_id)
    WHERE ended_at IS NULL;
CREATE INDEX billing_periods_resource_started_id_idx
    ON billing_periods (service, resource_id, started_at DESC, id DESC);

CREATE TABLE idempotency_keys (
    user_id UUID NOT NULL,
    key UUID NOT NULL,
    request_hash TEXT NOT NULL,
    response_status INTEGER NOT NULL,
    response_body JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT idempotency_keys_pkey PRIMARY KEY (user_id, key),
    CONSTRAINT idempotency_keys_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT,
    CONSTRAINT idempotency_keys_request_hash_valid CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT idempotency_keys_response_status_valid CHECK (response_status BETWEEN 200 AND 299)
);

CREATE INDEX idempotency_keys_created_at_idx ON idempotency_keys (created_at);

-- +goose Down
DROP TABLE idempotency_keys;
DROP TABLE billing_periods;
DROP TABLE valkey_instances;
