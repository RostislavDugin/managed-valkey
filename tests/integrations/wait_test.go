//go:build integration

package integrations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type processChecker interface {
	Check() error
}

type pidProcessChecker struct {
	apiPID      int
	operatorPID int
}

func (checker pidProcessChecker) Check() error {
	for name, pid := range map[string]int{"API": checker.apiPID, "operator": checker.operatorPID} {
		if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("процесс %s с PID %d завершился", name, pid)
		}
	}

	return nil
}

type checkFunc func() error

func (function checkFunc) Check() error {
	return function()
}

func waitForCondition(
	ctx context.Context,
	checker processChecker,
	interval time.Duration,
	observe func(context.Context) (string, bool, error),
) (string, error) {
	lastObservation := "условие ещё не проверялось"
	for {
		if err := checker.Check(); err != nil {
			return lastObservation, err
		}
		observation, ready, err := observe(ctx)
		if observation != "" {
			lastObservation = observation
		}
		if err != nil {
			return lastObservation, err
		}
		if ready {
			return lastObservation, nil
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}

			return lastObservation, fmt.Errorf("срок ожидания истёк: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func Test_WaitForCondition_WhenObservationBecomesReady_ReturnsLastObservation(t *testing.T) {
	attempts := 0
	last, err := waitForCondition(
		context.Background(),
		checkFunc(func() error { return nil }),
		time.Millisecond,
		func(context.Context) (string, bool, error) {
			attempts++

			return fmt.Sprintf("attempt=%d", attempts), attempts == 3, nil
		},
	)
	if err != nil || last != "attempt=3" {
		t.Fatalf("ожидание завершилось неверно: last=%q error=%v", last, err)
	}
}

func Test_WaitForCondition_WhenContextIsCancelled_ReturnsCancellationAndObservation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	last, err := waitForCondition(
		ctx,
		checkFunc(func() error { return nil }),
		time.Hour,
		func(context.Context) (string, bool, error) {
			cancel()

			return "status=provisioning", false, nil
		},
	)
	if !errors.Is(err, context.Canceled) || last != "status=provisioning" {
		t.Fatalf("отмена обработана неверно: last=%q error=%v", last, err)
	}
}

func Test_WaitForCondition_WhenDeadlineExpires_ReturnsDeadlineAndObservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	last, err := waitForCondition(
		ctx,
		checkFunc(func() error { return nil }),
		time.Millisecond,
		func(context.Context) (string, bool, error) {
			return "pod=Pending", false, nil
		},
	)
	if !errors.Is(err, context.DeadlineExceeded) || last != "pod=Pending" {
		t.Fatalf("срок обработан неверно: last=%q error=%v", last, err)
	}
}

func Test_WaitForCondition_WhenProcessStops_FailsBeforeAnotherObservation(t *testing.T) {
	observations := 0
	last, err := waitForCondition(
		context.Background(),
		checkFunc(func() error { return errors.New("operator stopped") }),
		time.Hour,
		func(context.Context) (string, bool, error) {
			observations++

			return "unexpected", true, nil
		},
	)
	if err == nil || observations != 0 || last != "условие ещё не проверялось" {
		t.Fatalf("завершение процесса обработано неверно: last=%q observations=%d error=%v", last, observations, err)
	}
}

type scenarioBarrier struct {
	mu       sync.Mutex
	want     int
	arrived  int
	released chan struct{}
	err      error
}

func newScenarioBarrier(participants int) *scenarioBarrier {
	return &scenarioBarrier{want: participants, released: make(chan struct{})}
}

func (barrier *scenarioBarrier) Wait(ctx context.Context) error {
	barrier.mu.Lock()
	if barrier.err != nil {
		err := barrier.err
		barrier.mu.Unlock()

		return err
	}
	barrier.arrived++
	if barrier.arrived == barrier.want {
		close(barrier.released)
	}
	released := barrier.released
	barrier.mu.Unlock()

	select {
	case <-released:
		barrier.mu.Lock()
		err := barrier.err
		barrier.mu.Unlock()

		return err
	case <-ctx.Done():
		barrier.Cancel(ctx.Err())

		return ctx.Err()
	}
}

func (barrier *scenarioBarrier) Cancel(err error) {
	barrier.mu.Lock()
	defer barrier.mu.Unlock()

	if barrier.err != nil || barrier.arrived >= barrier.want {
		return
	}
	barrier.err = err
	close(barrier.released)
}

func Test_ScenarioBarrier_WithFourParticipants_ReleasesAllTogether(t *testing.T) {
	barrier := newScenarioBarrier(4)
	released := make(chan time.Time, 4)
	for range 4 {
		go func() {
			if err := barrier.Wait(context.Background()); err != nil {
				return
			}
			released <- time.Now()
		}()
	}

	times := make([]time.Time, 0, 4)
	for range 4 {
		select {
		case releasedAt := <-released:
			times = append(times, releasedAt)
		case <-time.After(time.Second):
			t.Fatal("четыре участника не прошли барьер")
		}
	}
	oldest, newest := times[0], times[0]
	for _, releasedAt := range times[1:] {
		if releasedAt.Before(oldest) {
			oldest = releasedAt
		}
		if releasedAt.After(newest) {
			newest = releasedAt
		}
	}
	if newest.Sub(oldest) > 50*time.Millisecond {
		t.Fatalf("участники выпущены не одновременно: разброс %s", newest.Sub(oldest))
	}
}

func Test_ScenarioBarrier_WhenParticipantCancels_ReleasesWaitingParticipantsWithError(t *testing.T) {
	barrier := newScenarioBarrier(4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := barrier.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("отменённый участник вернул %v", err)
	}
	if err := barrier.Wait(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("ожидающий участник не получил отмену: %v", err)
	}
}

var diagnosticsFileMu sync.Mutex

func appendDiagnosticLine(path string, values ...string) error {
	diagnosticsFileMu.Lock()
	defer diagnosticsFileMu.Unlock()

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = fmt.Fprintln(file, strings.Join(values, "\t"))

	return err
}
