package store

import (
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

type ValkeyInstancePhaseEvent struct {
	ID         uuid.UUID                   `gorm:"column:id;type:uuid;default:uuidv7();primaryKey"`
	InstanceID uuid.UUID                   `gorm:"column:instance_id;type:uuid"`
	FromPhase  domain.ValkeyInstancePhase  `gorm:"column:from_phase"`
	ToPhase    domain.ValkeyInstanceStatus `gorm:"column:to_phase"`
	Reason     *string                     `gorm:"column:reason"`
	CreatedAt  time.Time                   `gorm:"column:created_at"`
}

func (ValkeyInstancePhaseEvent) TableName() string { return "valkey_instance_phase_events" }
