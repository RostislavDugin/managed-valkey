package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

var (
	ErrValkeyNameConflict  = errors.New("имя инстанса уже занято")
	ErrValkeySlugConflict  = errors.New("slug инстанса уже занят")
	ErrIdempotencyConflict = errors.New("ключ идемпотентности уже занят")
	ErrAdvisoryLockTimeout = errors.New("истёк срок ожидания общей блокировки")
)

type ResourceUsage struct {
	VCPU      int `gorm:"column:vcpu"`
	RAMGB     int `gorm:"column:ram_gb"`
	Instances int
}

type QuotaUsage struct {
	MaxVCPU   int `gorm:"column:max_vcpu"`
	MaxRAMGB  int `gorm:"column:max_ram_gb"`
	UsedVCPU  int `gorm:"column:used_vcpu"`
	UsedRAMGB int `gorm:"column:used_ram_gb"`
}

type ValkeyCapacityUsage struct {
	MaxVCPU          int `gorm:"column:max_vcpu"`
	MaxRAMGB         int `gorm:"column:max_ram_gb"`
	UserUsedVCPU     int `gorm:"column:user_used_vcpu"`
	UserUsedRAMGB    int `gorm:"column:user_used_ram_gb"`
	ClusterUsedVCPU  int `gorm:"column:cluster_used_vcpu"`
	ClusterUsedRAMGB int `gorm:"column:cluster_used_ram_gb"`
	Instances        int `gorm:"column:instances"`
}

func (s *Store) ListActiveValkeyInstances(ctx context.Context, userID uuid.UUID) ([]ValkeyInstance, error) {
	var instances []ValkeyInstance
	if err := s.db.WithContext(ctx).
		Where("user_id = ? AND deleted_at IS NULL", userID).
		Order("created_at DESC, id DESC").
		Find(&instances).Error; err != nil {
		return nil, fmt.Errorf("прочитать инстансы Valkey: %w", err)
	}

	return instances, nil
}

func (s *Store) FindActiveValkeyInstance(
	ctx context.Context,
	userID uuid.UUID,
	instanceID uuid.UUID,
) (ValkeyInstance, error) {
	return findValkeyInstance(s.db.WithContext(ctx).Where(
		"id = ? AND user_id = ? AND deleted_at IS NULL",
		instanceID,
		userID,
	))
}

func (s *Store) FindOwnedValkeyInstance(
	ctx context.Context,
	userID uuid.UUID,
	instanceID uuid.UUID,
) (ValkeyInstance, error) {
	return findValkeyInstance(s.db.WithContext(ctx).Where("id = ? AND user_id = ?", instanceID, userID))
}

func (s *Store) FindOwnedValkeyInstanceForUpdate(
	ctx context.Context,
	tx *gorm.DB,
	userID uuid.UUID,
	instanceID uuid.UUID,
) (ValkeyInstance, error) {
	return findValkeyInstance(tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND user_id = ?", instanceID, userID))
}

func findValkeyInstance(query *gorm.DB) (ValkeyInstance, error) {
	var instance ValkeyInstance
	if err := query.First(&instance).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ValkeyInstance{}, ErrNotFound
		}

		return ValkeyInstance{}, fmt.Errorf("прочитать инстанс Valkey: %w", err)
	}

	return instance, nil
}

func (s *Store) ValkeyNameExists(
	ctx context.Context,
	tx *gorm.DB,
	userID uuid.UUID,
	name string,
	excludeID *uuid.UUID,
) (bool, error) {
	query := tx.WithContext(ctx).Model(&ValkeyInstance{}).
		Where("user_id = ? AND name = ? AND deleted_at IS NULL", userID, name)
	if excludeID != nil {
		query = query.Where("id <> ?", *excludeID)
	}

	var count int64
	if err := query.Count(&count).Error; err != nil {
		return false, fmt.Errorf("проверить имя инстанса Valkey: %w", err)
	}

	return count > 0, nil
}

func (s *Store) CreateValkeyInstance(
	ctx context.Context,
	tx *gorm.DB,
	instance *ValkeyInstance,
) (bool, error) {
	result := tx.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "slug"}}, DoNothing: true}).
		Create(instance)
	if result.Error != nil {
		return false, classifyValkeyError(result.Error)
	}

	return result.RowsAffected == 1, nil
}

