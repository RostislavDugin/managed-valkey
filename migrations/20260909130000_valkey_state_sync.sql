-- +goose Up
ALTER TABLE valkey_instances
    ADD COLUMN kubernetes_namespace_uid TEXT,
    ADD COLUMN kubernetes_cr_uid TEXT,
    ADD COLUMN deletion_stage TEXT,
    ADD COLUMN sync_recovery_reason TEXT,
    ADD COLUMN is_operator_recovery_required BOOLEAN DEFAULT FALSE NOT NULL,
    ADD COLUMN operator_recovery_reason TEXT;

ALTER TABLE valkey_instances
    ADD CONSTRAINT valkey_instances_deletion_stage_valid CHECK (
        deletion_stage IS NULL OR deletion_stage IN ('cr_delete_prepared', 'namespace_delete_prepared')
    ),
    ADD CONSTRAINT valkey_instances_deletion_stage_after_request CHECK (
        deletion_stage IS NULL OR deletion_requested_at IS NOT NULL
    ),
    ADD CONSTRAINT valkey_instances_operator_recovery_reason_valid CHECK (
        is_operator_recovery_required OR operator_recovery_reason IS NULL
    );

UPDATE valkey_instances
SET sync_recovery_reason = 'legacy_recovery_required'
WHERE is_recovery_required;

CREATE TABLE valkey_instance_nodes (
    instance_id UUID NOT NULL,
    ordinal INTEGER NOT NULL,
    role TEXT NOT NULL,
    pod_name TEXT NOT NULL,
    pod_uid TEXT NOT NULL,
    container_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    node_name TEXT NOT NULL,
    node_uid TEXT NOT NULL,
    is_ready BOOLEAN NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT valkey_instance_nodes_pkey PRIMARY KEY (instance_id, ordinal),
    CONSTRAINT valkey_instance_nodes_ordinal_nonnegative CHECK (ordinal >= 0),
    CONSTRAINT valkey_instance_nodes_role_valid CHECK (role IN ('primary', 'replica', 'unknown'))
);

CREATE TABLE valkey_instance_phase_events (
    id UUID DEFAULT uuidv7() NOT NULL,
    instance_id UUID NOT NULL,
    from_phase TEXT NOT NULL,
    to_phase TEXT NOT NULL,
    reason TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT valkey_instance_phase_events_pkey PRIMARY KEY (id),
    CONSTRAINT valkey_instance_phase_events_from_phase_valid CHECK (
        from_phase IN ('provisioning', 'running', 'updating', 'degraded', 'unavailable', 'error')
    ),
    CONSTRAINT valkey_instance_phase_events_to_phase_valid CHECK (
        to_phase IN ('provisioning', 'running', 'updating', 'degraded', 'unavailable', 'error', 'deleted')
    )
);

ALTER TABLE valkey_instance_nodes
    ADD CONSTRAINT valkey_instance_nodes_instance_id_fkey FOREIGN KEY (instance_id)
        REFERENCES valkey_instances (id) ON DELETE CASCADE;

ALTER TABLE valkey_instance_phase_events
    ADD CONSTRAINT valkey_instance_phase_events_instance_id_fkey FOREIGN KEY (instance_id)
        REFERENCES valkey_instances (id) ON DELETE CASCADE;

CREATE INDEX valkey_instance_nodes_observed_at_idx
    ON valkey_instance_nodes (observed_at);

CREATE INDEX valkey_instance_phase_events_instance_created_id_idx
    ON valkey_instance_phase_events (instance_id, created_at DESC, id DESC);

-- +goose Down
DROP TABLE valkey_instance_phase_events;
DROP TABLE valkey_instance_nodes;

ALTER TABLE valkey_instances
    DROP CONSTRAINT valkey_instances_operator_recovery_reason_valid,
    DROP CONSTRAINT valkey_instances_deletion_stage_after_request,
    DROP CONSTRAINT valkey_instances_deletion_stage_valid,
    DROP COLUMN operator_recovery_reason,
    DROP COLUMN is_operator_recovery_required,
    DROP COLUMN sync_recovery_reason,
    DROP COLUMN deletion_stage,
    DROP COLUMN kubernetes_cr_uid,
    DROP COLUMN kubernetes_namespace_uid;
