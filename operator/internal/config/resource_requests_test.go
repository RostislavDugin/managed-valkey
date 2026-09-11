//go:build !integration

package config_test

import (
	"testing"

	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

func Test_Load_WithIntegrationResourceRequestsWithoutIntegrationBuild_IgnoresValues(t *testing.T) {
	t.Setenv(config.EnvIntegrationRequestCPU, "100m")
	t.Setenv(config.EnvIntegrationRequestMemory, "128Mi")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}
	if len(cfg.ResourceRequests) != 0 {
		t.Fatalf("рабочая сборка приняла тестовые запросы ресурсов: %+v", cfg.ResourceRequests)
	}
}