func (s *Store) UpdateValkeyIntent(
	ctx context.Context,
	tx *gorm.DB,
	instanceID uuid.UUID,
	values map[string]any,
) error {
	result := tx.WithContext(ctx).Model(&ValkeyInstance{}).
		Where("id = ?", instanceID).
		UpdateColumns(values)
	if result.Error != nil {
		return classifyValkeyError(result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}

	return nil
}

func (s *Store) AcquireValkeyMutationLock(ctx context.Context, tx *gorm.DB) error {
	if err := tx.WithContext(ctx).Exec("SET LOCAL lock_timeout = '3s'").Error; err != nil {
		return fmt.Errorf("настроить срок ожидания блокировки: %w", err)
	}
	if err := tx.WithContext(ctx).Exec("SELECT pg_advisory_xact_lock(1)").Error; err != nil {
		if postgresError, ok := errors.AsType[*pgconn.PgError](err); ok && postgresError.Code == "55P03" {
			return ErrAdvisoryLockTimeout
		}

		return fmt.Errorf("получить общую блокировку Valkey: %w", err)
	}

	return nil
}

func (s *Store) DatabaseTime(ctx context.Context, tx *gorm.DB) (time.Time, error) {
	var now time.Time
	if err := tx.WithContext(ctx).Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
		return time.Time{}, fmt.Errorf("прочитать время PostgreSQL: %w", err)
	}

	return now.UTC(), nil
}

func (s *Store) ValkeyUsage(
	ctx context.Context,
	tx *gorm.DB,
	userID *uuid.UUID,
) (ResourceUsage, error) {
	query := `
		SELECT
			COALESCE(SUM((CASE WHEN mode = 'ha' THEN 3 ELSE 1 END) * GREATEST(vcpu, applied_vcpu)), 0) AS vcpu,
			COALESCE(SUM((CASE WHEN mode = 'ha' THEN 3 ELSE 1 END) * GREATEST(ram_gb, applied_ram_gb)), 0) AS ram_gb,
			COUNT(*) AS instances
		FROM valkey_instances
		WHERE deleted_at IS NULL`
	args := make([]any, 0, 1)
	if userID != nil {
		query += " AND user_id = ?"
		args = append(args, *userID)
	}

	var usage ResourceUsage
	if err := tx.WithContext(ctx).Raw(query, args...).Scan(&usage).Error; err != nil {
		return ResourceUsage{}, fmt.Errorf("посчитать резерв Valkey: %w", err)
	}

	return usage, nil
}

func (s *Store) GetQuotaInTx(ctx context.Context, tx *gorm.DB, userID uuid.UUID) (UserQuota, error) {
	var quota UserQuota
	if err := tx.WithContext(ctx).Where("user_id = ?", userID).First(&quota).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return UserQuota{}, ErrNotFound
		}

		return UserQuota{}, fmt.Errorf("прочитать квоту пользователя: %w", err)
	}

	return quota, nil
}

func (s *Store) GetQuotaUsage(ctx context.Context, userID uuid.UUID) (QuotaUsage, error) {
	var usage QuotaUsage
	err := s.db.WithContext(ctx).Raw(`
		SELECT
			q.max_vcpu,
			q.max_ram_gb,
			COALESCE(SUM((CASE WHEN i.mode = 'ha' THEN 3 ELSE 1 END) * GREATEST(i.vcpu, i.applied_vcpu)), 0) AS used_vcpu,
			COALESCE(SUM((CASE WHEN i.mode = 'ha' THEN 3 ELSE 1 END) * GREATEST(i.ram_gb, i.applied_ram_gb)), 0) AS used_ram_gb
		FROM user_quotas q
		LEFT JOIN valkey_instances i ON i.user_id = q.user_id AND i.deleted_at IS NULL
		WHERE q.user_id = ?
		GROUP BY q.max_vcpu, q.max_ram_gb`, userID).Scan(&usage).Error
	if err != nil {
		return QuotaUsage{}, fmt.Errorf("прочитать квоту и резерв пользователя: %w", err)
	}
	if usage.MaxVCPU == 0 || usage.MaxRAMGB == 0 {
		return QuotaUsage{}, ErrNotFound
	}

	return usage, nil
}

