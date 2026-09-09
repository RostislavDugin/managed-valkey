package sync_test

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	valkeysync "github.com/RostislavDugin/managed-valkey/api/internal/sync"
)

func TestRunnerStartsImmediatelyAndDoesNotOverlapPasses(t *testing.T) {
	clock := clocktesting.NewFakeClock(time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC))
	deliveryStarted := make(chan struct{}, 8)
	deliveryRelease := make(chan struct{}, 8)
	importStarted := make(chan struct{})
	var activeDelivery atomic.Int32
	var maximumDelivery atomic.Int32

	delivery := func(ctx context.Context) error {
		active := activeDelivery.Add(1)
		maximumDelivery.Store(max(maximumDelivery.Load(), active))
		deliveryStarted <- struct{}{}
		select {
		case <-ctx.Done():
		case <-deliveryRelease:
		}
		activeDelivery.Add(-1)

		return nil
	}
	importer := func(ctx context.Context) error {
		close(importStarted)
		<-ctx.Done()

		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	runner := valkeysync.NewRunner(
		time.Second,
		clock,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		delivery,
		importer,
	)
	runner.Start(ctx)

	waitSignal(t, deliveryStarted)
	waitSignal(t, importStarted)
	clock.Step(5 * time.Second)
	assertNoSignal(t, deliveryStarted)
	deliveryRelease <- struct{}{}
	assertNoSignal(t, deliveryStarted)
	waitForClockWaiters(t, clock, 1)
	clock.Step(time.Second)
	waitSignal(t, deliveryStarted)
	if maximumDelivery.Load() != 1 {
		t.Fatalf("одновременно выполнялось проходов доставки: %d", maximumDelivery.Load())
	}

	cancel()
	runner.Wait()
}

func waitForClockWaiters(t *testing.T, clock *clocktesting.FakeClock, count int) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for clock.Waiters() != count {
		if time.Now().After(deadline) {
			t.Fatalf("ожидающих отсчёта времени: %d, ожидалось: %d", clock.Waiters(), count)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunnerCancelsActivePass(t *testing.T) {
	clock := clocktesting.NewFakeClock(time.Now())
	started := make(chan struct{})
	stopped := make(chan struct{})
	delivery := func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(stopped)

		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	runner := valkeysync.NewRunner(
		time.Second,
		clock,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		delivery,
		func(context.Context) error { return nil },
	)
	runner.Start(ctx)
	waitSignal(t, started)
	cancel()
	runner.Wait()
	waitSignal(t, stopped)
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("событие не наступило")
	}
}

func assertNoSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()

	select {
	case <-signal:
		t.Fatal("получено лишнее событие")
	case <-time.After(20 * time.Millisecond):
	}
}
