package store

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
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
