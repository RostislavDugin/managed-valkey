package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

func (s *Store) WriteAudit(ctx context.Context, tx *gorm.DB, event audit.Event) error {
	record := AuditLog{
		UserID: event.UserID, UserEmail: event.UserEmail, Action: event.Action,
		Service: event.Service, ResourceID: event.ResourceID, RequestID: event.RequestID, CreatedAt: event.CreatedAt,
	}
	if err := tx.WithContext(ctx).Create(&record).Error; err != nil {
		return fmt.Errorf("вставить audit_logs: %w", err)
	}

	return nil
}

type auditLogRow struct {
	ID        uuid.UUID         `gorm:"column:id"`
	Action    audit.EventAction `gorm:"column:action"`
	UserEmail string            `gorm:"column:user_email"`
	CreatedAt time.Time         `gorm:"column:created_at"`
}

func (s *Store) ListAudit(
	ctx context.Context,
	service domain.ManagedService,
	resourceID uuid.UUID,
	before *audit.Position,
	limit int,
) ([]audit.LogEntry, error) {
	query := s.db.WithContext(ctx).
		Model(&AuditLog{}).
		Select("id", "action", "user_email", "created_at").
		Where("service = ? AND resource_id = ?", service, resourceID)
	if before != nil {
		query = query.Where("(created_at, id) < (?, ?)", before.CreatedAt, before.ID)
	}

	var rows []auditLogRow
	if err := query.Order("created_at DESC, id DESC").Limit(limit).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("прочитать audit_logs: %w", err)
	}

	items := make([]audit.LogEntry, 0, len(rows))
	for _, row := range rows {
		items = append(items, audit.LogEntry{
			ID: row.ID, Action: row.Action, UserEmail: row.UserEmail, CreatedAt: row.CreatedAt,
		})
	}

	return items, nil
}
