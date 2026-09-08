-- +goose Up
CREATE TABLE users (
    id UUID DEFAULT uuidv7() NOT NULL,
    email TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    is_blocked BOOLEAN DEFAULT FALSE NOT NULL,
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT users_pkey PRIMARY KEY (id),
    CONSTRAINT users_email_key UNIQUE (email),
    CONSTRAINT users_email_normalized CHECK (email = lower(btrim(email)))
);

CREATE TABLE user_quotas (
    user_id UUID NOT NULL,
    max_vcpu INTEGER DEFAULT 4 NOT NULL,
    max_ram_gb INTEGER DEFAULT 16 NOT NULL,
    CONSTRAINT user_quotas_pkey PRIMARY KEY (user_id),
    CONSTRAINT user_quotas_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT user_quotas_max_vcpu_positive CHECK (max_vcpu > 0),
    CONSTRAINT user_quotas_max_ram_gb_positive CHECK (max_ram_gb > 0)
);

CREATE TABLE audit_logs (
    id UUID DEFAULT uuidv7() NOT NULL,
    user_id UUID NOT NULL,
    user_email TEXT NOT NULL,
    action TEXT NOT NULL,
    service TEXT,
    resource_id UUID,
    request_id TEXT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT audit_logs_pkey PRIMARY KEY (id),
    CONSTRAINT audit_logs_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE RESTRICT,
    CONSTRAINT audit_logs_action_not_empty CHECK (action <> ''),
    CONSTRAINT audit_logs_request_id_not_empty CHECK (request_id <> '')
);

CREATE INDEX audit_logs_resource_created_id_idx ON audit_logs (resource_id, created_at DESC, id DESC);
CREATE INDEX audit_logs_user_created_idx ON audit_logs (user_id, created_at DESC);

CREATE TABLE auth_registration_keys (
    key UUID NOT NULL,
    user_id UUID NOT NULL,
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT auth_registration_keys_pkey PRIMARY KEY (key),
    CONSTRAINT auth_registration_keys_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
);

CREATE INDEX auth_registration_keys_created_at_idx ON auth_registration_keys (created_at);

-- +goose Down
DROP TABLE auth_registration_keys;
DROP TABLE audit_logs;
DROP TABLE user_quotas;
DROP TABLE users;
