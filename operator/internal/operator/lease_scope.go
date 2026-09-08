package operator

import (
	"context"
	"errors"
	"sync"

	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

var ErrLeaseInactive = errors.New("lease оператора не активен")

type LeaseScope struct {
	mu          sync.Mutex
	lease       context.Context
	active      bool
	connections map[connectionCloser]struct{}
}

type connectionCloser interface {
	Close()
}

func NewLeaseScope() *LeaseScope {
	return &LeaseScope{connections: make(map[connectionCloser]struct{})}
}

func (s *LeaseScope) NeedLeaderElection() bool {
	return true
}

func (s *LeaseScope) Start(ctx context.Context) error {
	s.mu.Lock()
	s.lease = ctx
	s.active = true
	s.mu.Unlock()

	<-ctx.Done()
	s.closeConnections()

	return nil
}

func (s *LeaseScope) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.active && s.lease.Err() == nil
}

func (s *LeaseScope) DialValkey(
	ctx context.Context,
	cfg operatorvalkey.ClientConfig,
) (*operatorvalkey.Client, error) {
	s.mu.Lock()
	if !s.active || s.lease.Err() != nil {
		s.mu.Unlock()

		return nil, ErrLeaseInactive
	}
	lease := s.lease
	s.mu.Unlock()

	cfg.LeaseContext = lease
	connection, err := operatorvalkey.Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if !s.active || s.lease != lease || lease.Err() != nil {
		s.mu.Unlock()
		connection.Close()

		return nil, operatorvalkey.ErrLeaseLost
	}
	s.connections[connection] = struct{}{}
	s.mu.Unlock()

	return connection, nil
}

func (s *LeaseScope) closeConnections() {
	s.mu.Lock()
	s.active = false
	connections := make([]connectionCloser, 0, len(s.connections))
	for connection := range s.connections {
		connections = append(connections, connection)
	}
	clear(s.connections)
	s.mu.Unlock()

	for _, connection := range connections {
		connection.Close()
	}
}
