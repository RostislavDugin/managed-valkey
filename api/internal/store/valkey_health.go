package store

import (
	"context"
	"fmt"
)

func (s *Store) ListValkeyInstancesForHealth(ctx context.Context) ([]ValkeyInstance, error) {
	var instances []ValkeyInstance
	if err := s.db.WithContext(ctx).
		Select(
			"id",
			"slug",
			"desired_generation",
			"configuration_requested_at",
			"password_version",
			"deletion_requested_at",
			"phase",
			"phase_reason",
			"observed_generation",
			"observed_at",
			"is_recovery_required",
			"sync_recovery_reason",
			"is_operator_recovery_required",
			"operator_recovery_reason",
			"applied_password_version",
		).
		Where("deleted_at IS NULL").
		Order("slug, id").
		Find(&instances).Error; err != nil {
		return nil, fmt.Errorf("прочитать состояние инстансов Valkey: %w", err)
	}

	return instances, nil
}
