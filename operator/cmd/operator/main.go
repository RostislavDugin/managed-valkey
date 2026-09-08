// К PostgreSQL и HTTP API сервиса процесс не обращается: состояние приходит
// через CR.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/RostislavDugin/managed-valkey/internal/logging"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	"github.com/RostislavDugin/managed-valkey/operator/internal/operator"
)

// Недоступная VictoriaLogs не должна задерживать остановку процесса.
const logFlushTimeout = 5 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "operator:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger, shutdownLogs := logging.New(cfg.Logging)
	defer flushLogs(logger, shutdownLogs)

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("получить конфигурацию Kubernetes: %w", err)
	}

	mgr, err := operator.NewManager(restConfig, cfg, logger)
	if err != nil {
		return err
	}

	logger.Info("оператор запускается",
		"namespace", cfg.SystemNamespace,
		"leader_election", config.LeaderElection,
		"valkey_image", cfg.ValkeyImage,
	)

	if err := operator.Run(ctx, mgr); err != nil {
		return err
	}

	logger.Info("оператор остановлен")

	return nil
}

func flushLogs(logger *slog.Logger, shutdown logging.ShutdownFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), logFlushTimeout)
	defer cancel()

	if err := shutdown(ctx); err != nil {
		logger.Error("не удалось отправить накопленные логи", "error", err)
	}
}
