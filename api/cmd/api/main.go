// Схему PostgreSQL готовит отдельный процесс миграций до старта: API не
// применяет их сам.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RostislavDugin/managed-valkey/api/internal/api"
	"github.com/RostislavDugin/managed-valkey/api/internal/auth"
	"github.com/RostislavDugin/managed-valkey/api/internal/config"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	"github.com/RostislavDugin/managed-valkey/internal/logging"
)

// Недоступная VictoriaLogs не должна задерживать остановку процесса.
const logFlushTimeout = 5 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "api:", err)
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

	database, err := store.Open(ctx, cfg.DatabaseURL, logger)
	if err != nil {
		return err
	}

	defer func() {
		if err := database.Close(); err != nil {
			logger.Error("не удалось закрыть пул соединений", "error", err)
		}
	}()

	clock := auth.SystemClock{}
	tokens := auth.NewTokenService(cfg.JWTSecret, clock)
	authService, err := auth.NewService(database, tokens, clock)
	if err != nil {
		return fmt.Errorf("создать сервис авторизации: %w", err)
	}

	router, err := api.NewRouter(logger, database, authService)
	if err != nil {
		return fmt.Errorf("создать маршрутизатор HTTP: %w", err)
	}

	server := api.NewServer(cfg.HTTPAddr, router, logger, config.ShutdownTimeout)

	if err := server.Run(ctx); err != nil {
		return err
	}

	logger.Info("api остановлен")

	return nil
}

func flushLogs(logger *slog.Logger, shutdown logging.ShutdownFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), logFlushTimeout)
	defer cancel()

	if err := shutdown(ctx); err != nil {
		logger.Error("не удалось отправить накопленные логи", "error", err)
	}
}
