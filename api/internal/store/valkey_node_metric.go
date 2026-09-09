package store

import (
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

type ValkeyNodeMetric struct {
	InstanceID       uuid.UUID             `gorm:"column:instance_id;type:uuid"`
	Ordinal          int                   `gorm:"column:ordinal"`
	TS               time.Time             `gorm:"column:ts"`
	Role             domain.ValkeyNodeRole `gorm:"column:role"`
	RunID            string                `gorm:"column:run_id"`
	UsedMemoryBytes  int64                 `gorm:"column:used_memory_bytes"`
	MaxmemoryBytes   int64                 `gorm:"column:maxmemory_bytes"`
	ConnectedClients int64                 `gorm:"column:connected_clients"`
	OpsPerSec        int64                 `gorm:"column:ops_per_sec"`
	KeyspaceHits     int64                 `gorm:"column:keyspace_hits"`
	KeyspaceMisses   int64                 `gorm:"column:keyspace_misses"`
	EvictedKeys      int64                 `gorm:"column:evicted_keys"`
	CPUMillicores    *int64                `gorm:"column:cpu_millicores"`
}

func (ValkeyNodeMetric) TableName() string { return "valkey_node_metrics" }
