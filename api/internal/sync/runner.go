package sync

import (
	"context"
	"log/slog"
	stdsync "sync"
	"time"

	clockutils "k8s.io/utils/clock"
)

type Pass func(context.Context) error

type Runner struct {
	interval time.Duration
	clock    clockutils.WithTicker
	logger   *slog.Logger
	delivery Pass
	importer Pass
	wait     stdsync.WaitGroup
}

func NewRunner(
	interval time.Duration,
	clock clockutils.WithTicker,
	logger *slog.Logger,
	delivery Pass,
	importer Pass,
) *Runner {
	return &Runner{interval: interval, clock: clock, logger: logger, delivery: delivery, importer: importer}
}

func (r *Runner) Start(ctx context.Context) {
	r.wait.Add(2)

	go r.run(ctx, "доставка намерений", r.delivery)
	go r.run(ctx, "импорт наблюдений", r.importer)
}

func (r *Runner) Wait() {
	r.wait.Wait()
}

func (r *Runner) run(ctx context.Context, name string, pass Pass) {
	defer r.wait.Done()

	for {
		if err := pass(ctx); err != nil && ctx.Err() == nil {
			r.logger.Error("проход синхронизации завершился ошибкой", "direction", name, "error", err)
		}
		if ctx.Err() != nil {
			return
		}

		ticker := r.clock.NewTicker(r.interval)
		select {
		case <-ctx.Done():
			ticker.Stop()
			return
		case <-ticker.C():
			ticker.Stop()
		}
	}
}
