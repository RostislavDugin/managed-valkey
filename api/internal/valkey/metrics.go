package valkey

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

const (
	MetricRange5Minutes MetricRange = "5m"
	MetricRange1Hour    MetricRange = "1h"
	MetricRange24Hours  MetricRange = "24h"
	MetricRange7Days    MetricRange = "7d"
	MetricHistoryLimit              = 7 * 24 * time.Hour
)

type MetricRange string

type MetricsInput struct {
	Range *string
	From  *string
	To    *string
}

type MetricWindow struct {
	From time.Time
	To   time.Time
	Step time.Duration
}

type MetricPoint struct {
	CollectedAt      time.Time `json:"collected_at"`
	UsedMemoryBytes  *float64  `json:"used_memory_bytes"`
	CPUMillicores    *float64  `json:"cpu_millicores"`
	ConnectedClients *float64  `json:"connected_clients"`
	OpsPerSec        *float64  `json:"ops_per_sec"`
	KeyspaceHits     *int64    `json:"keyspace_hits"`
	KeyspaceMisses   *int64    `json:"keyspace_misses"`
	EvictedKeys      *int64    `json:"evicted_keys"`
}

type MetricNode struct {
	Ordinal int                   `json:"ordinal"`
	Name    string                `json:"name"`
	Role    domain.ValkeyNodeRole `json:"role"`
	Points  []MetricPoint         `json:"points"`
}

type Metrics struct {
	From        time.Time    `json:"from"`
	To          time.Time    `json:"to"`
	StepSeconds int64        `json:"step_seconds"`
	Nodes       []MetricNode `json:"nodes"`
}

func ParseMetricWindow(input MetricsInput, now time.Time) (MetricWindow, error) {
	if input.Range != nil && (input.From != nil || input.To != nil) {
		return MetricWindow{}, validation(map[string]string{"range": "conflicts_with_from_to"})
	}
	if (input.From == nil) != (input.To == nil) {
		return MetricWindow{}, validation(map[string]string{"from": "required_with_to", "to": "required_with_from"})
	}
	if input.From != nil {
		return parseCustomMetricWindow(*input.From, *input.To)
	}

	rangeValue := string(MetricRange1Hour)
	if input.Range != nil {
		rangeValue = *input.Range
	}
	duration, step, ok := metricRangeConfig(MetricRange(rangeValue))
	if !ok {
		return MetricWindow{}, validation(map[string]string{"range": "invalid_choice"})
	}

	to := now.UTC().Truncate(step).Add(step)

	return MetricWindow{From: to.Add(-duration), To: to, Step: step}, nil
}

func parseCustomMetricWindow(fromValue, toValue string) (MetricWindow, error) {
	from, fromErr := time.Parse(time.RFC3339Nano, fromValue)
	to, toErr := time.Parse(time.RFC3339Nano, toValue)
	fields := make(map[string]string, 2)
	if fromErr != nil {
		fields["from"] = "invalid_timestamp"
	}
	if toErr != nil {
		fields["to"] = "invalid_timestamp"
	}
	if len(fields) > 0 {
		return MetricWindow{}, validation(fields)
	}

	from = from.UTC()
	to = to.UTC()
	duration := to.Sub(from)
	if duration <= 0 {
		return MetricWindow{}, validation(map[string]string{"from": "not_before_to"})
	}
	if duration > MetricHistoryLimit {
		return MetricWindow{}, validation(map[string]string{"from": "range_too_wide", "to": "range_too_wide"})
	}

	return MetricWindow{From: from, To: to, Step: metricStep(duration)}, nil
}

func metricRangeConfig(metricRange MetricRange) (time.Duration, time.Duration, bool) {
	switch metricRange {
	case MetricRange5Minutes:
		return 5 * time.Minute, 10 * time.Second, true
	case MetricRange1Hour:
		return time.Hour, time.Minute, true
	case MetricRange24Hours:
		return 24 * time.Hour, 5 * time.Minute, true
	case MetricRange7Days:
		return MetricHistoryLimit, 30 * time.Minute, true
	default:
		return 0, 0, false
	}
}

func metricStep(duration time.Duration) time.Duration {
	switch {
	case duration <= 5*time.Minute:
		return 10 * time.Second
	case duration <= time.Hour:
		return time.Minute
	case duration <= 24*time.Hour:
		return 5 * time.Minute
	default:
		return 30 * time.Minute
	}
}

func (s *Service) Metrics(
	ctx context.Context,
	actor Actor,
	instanceID uuid.UUID,
	input MetricsInput,
) (Metrics, error) {
	window, err := ParseMetricWindow(input, s.clock.Now().UTC())
	if err != nil {
		return Metrics{}, err
	}

	record, err := s.repository.FindActiveValkeyInstance(ctx, actor.ID, instanceID)
	if err != nil {
		return Metrics{}, publicStoreError(err)
	}

	response := Metrics{
		From: window.From, To: window.To, StepSeconds: int64(window.Step / time.Second), Nodes: []MetricNode{},
	}

	buckets, err := s.repository.ReadValkeyMetricBuckets(
		ctx,
		instanceID,
		window.From,
		window.To,
		window.Step,
	)
	if err != nil {
		return Metrics{}, apierr.WrapInternal(err)
	}

	for _, bucket := range buckets {
		if len(response.Nodes) == 0 || response.Nodes[len(response.Nodes)-1].Ordinal != bucket.Ordinal {
			response.Nodes = append(response.Nodes, MetricNode{
				Ordinal: bucket.Ordinal,
				Name:    fmt.Sprintf("%s-%d", record.Slug, bucket.Ordinal),
				Role:    bucket.Role,
				Points:  []MetricPoint{},
			})
		}

		node := &response.Nodes[len(response.Nodes)-1]
		node.Points = append(node.Points, MetricPoint{
			CollectedAt: bucket.CollectedAt.UTC(), UsedMemoryBytes: bucket.UsedMemoryBytes,
			CPUMillicores: bucket.CPUMillicores, ConnectedClients: bucket.ConnectedClients,
			OpsPerSec: bucket.OpsPerSec, KeyspaceHits: bucket.KeyspaceHits,
			KeyspaceMisses: bucket.KeyspaceMisses, EvictedKeys: bucket.EvictedKeys,
		})
	}

	return response, nil
}
