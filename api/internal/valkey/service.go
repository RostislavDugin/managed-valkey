package valkey

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

const InstanceLimit = 32

type ClusterCapacity struct {
	CPUMilli int64
	RAMMiB   int64
}

type Repository interface {
	ListActiveValkeyInstances(context.Context, uuid.UUID) ([]store.ValkeyInstance, error)
	GetValkeyCapacityUsage(context.Context, uuid.UUID) (store.ValkeyCapacityUsage, error)
	FindActiveValkeyInstance(context.Context, uuid.UUID, uuid.UUID) (store.ValkeyInstance, error)
	FindOwnedValkeyInstance(context.Context, uuid.UUID, uuid.UUID) (store.ValkeyInstance, error)
	ReadValkeyMetricBuckets(
		context.Context,
		uuid.UUID,
		time.Time,
		time.Time,
		time.Duration,
	) ([]store.ValkeyMetricBucket, error)
	FindOwnedValkeyInstanceForUpdate(context.Context, *gorm.DB, uuid.UUID, uuid.UUID) (store.ValkeyInstance, error)
	ValkeyNameExists(context.Context, *gorm.DB, uuid.UUID, string, *uuid.UUID) (bool, error)
	CreateValkeyInstance(context.Context, *gorm.DB, *store.ValkeyInstance) (bool, error)
	UpdateValkeyIntent(context.Context, *gorm.DB, uuid.UUID, map[string]any) error
	AcquireValkeyMutationLock(context.Context, *gorm.DB) error
	ValkeyUsage(context.Context, *gorm.DB, *uuid.UUID) (store.ResourceUsage, error)
	GetQuotaInTx(context.Context, *gorm.DB, uuid.UUID) (store.UserQuota, error)
	CreateBillingPeriod(context.Context, *gorm.DB, *store.BillingPeriod) error
	CloseBillingPeriod(context.Context, *gorm.DB, uuid.UUID, time.Time, domain.BillingPeriodEndReason) error
	FindIdempotencyKey(context.Context, *gorm.DB, uuid.UUID, uuid.UUID) (store.IdempotencyKey, bool, error)
	DeleteIdempotencyKey(context.Context, *gorm.DB, uuid.UUID, uuid.UUID) error
	CreateIdempotencyKey(context.Context, *gorm.DB, *store.IdempotencyKey) error
}

type TxRunner interface {
	WithinTransaction(context.Context, func(*gorm.DB) error) error
}

type AuditWriter interface {
	Write(context.Context, *gorm.DB, audit.Event) error
}

type DatabaseClock interface {
	DatabaseTime(context.Context, *gorm.DB) (time.Time, error)
}

type Clock interface {
	Now() time.Time
}

type Service struct {
	repository    Repository
	txRunner      TxRunner
	audit         AuditWriter
	databaseClock DatabaseClock
	clock         Clock
	catalog       Catalog
	cluster       ClusterCapacity
	slugs         SlugGenerator
}

func NewService(
	repository Repository,
	txRunner TxRunner,
	auditWriter AuditWriter,
	databaseClock DatabaseClock,
	clock Clock,
	catalog Catalog,
	cluster ClusterCapacity,
	slugs SlugGenerator,
) *Service {
	return &Service{
		repository: repository, txRunner: txRunner, audit: auditWriter, databaseClock: databaseClock, clock: clock,
		catalog: catalog, cluster: cluster, slugs: slugs,
	}
}

func (s *Service) Sizes() Catalog {
	return s.catalog
}

func (s *Service) Capacity(ctx context.Context, actor Actor) (Capacity, error) {
	usage, err := s.repository.GetValkeyCapacityUsage(ctx, actor.ID)
	if err != nil {
		return Capacity{}, apierr.WrapInternal(err)
	}
	clusterVCPU := int(s.cluster.CPUMilli / 1000)
	clusterRAMGB := int(s.cluster.RAMMiB / 1024)

	return Capacity{
		User: CapacityBudget{
			Limit: CapacityResources{VCPU: usage.MaxVCPU, RAMGB: usage.MaxRAMGB},
			Used:  CapacityResources{VCPU: usage.UserUsedVCPU, RAMGB: usage.UserUsedRAMGB},
		},
		Cluster: CapacityBudget{
			Limit: CapacityResources{VCPU: clusterVCPU, RAMGB: clusterRAMGB},
			Used:  CapacityResources{VCPU: usage.ClusterUsedVCPU, RAMGB: usage.ClusterUsedRAMGB},
		},
		Instances: CapacityInstances{Limit: InstanceLimit, Used: usage.Instances},
	}, nil
}

