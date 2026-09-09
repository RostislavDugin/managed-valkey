-- +goose Up
CREATE TABLE valkey_node_metrics (
    instance_id UUID NOT NULL,
    ordinal INTEGER NOT NULL,
    ts TIMESTAMPTZ NOT NULL,
    role TEXT NOT NULL,
    run_id TEXT NOT NULL,
    used_memory_bytes BIGINT NOT NULL,
    maxmemory_bytes BIGINT NOT NULL,
    connected_clients BIGINT NOT NULL,
    ops_per_sec BIGINT NOT NULL,
    keyspace_hits BIGINT NOT NULL,
    keyspace_misses BIGINT NOT NULL,
    evicted_keys BIGINT NOT NULL,
    cpu_millicores BIGINT,
    CONSTRAINT valkey_node_metrics_ordinal_nonnegative CHECK (ordinal >= 0),
    CONSTRAINT valkey_node_metrics_role_valid CHECK (role IN ('primary', 'replica', 'unknown')),
    CONSTRAINT valkey_node_metrics_run_id_not_empty CHECK (run_id <> ''),
    CONSTRAINT valkey_node_metrics_values_nonnegative CHECK (
        used_memory_bytes >= 0
        AND maxmemory_bytes >= 0
        AND connected_clients >= 0
        AND ops_per_sec >= 0
        AND keyspace_hits >= 0
        AND keyspace_misses >= 0
        AND evicted_keys >= 0
        AND (cpu_millicores IS NULL OR cpu_millicores >= 0)
    )
);

ALTER TABLE valkey_node_metrics
    ADD CONSTRAINT valkey_node_metrics_instance_id_fkey
    FOREIGN KEY (instance_id) REFERENCES valkey_instances (id) ON DELETE RESTRICT;

CREATE UNIQUE INDEX valkey_node_metrics_instance_ordinal_ts_key
    ON valkey_node_metrics (instance_id, ordinal, ts);
CREATE INDEX valkey_node_metrics_instance_ts_idx
    ON valkey_node_metrics (instance_id, ts DESC);

-- +goose Down
DROP TABLE valkey_node_metrics;
