package store

import (
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

type ValkeyInstanceNode struct {
	InstanceID  uuid.UUID             `gorm:"column:instance_id;type:uuid;primaryKey"`
	Ordinal     int                   `gorm:"column:ordinal;primaryKey"`
	Role        domain.ValkeyNodeRole `gorm:"column:role"`
	PodName     string                `gorm:"column:pod_name"`
	PodUID      string                `gorm:"column:pod_uid"`
	ContainerID string                `gorm:"column:container_id"`
	RunID       string                `gorm:"column:run_id"`
	NodeName    string                `gorm:"column:node_name"`
	NodeUID     string                `gorm:"column:node_uid"`
	IsReady     bool                  `gorm:"column:is_ready"`
	ObservedAt  time.Time             `gorm:"column:observed_at"`
}

func (ValkeyInstanceNode) TableName() string { return "valkey_instance_nodes" }
