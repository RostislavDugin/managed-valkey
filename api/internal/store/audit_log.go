package store

import (
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

type AuditLog struct {
	ID         uuid.UUID              `gorm:"column:id;type:uuid;default:uuidv7();primaryKey"`
	UserID     uuid.UUID              `gorm:"column:user_id;type:uuid"`
	UserEmail  string                 `gorm:"column:user_email"`
	Action     audit.EventAction      `gorm:"column:action"`
	Service    *domain.ManagedService `gorm:"column:service"`
	ResourceID *uuid.UUID             `gorm:"column:resource_id;type:uuid"`
	RequestID  string                 `gorm:"column:request_id"`
	CreatedAt  time.Time              `gorm:"column:created_at"`
}

func (AuditLog) TableName() string { return "audit_logs" }