func (s *Store) GetValkeyCapacityUsage(ctx context.Context, userID uuid.UUID) (ValkeyCapacityUsage, error) {
	var usage ValkeyCapacityUsage
	err := s.db.WithContext(ctx).Raw(`
		WITH active_instances AS (
			SELECT
				user_id,
				(CASE WHEN mode = 'ha' THEN 3 ELSE 1 END) * GREATEST(vcpu, applied_vcpu) AS reserved_vcpu,
				(CASE WHEN mode = 'ha' THEN 3 ELSE 1 END) * GREATEST(ram_gb, applied_ram_gb) AS reserved_ram_gb
			FROM valkey_instances
			WHERE deleted_at IS NULL
		), cluster_usage AS (
			SELECT
				COALESCE(SUM(reserved_vcpu) FILTER (WHERE user_id = ?), 0) AS user_used_vcpu,
				COALESCE(SUM(reserved_ram_gb) FILTER (WHERE user_id = ?), 0) AS user_used_ram_gb,
				COALESCE(SUM(reserved_vcpu), 0) AS cluster_used_vcpu,
				COALESCE(SUM(reserved_ram_gb), 0) AS cluster_used_ram_gb,
				COUNT(*) AS instances
			FROM active_instances
		)
		SELECT
			q.max_vcpu,
			q.max_ram_gb,
			c.user_used_vcpu,
			c.user_used_ram_gb,
			c.cluster_used_vcpu,
			c.cluster_used_ram_gb,
			c.instances
		FROM user_quotas q
		CROSS JOIN cluster_usage c
		WHERE q.user_id = ?`, userID, userID, userID).Scan(&usage).Error
	if err != nil {
		return ValkeyCapacityUsage{}, fmt.Errorf("прочитать доступную ёмкость Valkey: %w", err)
	}
	if usage.MaxVCPU == 0 || usage.MaxRAMGB == 0 {
		return ValkeyCapacityUsage{}, ErrNotFound
	}

	return usage, nil
}

func (s *Store) CreateBillingPeriod(ctx context.Context, tx *gorm.DB, period *BillingPeriod) error {
	if err := tx.WithContext(ctx).Create(period).Error; err != nil {
		return fmt.Errorf("открыть биллинговый период: %w", err)
	}

	return nil
}

func (s *Store) CloseBillingPeriod(
	ctx context.Context,
	tx *gorm.DB,
	resourceID uuid.UUID,
	endedAt time.Time,
	endedReason domain.BillingPeriodEndReason,
) error {
	result := tx.WithContext(ctx).Model(&BillingPeriod{}).
		Where("service = ? AND resource_id = ? AND ended_at IS NULL", domain.ManagedServiceValkey, resourceID).
		UpdateColumns(map[string]any{"ended_at": endedAt, "ended_reason": endedReason})
	if result.Error != nil {
		return fmt.Errorf("закрыть биллинговый период: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("закрыть биллинговый период: %w", ErrNotFound)
	}

	return nil
}

func (s *Store) FindIdempotencyKey(
	ctx context.Context,
	tx *gorm.DB,
	userID uuid.UUID,
	keyID uuid.UUID,
) (IdempotencyKey, bool, error) {
	var key IdempotencyKey
	err := tx.WithContext(ctx).Where("user_id = ? AND key = ?", userID, keyID).First(&key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return IdempotencyKey{}, false, nil
	}
	if err != nil {
		return IdempotencyKey{}, false, fmt.Errorf("прочитать ключ идемпотентности: %w", err)
	}

	return key, true, nil
}

func (s *Store) DeleteIdempotencyKey(
	ctx context.Context,
	tx *gorm.DB,
	userID uuid.UUID,
	keyID uuid.UUID,
) error {
	if err := tx.WithContext(ctx).Where("user_id = ? AND key = ?", userID, keyID).
		Delete(&IdempotencyKey{}).Error; err != nil {
		return fmt.Errorf("удалить истёкший ключ идемпотентности: %w", err)
	}

	return nil
}

func (s *Store) CreateIdempotencyKey(ctx context.Context, tx *gorm.DB, key *IdempotencyKey) error {
	if err := tx.WithContext(ctx).Create(key).Error; err != nil {
		if postgresError, ok := errors.AsType[*pgconn.PgError](err); ok &&
			postgresError.ConstraintName == "idempotency_keys_pkey" {
			return ErrIdempotencyConflict
		}

		return fmt.Errorf("сохранить ключ идемпотентности: %w", err)
	}

	return nil
}

func classifyValkeyError(err error) error {
	if postgresError, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch postgresError.ConstraintName {
		case "valkey_instances_user_name_active_key":
			return ErrValkeyNameConflict
		case "valkey_instances_slug_key":
			return ErrValkeySlugConflict
		}
	}

	return fmt.Errorf("записать инстанс Valkey: %w", err)
}
