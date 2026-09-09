package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

const maxValkeyMetricBatchSize = 3

var (
	ErrValkeyMetricBatchTooLarge = errors.New("пакет метрик Valkey содержит больше трёх строк")
	ErrValkeyMetricsRetention    = errors.New("срок хранения метрик Valkey должен быть положительным")
)

type ValkeyMetricBucket struct {
	Ordinal          int                   `gorm:"column:ordinal"`
	Role             domain.ValkeyNodeRole `gorm:"column:role"`
	CollectedAt      time.Time             `gorm:"column:collected_at"`
	UsedMemoryBytes  *float64              `gorm:"column:used_memory_bytes"`
	CPUMillicores    *float64              `gorm:"column:cpu_millicores"`
	ConnectedClients *float64              `gorm:"column:connected_clients"`
	OpsPerSec        *float64              `gorm:"column:ops_per_sec"`
	KeyspaceHits     *int64                `gorm:"column:keyspace_hits"`
	KeyspaceMisses   *int64                `gorm:"column:keyspace_misses"`
	EvictedKeys      *int64                `gorm:"column:evicted_keys"`
}

func (s *Store) ImportValkeyNodeMetrics(
	ctx context.Context,
	instanceID uuid.UUID,
	metrics []ValkeyNodeMetric,
	retention time.Duration,
) error {
	if len(metrics) > maxValkeyMetricBatchSize {
		return ErrValkeyMetricBatchTooLarge
	}
	if retention <= 0 {
		return ErrValkeyMetricsRetention
	}
	if len(metrics) == 0 {
		return nil
	}

	return s.WithinTransaction(ctx, func(tx *gorm.DB) error {
		instance, err := findValkeyInstanceForSyncUpdate(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if instance.DeletionRequestedAt != nil {
			return nil
		}

		now, err := s.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		cutoff := now.Add(-retention)
		accepted := make([]ValkeyNodeMetric, 0, len(metrics))
		for _, metric := range metrics {
			if metric.TS.Before(cutoff) {
				continue
			}

			metric.InstanceID = instanceID
			metric.TS = metric.TS.UTC()
			accepted = append(accepted, metric)
		}
		if len(accepted) == 0 {
			return nil
		}

		result := tx.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "instance_id"},
				{Name: "ordinal"},
				{Name: "ts"},
			},
			DoNothing: true,
		}).Create(&accepted)
		if result.Error != nil {
			return fmt.Errorf("сохранить метрики Valkey: %w", result.Error)
		}

		return nil
	})
}

func (s *Store) DeleteExpiredValkeyNodeMetrics(ctx context.Context, retention time.Duration) error {
	if retention <= 0 {
		return ErrValkeyMetricsRetention
	}

	return s.WithinTransaction(ctx, func(tx *gorm.DB) error {
		now, err := s.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}

		_, err = s.DeleteValkeyNodeMetricsBefore(ctx, tx, now.Add(-retention))
		return err
	})
}

func (s *Store) DeleteValkeyNodeMetricsBefore(
	ctx context.Context,
	tx *gorm.DB,
	cutoff time.Time,
) (int64, error) {
	result := tx.WithContext(ctx).Where("ts < ?", cutoff.UTC()).Delete(&ValkeyNodeMetric{})
	if result.Error != nil {
		return 0, fmt.Errorf("удалить просроченные метрики Valkey: %w", result.Error)
	}

	return result.RowsAffected, nil
}

