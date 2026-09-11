//go:build integration

package config_test

import (
	"testing"

	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

func Test_Load_WithIntegrationResourceRequests_UsesConfiguredValues(t *testing.T) {
	t.Setenv(config.EnvIntegrationRequestCPU, "100m")
	t.Setenv(config.EnvIntegrationRequestMemory, "128Mi")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}
	if cfg.ResourceRequests.Cpu().String() != "100m" ||
		cfg.ResourceRequests.Memory().String() != "128Mi" {
		t.Fatalf("запросы ресурсов не совпали: %+v", cfg.ResourceRequests)
	}
}

func Test_Load_WithIncompleteOrInvalidIntegrationResourceRequests_ReturnsError(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		cpu    string
		memory string
	}{
		{name: "задан только запрос CPU", cpu: "100m"},
		{name: "задан только запрос RAM", memory: "128Mi"},
		{name: "запрос CPU равен нулю", cpu: "0", memory: "128Mi"},
		{name: "запрос RAM имеет неверный формат", cpu: "100m", memory: "invalid"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(config.EnvIntegrationRequestCPU, testCase.cpu)
			t.Setenv(config.EnvIntegrationRequestMemory, testCase.memory)
			if _, err := config.Load(); err == nil {
				t.Fatal("некорректные запросы ресурсов приняты")
			}
		})
	}
}
