//go:build !integration

package config_test

import (
	"testing"

	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

func TestProductionIgnoresEnvoyProcessOverride(t *testing.T) {
	t.Setenv("VALKEY_ENVOY_PROCESSES", "1")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}
	if cfg.EnvoyProcesses != config.DefaultEnvoyProcesses {
		t.Fatalf("процессов Envoy %d, ожидалось %d", cfg.EnvoyProcesses, config.DefaultEnvoyProcesses)
	}
}