func (s *Store) ReadValkeyMetricBuckets(
	ctx context.Context,
	instanceID uuid.UUID,
	from time.Time,
	to time.Time,
	step time.Duration,
) ([]ValkeyMetricBucket, error) {
	const query = `
		WITH selected AS (
			SELECT
				ordinal,
				ts,
				role,
				run_id,
				used_memory_bytes,
				connected_clients,
				ops_per_sec,
				keyspace_hits,
				keyspace_misses,
				evicted_keys,
				cpu_millicores
			FROM valkey_node_metrics
			WHERE instance_id = @instance_id
			  AND ts >= @from
			  AND ts < @to
			  AND ts >= CURRENT_TIMESTAMP - INTERVAL '7 days'
		),
		nodes AS (
			SELECT DISTINCT ordinal
			FROM selected
		),
		previous AS (
			SELECT DISTINCT ON (metric.ordinal)
				metric.ordinal,
				metric.ts,
				metric.role,
				metric.run_id,
				metric.used_memory_bytes,
				metric.connected_clients,
				metric.ops_per_sec,
				metric.keyspace_hits,
				metric.keyspace_misses,
				metric.evicted_keys,
				metric.cpu_millicores
			FROM valkey_node_metrics metric
			JOIN nodes ON nodes.ordinal = metric.ordinal
			WHERE metric.instance_id = @instance_id
			  AND metric.ts < @from
			  AND metric.ts >= CURRENT_TIMESTAMP - INTERVAL '7 days'
			ORDER BY metric.ordinal, metric.ts DESC
		),
		samples AS (
			SELECT * FROM previous
			UNION ALL
			SELECT * FROM selected
		),
		ordered AS (
			SELECT
				*,
				LAG(run_id) OVER node_order AS previous_run_id,
				LAG(keyspace_hits) OVER node_order AS previous_keyspace_hits,
				LAG(keyspace_misses) OVER node_order AS previous_keyspace_misses,
				LAG(evicted_keys) OVER node_order AS previous_evicted_keys
			FROM samples
			WINDOW node_order AS (PARTITION BY ordinal ORDER BY ts)
		),
		deltas AS (
			SELECT
				*,
				CASE
					WHEN previous_run_id = run_id
						THEN GREATEST(keyspace_hits - previous_keyspace_hits, 0)
					ELSE keyspace_hits
				END AS keyspace_hits_delta,
				CASE
					WHEN previous_run_id = run_id
						THEN GREATEST(keyspace_misses - previous_keyspace_misses, 0)
					ELSE keyspace_misses
				END AS keyspace_misses_delta,
				CASE
					WHEN previous_run_id = run_id
						THEN GREATEST(evicted_keys - previous_evicted_keys, 0)
					ELSE evicted_keys
				END AS evicted_keys_delta
			FROM ordered
			WHERE ts >= @from
		),
		bucketed AS (
			SELECT
				ordinal,
				date_bin(make_interval(secs => @step_seconds), ts, CAST(@from AS TIMESTAMPTZ)) AS collected_at,
				AVG(used_memory_bytes)::DOUBLE PRECISION AS used_memory_bytes,
				AVG(cpu_millicores)::DOUBLE PRECISION AS cpu_millicores,
				AVG(connected_clients)::DOUBLE PRECISION AS connected_clients,
				AVG(ops_per_sec)::DOUBLE PRECISION AS ops_per_sec,
				SUM(keyspace_hits_delta)::BIGINT AS keyspace_hits,
				SUM(keyspace_misses_delta)::BIGINT AS keyspace_misses,
				SUM(evicted_keys_delta)::BIGINT AS evicted_keys
			FROM deltas
			GROUP BY ordinal, collected_at
		),
		roles AS (
			SELECT DISTINCT ON (ordinal) ordinal, role
			FROM selected
			ORDER BY ordinal, ts DESC
		),
		series AS (
			SELECT
				nodes.ordinal,
				points.collected_at
			FROM nodes
			CROSS JOIN LATERAL generate_series(
				CAST(@from AS TIMESTAMPTZ),
				CAST(@to AS TIMESTAMPTZ) - INTERVAL '1 microsecond',
				make_interval(secs => @step_seconds)
			) AS points(collected_at)
		)
		SELECT
			series.ordinal,
			roles.role,
			series.collected_at,
			bucketed.used_memory_bytes,
			bucketed.cpu_millicores,
			bucketed.connected_clients,
			bucketed.ops_per_sec,
			bucketed.keyspace_hits,
			bucketed.keyspace_misses,
			bucketed.evicted_keys
		FROM series
		JOIN roles USING (ordinal)
		LEFT JOIN bucketed USING (ordinal, collected_at)
		ORDER BY series.ordinal, series.collected_at`

	arguments := []any{
		sql.Named("instance_id", instanceID),
		sql.Named("from", from.UTC()),
		sql.Named("to", to.UTC()),
		sql.Named("step_seconds", int64(step/time.Second)),
	}

	var buckets []ValkeyMetricBucket
	if err := s.db.WithContext(ctx).Raw(query, arguments...).Scan(&buckets).Error; err != nil {
		return nil, fmt.Errorf("прочитать метрики Valkey: %w", err)
	}

	return buckets, nil
}
