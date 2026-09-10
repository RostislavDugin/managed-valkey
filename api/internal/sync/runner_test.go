package sync_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	valkeysync "github.com/RostislavDugin/managed-valkey/api/internal/sync"
)

func Test_StartSyncRunner_WhenPassIsStillActive_StartsImmediatelyWithoutOverlap(t *testing.T) {
	clock := clocktesting.NewFakeClock(time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC))
	deliveryStarted := make(chan struct{}, 8)
	deliveryRelease := make(chan struct{}, 8)
	importStarted := make(chan struct{})
	cleanupStarted := make(chan struct{})
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
	cleaner := func(ctx context.Context) error {
		close(cleanupStarted)
		<-ctx.Done()

		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	runner := valkeysync.NewRunner(
		time.Second,
		24*time.Hour,
		clock,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		delivery,
		importer,
		cleaner,
	)
	runner.Start(ctx)

	waitSignal(t, deliveryStarted)
	waitSignal(t, importStarted)
	waitSignal(t, cleanupStarted)
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

func Test_StopSyncRunner_WithActivePass_CancelsAndWaitsForPass(t *testing.T) {
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
		24*time.Hour,
		clock,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		delivery,
		func(context.Context) error { return nil },
		func(context.Context) error { return nil },
	)
	runner.Start(ctx)
	waitSignal(t, started)
	cancel()
	runner.Wait()
	waitSignal(t, stopped)
}

func Test_RunMetricCleanup_WhenPreviousPassFailsOrBlocks_RetriesWithoutOverlap(t *testing.T) {
	clock := clocktesting.NewFakeClock(time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC))
	cleanupStarted := make(chan int32, 4)
	cleanupRelease := make(chan struct{})
	cleanupReleased := make(chan struct{})
	var cleanupCalls atomic.Int32
	cleaner := func(ctx context.Context) error {
		call := cleanupCalls.Add(1)
		cleanupStarted <- call
		switch call {
		case 1:
			return errors.New("cleanup failed")
		case 2:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-cleanupRelease:
			}
			close(cleanupReleased)
		}

		return nil
	}
	blocked := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	logs := &bytes.Buffer{}
	ctx, cancel := context.WithCancel(context.Background())
	runner := valkeysync.NewRunner(
		time.Second,
		24*time.Hour,
		clock,
		slog.New(slog.NewTextHandler(logs, nil)),
		blocked,
		blocked,
		cleaner,
	)
	runner.Start(ctx)

	if call := waitCleanupCall(t, cleanupStarted); call != 1 {
		t.Fatalf("первый проход имеет номер %d", call)
	}
	waitForClockWaiters(t, clock, 1)
	clock.Step(24 * time.Hour)
	if call := waitCleanupCall(t, cleanupStarted); call != 2 {
		t.Fatalf("повторный проход имеет номер %d", call)
	}
	clock.Step(48 * time.Hour)
	select {
	case call := <-cleanupStarted:
		t.Fatalf("перекрывающийся проход имеет номер %d", call)
	case <-time.After(20 * time.Millisecond):
	}
	previousWaiters := clock.Waiters()
	close(cleanupRelease)
	waitSignal(t, cleanupReleased)
	waitForMoreClockWaiters(t, clock, previousWaiters)
	clock.Step(24 * time.Hour)
	if call := waitCleanupCall(t, cleanupStarted); call != 3 {
		t.Fatalf("третий проход имеет номер %d", call)
	}

	cancel()
	runner.Wait()
	if !strings.Contains(logs.String(), "cleanup failed") || !strings.Contains(logs.String(), "очистка метрик") {
		t.Fatalf("ошибка очистки не записана: %s", logs.String())
	}
}

func waitForMoreClockWaiters(t *testing.T, clock *clocktesting.FakeClock, previous int) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for clock.Waiters() <= previous {
		if time.Now().After(deadline) {
			t.Fatalf("ожидающих отсчёта времени: %d, ожидалось больше %d", clock.Waiters(), previous)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitCleanupCall(t *testing.T, calls <-chan int32) int32 {
	t.Helper()

	select {
	case call := <-calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("проход очистки не начался")
		return 0
	}
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
