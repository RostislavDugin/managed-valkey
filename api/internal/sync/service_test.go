package sync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

func Test_ProcessInstances_WhenOneInstanceFails_ContinuesWithNextInstance(t *testing.T) {
	failedID := uuid.Must(uuid.NewV7())
	healthyID := uuid.Must(uuid.NewV7())
	healthyCalled := make(chan struct{})
	service := &Service{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	service.processInstances(
		context.Background(),
		[]store.ValkeyInstance{{ID: failedID}, {ID: healthyID}},
		"импортировать наблюдение",
		func(_ context.Context, instanceID uuid.UUID) error {
			if instanceID == failedID {
				return errors.New("import failed")
			}
			close(healthyCalled)
			return nil
		},
	)

	select {
	case <-healthyCalled:
	case <-time.After(time.Second):
		t.Fatal("здоровый инстанс не обработан после ошибки соседнего")
	}
}
