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

	clockutils "k8s.io/utils/clock"

	"github.com/RostislavDugin/managed-valkey/api/internal/api"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/auth"
	"github.com/RostislavDugin/managed-valkey/api/internal/config"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeysync "github.com/RostislavDugin/managed-valkey/api/internal/sync"
	"github.com/RostislavDugin/managed-valkey/api/internal/valkey"
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
	auditService := audit.NewService(database, database)
	authService, err := auth.NewService(database, database, auditService, tokens, clock)
	if err != nil {
		return fmt.Errorf("создать сервис авторизации: %w", err)
	}
	catalog, err := valkey.NewCatalog(valkey.CatalogConfig{
		MaxVCPU:           cfg.ValkeyInstanceMaxVCPU,
		MaxRAMGB:          cfg.ValkeyInstanceMaxRAMGB,
		VCPUCoinsPerHour:  cfg.ValkeyVCPUPriceCoinsPerHour,
		RAMGBCoinsPerHour: cfg.ValkeyRAMGBPriceCoinsPerHour,
		Domain:            cfg.ValkeyBaseDomain, Port: cfg.ValkeyPublicPort,
	})
	if err != nil {
		return fmt.Errorf("создать каталог Valkey: %w", err)
	}
	valkeyService := valkey.NewService(
		database,
		database,
		auditService,
		database,
		clock,
		catalog,
		cfg.ManagedK8SNodeVCPU,
		cfg.ManagedK8SNodeRAMGB,
		valkey.CryptoSlugGenerator{},
	)
	kubernetes, err := valkeysync.NewKubernetesClient()
	if err != nil {
		return err
	}
	syncService := valkeysync.NewService(database, kubernetes, logger)
	syncRunner := valkeysync.NewRunner(
		config.SyncInterval,
		clockutils.RealClock{},
		logger,
		syncService.RunDelivery,
		syncService.RunImport,
	)
	syncContext, stopSync := context.WithCancel(ctx)
	syncRunner.Start(syncContext)

	router, err := api.NewRouter(logger, database, authService, valkeyService, auditService)
	if err != nil {
		stopSync()
		syncRunner.Wait()

		return fmt.Errorf("создать маршрутизатор HTTP: %w", err)
	}

	server := api.NewServer(cfg.HTTPAddr, router, logger, config.ShutdownTimeout)

	serverErr := server.Run(ctx)
	stopSync()
	syncRunner.Wait()
	if serverErr != nil {
		return serverErr
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
