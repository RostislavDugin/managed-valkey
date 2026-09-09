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
	syncInterval    time.Duration
	cleanupInterval time.Duration
	clock           clockutils.WithTicker
	logger          *slog.Logger
	delivery        Pass
	importer        Pass
	cleaner         Pass
	wait            stdsync.WaitGroup
}

func NewRunner(
	syncInterval time.Duration,
	cleanupInterval time.Duration,
	clock clockutils.WithTicker,
	logger *slog.Logger,
	delivery Pass,
	importer Pass,
	cleaner Pass,
) *Runner {
	return &Runner{
		syncInterval: syncInterval, cleanupInterval: cleanupInterval,
		clock: clock, logger: logger, delivery: delivery, importer: importer, cleaner: cleaner,
	}
}

func (r *Runner) Start(ctx context.Context) {
	r.wait.Add(3)

	go r.run(ctx, r.syncInterval, "доставка намерений", r.delivery)
	go r.run(ctx, r.syncInterval, "импорт наблюдений", r.importer)
	go r.run(ctx, r.cleanupInterval, "очистка метрик", r.cleaner)
}

func (r *Runner) Wait() {
	r.wait.Wait()
}

func (r *Runner) run(ctx context.Context, interval time.Duration, name string, pass Pass) {
	defer r.wait.Done()

	for {
		if err := pass(ctx); err != nil && ctx.Err() == nil {
			r.logger.Error("проход синхронизации завершился ошибкой", "direction", name, "error", err)
		}
		if ctx.Err() != nil {
			return
		}

		ticker := r.clock.NewTicker(interval)
		select {
		case <-ctx.Done():
			ticker.Stop()
			return
		case <-ticker.C():
			ticker.Stop()
		}
	}
}