func (s *Service) Owns(ctx context.Context, actor Actor, instanceID uuid.UUID) error {
	_, err := s.repository.FindOwnedValkeyInstance(ctx, actor.ID, instanceID)
	return publicStoreError(err)
}

func (s *Service) List(ctx context.Context, actor Actor) ([]Instance, error) {
	records, err := s.repository.ListActiveValkeyInstances(ctx, actor.ID)
	if err != nil {
		return nil, apierr.WrapInternal(err)
	}

	now := s.clock.Now().UTC()
	instances := make([]Instance, 0, len(records))
	for _, record := range records {
		instances = append(instances, instanceDTO(record, now))
	}

	return instances, nil
}

func (s *Service) Get(ctx context.Context, actor Actor, instanceID uuid.UUID) (Instance, error) {
	record, err := s.repository.FindActiveValkeyInstance(ctx, actor.ID, instanceID)
	if err != nil {
		return Instance{}, publicStoreError(err)
	}

	return instanceDTO(record, s.clock.Now().UTC()), nil
}

func (s *Service) GetCredentials(
	ctx context.Context,
	actor Actor,
	instanceID uuid.UUID,
) (Credentials, error) {
	record, err := s.repository.FindActiveValkeyInstance(ctx, actor.ID, instanceID)
	if err != nil {
		return Credentials{}, publicStoreError(err)
	}

	return credentialsDTO(record), nil
}

func (s *Service) beginMutation(ctx context.Context, fn func(*gorm.DB, time.Time) error) error {
	err := s.txRunner.WithinTransaction(ctx, func(tx *gorm.DB) error {
		if lockErr := s.repository.AcquireValkeyMutationLock(ctx, tx); lockErr != nil {
			return lockErr
		}

		now, timeErr := s.databaseClock.DatabaseTime(ctx, tx)
		if timeErr != nil {
			return timeErr
		}

		return fn(tx, now)
	})
	if errors.Is(err, store.ErrAdvisoryLockTimeout) {
		return apierr.New(
			apierr.CodeUnavailable,
			"Сервис временно занят",
			map[string]any{"reason": "lock_timeout"},
		)
	}
	if err != nil {
		if known, ok := errors.AsType[*apierr.Error](err); ok {
			return known
		}

		return apierr.WrapInternal(err)
	}

	return nil
}

func publicStoreError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return apierr.New(apierr.CodeNotFound, "Инстанс не найден", nil)
	}
	if err != nil {
		return apierr.WrapInternal(err)
	}

	return nil
}

func nameConflict() error {
	return apierr.New(
		apierr.CodeConflict,
		"База с таким именем уже существует",
		map[string]any{"reason": "name_taken", "field": "name"},
	)
}

func instanceNotReady(record store.ValkeyInstance, reason string) error {
	details := map[string]any{"reason": reason, "status": instanceDTO(record, time.Now().UTC()).Status}
	if record.ObservedAt != nil {
		details["observed_at"] = *record.ObservedAt
	}

	return apierr.New(apierr.CodeInstanceNotReady, "Инстанс не готов к изменению", details)
}

func operationInProgress(record store.ValkeyInstance) error {
	return apierr.New(
		apierr.CodeOperationInProgress,
		"Предыдущее изменение ещё применяется",
		map[string]any{
			"desired_generation":         record.DesiredGeneration,
			"observed_generation":        record.ObservedGeneration,
			"configuration_requested_at": record.ConfigurationRequestedAt,
		},
	)
}

func ensureReady(record store.ValkeyInstance, now time.Time) error {
	if record.DesiredGeneration != record.ObservedGeneration {
		return operationInProgress(record)
	}
	if record.Phase != domain.ValkeyInstancePhaseRunning && record.Phase != domain.ValkeyInstancePhaseDegraded {
		return instanceNotReady(record, "invalid_phase")
	}
	if isStale(record.ObservedAt, now) {
		return instanceNotReady(record, "stale_observation")
	}

	return nil
}

func auditEvent(
	actor Actor,
	action audit.EventAction,
	requestID string,
	resourceID uuid.UUID,
	now time.Time,
) audit.Event {
	service := domain.ManagedServiceValkey

	return audit.Event{
		UserID: actor.ID, UserEmail: actor.Email, Action: action,
		Service: &service, ResourceID: &resourceID, RequestID: requestID, CreatedAt: now,
	}
}
