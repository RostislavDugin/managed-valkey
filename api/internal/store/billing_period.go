package store

import (
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

type BillingPeriod struct {
	ID                uuid.UUID                       `gorm:"column:id;type:uuid;default:uuidv7();primaryKey"`
	UserID            uuid.UUID                       `gorm:"column:user_id;type:uuid"`
	Service           domain.ManagedService           `gorm:"column:service"`
	ResourceID        uuid.UUID                       `gorm:"column:resource_id;type:uuid"`
	StartedAt         time.Time                       `gorm:"column:started_at"`
	EndedAt           *time.Time                      `gorm:"column:ended_at"`
	Mode              domain.ValkeyInstanceMode       `gorm:"column:mode"`
	VCPU              int                             `gorm:"column:vcpu"`
	RAMGB             int                             `gorm:"column:ram_gb"`
	ProcessCount      int                             `gorm:"column:node_count"`
	PriceCoinsPerHour int64                           `gorm:"column:price_coins_per_hour"`
	StartedReason     domain.BillingPeriodStartReason `gorm:"column:started_reason"`
	EndedReason       *domain.BillingPeriodEndReason  `gorm:"column:ended_reason"`
}

func (BillingPeriod) TableName() string { return "billing_periods" }
