package valkey_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

type capacityRepository struct {
	valkeydomain.Repository
	usage store.ValkeyCapacityUsage
}

func (r capacityRepository) GetValkeyCapacityUsage(context.Context, uuid.UUID) (store.ValkeyCapacityUsage, error) {
	return r.usage, nil
}

func Test_Capacity_WithConfiguredAggregateBudget_ReturnsRoundedLimitAndRepositoryUsage(t *testing.T) {
	repository := capacityRepository{usage: store.ValkeyCapacityUsage{
		MaxVCPU: 4, MaxRAMGB: 12, UserUsedVCPU: 3, UserUsedRAMGB: 6,
		ClusterUsedVCPU: 9, ClusterUsedRAMGB: 30, Instances: 5,
	}}
	service := valkeydomain.NewService(
		repository,
		nil,
		nil,
		nil,
		nil,
		valkeydomain.Catalog{},
		valkeydomain.ClusterCapacity{CPUMilli: 10200, RAMMiB: 44236},
		nil,
	)

	capacity, err := service.Capacity(context.Background(), valkeydomain.Actor{ID: uuid.New()})
	if err != nil {
		t.Fatalf("получить доступную ёмкость: %v", err)
	}

	if capacity.User.Limit != (valkeydomain.CapacityResources{VCPU: 4, RAMGB: 12}) ||
		capacity.User.Used != (valkeydomain.CapacityResources{VCPU: 3, RAMGB: 6}) {
		t.Fatalf("неверный личный бюджет: %+v", capacity.User)
	}
	if capacity.Cluster.Limit != (valkeydomain.CapacityResources{VCPU: 10, RAMGB: 43}) ||
		capacity.Cluster.Used != (valkeydomain.CapacityResources{VCPU: 9, RAMGB: 30}) {
		t.Fatalf("неверный общий бюджет: %+v", capacity.Cluster)
	}
	if capacity.Instances != (valkeydomain.CapacityInstances{Limit: 32, Used: 5}) {
		t.Fatalf("неверный предел инстансов: %+v", capacity.Instances)
	}
}
