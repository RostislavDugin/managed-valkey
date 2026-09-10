//go:build integration

package config_test

import (
	"testing"

	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

func Test_LoadEnvoyProcesses_WithIntegrationOverride_AcceptsOneAndRejectsInvalidValues(t *testing.T) {
	t.Setenv(config.EnvEnvoyProcesses, "1")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузка числа процессов Envoy: %v", err)
	}
	if cfg.EnvoyProcesses != 1 {
		t.Fatalf("процессов Envoy %d, ожидался один", cfg.EnvoyProcesses)
	}

	for _, value := range []string{"0", "3", "wrong"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(config.EnvEnvoyProcesses, value)
			if _, err := config.Load(); err == nil {
				t.Fatalf("некорректное число процессов Envoy %q принято", value)
			}
		})
	}
}
