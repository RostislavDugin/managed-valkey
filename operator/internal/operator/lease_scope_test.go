package operator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func TestLeaseScopeActivationAndLoss(t *testing.T) {
	scope := NewLeaseScope()
	if scope.Active() {
		t.Fatal("Lease активен до запуска lifecycle")
	}
	if _, err := scope.DialValkey(
		context.Background(),
		operatorvalkey.ClientConfig{},
	); !errors.Is(
		err,
		ErrLeaseInactive,
	) {
		t.Fatalf("подключение без Lease вернуло %v", err)
	}

	leaseCtx, loseLease := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- scope.Start(leaseCtx)
	}()

	deadline := time.Now().Add(time.Second)
	for !scope.Active() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !scope.Active() {
		t.Fatal("Lease не стал активным")
	}
	connection := &trackedConnection{closed: make(chan struct{})}
	scope.mu.Lock()
	scope.connections[connection] = struct{}{}
	scope.mu.Unlock()

	loseLease()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("остановить lifecycle: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle не остановился после потери Lease")
	}
	if scope.Active() {
		t.Fatal("Lease остался активным после отмены")
	}
	select {
	case <-connection.closed:
	default:
		t.Fatal("соединение не закрыто после потери Lease")
	}
}

func TestLeaseScopeRequiresLeaderElection(t *testing.T) {
	if !NewLeaseScope().NeedLeaderElection() {
		t.Fatal("lifecycle запущен без владения Lease")
	}
}

type trackedConnection struct {
	once   sync.Once
	closed chan struct{}
}

func (c *trackedConnection) Close() {
	c.once.Do(func() {
		close(c.closed)
	})
}
