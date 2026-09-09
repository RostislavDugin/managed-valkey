package sync

import (
	"context"
	"log/slog"
	stdsync "sync"

	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

const workerCount = 4

type Repository interface {
	ListValkeyInstancesForSync(context.Context) ([]store.ValkeyInstance, error)
	FindValkeyInstanceForSync(context.Context, uuid.UUID) (store.ValkeyInstance, error)
	BindValkeyNamespaceUID(context.Context, uuid.UUID, string) error
	BindValkeyCRUID(context.Context, uuid.UUID, string) error
	SetValkeySyncRecoveryReason(context.Context, uuid.UUID, *string) error
	PrepareValkeyDeletion(context.Context, uuid.UUID, domain.ValkeyDeletionStage, string, *string) error
	CompleteValkeyDeletion(context.Context, uuid.UUID) error
	ImportValkeyObservation(context.Context, uuid.UUID, store.ValkeyObservation) error
}

type Service struct {
	repository Repository
	kubernetes client.Client
	logger     *slog.Logger
}

func NewService(repository Repository, kubernetes client.Client, logger *slog.Logger) *Service {
	return &Service{repository: repository, kubernetes: kubernetes, logger: logger}
}

func (s *Service) RunDelivery(ctx context.Context) error {
	instances, err := s.repository.ListValkeyInstancesForSync(ctx)
	if err != nil {
		return err
	}

	s.processInstances(ctx, instances, "доставить намерение", s.deliverInstance)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	return s.reportOrphanNamespaces(ctx)
}

func (s *Service) RunImport(ctx context.Context) error {
	instances, err := s.repository.ListValkeyInstancesForSync(ctx)
	if err != nil {
		return err
	}

	s.processInstances(ctx, instances, "импортировать наблюдение", s.importInstance)

	return ctx.Err()
}

func (s *Service) processInstances(
	ctx context.Context,
	instances []store.ValkeyInstance,
	operation string,
	process func(context.Context, uuid.UUID) error,
) {
	jobs := make(chan uuid.UUID)
	var workers stdsync.WaitGroup

	count := min(workerCount, len(instances))
	workers.Add(count)
	for range count {
		go func() {
			defer workers.Done()

			for instanceID := range jobs {
				if err := process(ctx, instanceID); err != nil && ctx.Err() == nil {
					s.logger.Error(operation, "instance_id", instanceID, "error", err)
				}
			}
		}()
	}

	for _, instance := range instances {
		select {
		case <-ctx.Done():
			close(jobs)
			workers.Wait()

			return
		case jobs <- instance.ID:
		}
	}
	close(jobs)
	workers.Wait()
}
