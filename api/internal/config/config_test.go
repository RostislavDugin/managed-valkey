package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RostislavDugin/managed-valkey/api/internal/config"
)

func Test_LoadConfig_WithoutDatabaseUrl_ReturnsRequiredValueError(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvDatabaseURL, "")

	_, err := config.Load()
	if !errors.Is(err, config.ErrDatabaseURLRequired) {
		t.Fatalf("ошибка %v, ожидалась %v", err, config.ErrDatabaseURLRequired)
	}
}

func Test_LoadConfig_WithoutJwtSecret_ReturnsRequiredValueError(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvJWTSecret, "")

	_, err := config.Load()
	if !errors.Is(err, config.ErrJWTSecretRequired) {
		t.Fatalf("ошибка %v, ожидалась %v", err, config.ErrJWTSecretRequired)
	}
}

func Test_LoadConfig_WithoutOptionalValues_UsesDefaults(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvHTTPAddr, "")
	t.Setenv(config.EnvValkeyPublicPort, "")
	t.Setenv(config.EnvValkeyVCPUPriceCoinsPerHour, "")
	t.Setenv(config.EnvValkeyRAMGBPriceCoinsPerHour, "")
	t.Setenv(config.EnvValkeyMetricsRetention, "")
	t.Setenv(config.EnvKubernetesBackgroundSync, "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}

	if cfg.HTTPAddr != config.DefaultHTTPAddr {
		t.Errorf("адрес %q, ожидался %q", cfg.HTTPAddr, config.DefaultHTTPAddr)
	}

	if cfg.JWTSecret != "test-secret" {
		t.Errorf("JWT secret %q, ожидался заданный", cfg.JWTSecret)
	}

	if cfg.Logging.ServiceName != config.ServiceName {
		t.Errorf("service.name %q, ожидался %q", cfg.Logging.ServiceName, config.ServiceName)
	}
	if !cfg.KubernetesBackgroundSyncEnabled {
		t.Error("фоновые процессы синхронизации с Kubernetes по умолчанию выключены")
	}
	if cfg.ValkeyPublicPort != config.DefaultValkeyPublicPort ||
		cfg.ValkeyVCPUPriceCoinsPerHour != config.DefaultValkeyVCPUPriceCoinsPerHour ||
		cfg.ValkeyRAMGBPriceCoinsPerHour != config.DefaultValkeyRAMGBPriceCoinsPerHour ||
		cfg.ValkeyMetricsRetention != config.DefaultValkeyMetricsRetention {
		t.Errorf("неожиданные значения Valkey по умолчанию: %+v", cfg)
	}
}

func Test_LoadConfig_WithMetricsRetention_ParsesDuration(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvValkeyMetricsRetention, "36h30m")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузить конфигурацию: %v", err)
	}
	if cfg.ValkeyMetricsRetention != 36*time.Hour+30*time.Minute {
		t.Fatalf("срок хранения %s, ожидался 36h30m", cfg.ValkeyMetricsRetention)
	}
}

func Test_LoadConfig_WithKubernetesBackgroundSyncDisabled_DisablesBackgroundSynchronization(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvKubernetesBackgroundSync, "false")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузить конфигурацию: %v", err)
	}
	if cfg.KubernetesBackgroundSyncEnabled {
		t.Fatal("фоновые процессы синхронизации с Kubernetes включены")
	}
}

func Test_LoadConfig_WithInvalidKubernetesBackgroundSync_ReturnsVariableError(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvKubernetesBackgroundSync, "sometimes")

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), config.EnvKubernetesBackgroundSync) {
		t.Fatalf("ошибка %v, ожидалось имя %s", err, config.EnvKubernetesBackgroundSync)
	}
}

func Test_LoadConfig_WithInvalidMetricsRetention_ReturnsVariableError(t *testing.T) {
	for _, value := range []string{"invalid", "0s", "-1h"} {
		t.Run("недопустимый срок хранения "+value+" возвращает ошибку переменной окружения", func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(config.EnvValkeyMetricsRetention, value)

			_, err := config.Load()
			if err == nil || !strings.Contains(err.Error(), config.EnvValkeyMetricsRetention) {
				t.Fatalf("ошибка %v, ожидалось имя %s", err, config.EnvValkeyMetricsRetention)
			}
		})
	}
}

func Test_LoadConfig_WithInvalidValkeyValues_ReturnsVariableError(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		value string
	}{
		{
			name:  "отсутствующее число нод возвращает ошибку переменной окружения",
			env:   config.EnvManagedK8SNodeCount,
			value: "",
		},
		{
			name:  "отсутствующая ёмкость CPU ноды возвращает ошибку переменной окружения",
			env:   config.EnvManagedK8SNodeCapacityVCPU,
			value: "",
		},
		{
			name:  "отсутствующая ёмкость RAM ноды возвращает ошибку переменной окружения",
			env:   config.EnvManagedK8SNodeCapacityRAMGB,
			value: "",
		},
		{
			name:  "отсутствующий резерв CPU возвращает ошибку переменной окружения",
			env:   config.EnvManagedK8SNodeReservedCPUMilli,
			value: "",
		},
		{
			name:  "отсутствующий резерв RAM возвращает ошибку переменной окружения",
			env:   config.EnvManagedK8SNodeReservedRAMMiB,
			value: "",
		},
		{
			name:  "отрицательный резерв RAM возвращает ошибку переменной окружения",
			env:   config.EnvManagedK8SNodeReservedRAMMiB,
			value: "-1",
		},
		{
			name:  "дробный максимум CPU возвращает ошибку переменной окружения",
			env:   config.EnvValkeyInstanceMaxVCPU,
			value: "1.5",
		},
		{
			name:  "нулевой максимум RAM возвращает ошибку переменной окружения",
			env:   config.EnvValkeyInstanceMaxRAMGB,
			value: "0",
		},
		{name: "нулевой порт возвращает ошибку переменной окружения", env: config.EnvValkeyPublicPort, value: "0"},
		{
			name:  "слишком большой порт возвращает ошибку переменной окружения",
			env:   config.EnvValkeyPublicPort,
			value: "65536",
		},
		{
			name:  "отрицательная ставка возвращает ошибку переменной окружения",
			env:   config.EnvValkeyVCPUPriceCoinsPerHour,
			value: "-1",
		},
		{
			name:  "недопустимый домен возвращает ошибку переменной окружения",
			env:   config.EnvValkeyBaseDomain,
			value: "bad_domain",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(testCase.env, testCase.value)

			_, err := config.Load()
			if err == nil || !strings.Contains(err.Error(), testCase.env) {
				t.Fatalf("ошибка %v, ожидалось имя %s", err, testCase.env)
			}
		})
	}
}

func Test_LoadConfig_WithThreeNodesAndInfrastructureReserve_CalculatesAvailableBudgets(t *testing.T) {
	setValidEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузить конфигурацию: %v", err)
	}
	if cfg.ManagedK8SNodeCount != 3 ||
		cfg.ManagedK8SNodeCapacityCPUMilli != 4000 ||
		cfg.ManagedK8SNodeCapacityRAMMiB != 16384 ||
		cfg.ManagedK8SNodeReservedCPUMilli != 500 ||
		cfg.ManagedK8SNodeReservedRAMMiB != 1024 ||
		cfg.ManagedK8SNodeAvailableCPUMilli != 3500 ||
		cfg.ManagedK8SNodeAvailableRAMMiB != 15360 ||
		cfg.ManagedK8SClusterAvailableCPUMilli != 10500 ||
		cfg.ManagedK8SClusterAvailableRAMMiB != 46080 {
		t.Fatalf("неверный бюджет управляемого кластера: %+v", cfg)
	}
}

func Test_LoadConfig_WithInfrastructureUsingWholeNode_ReturnsReserveVariableError(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		value string
	}{
		{name: "CPU", env: config.EnvManagedK8SNodeReservedCPUMilli, value: "4000"},
		{name: "RAM", env: config.EnvManagedK8SNodeReservedRAMMiB, value: "16384"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(testCase.env, testCase.value)

			_, err := config.Load()
			if err == nil || !strings.Contains(err.Error(), testCase.env) {
				t.Fatalf("ошибка %v, ожидалось имя %s", err, testCase.env)
			}
		})
	}
}

func Test_LoadConfig_WithLegacyClusterBudget_ReturnsLegacyVariableError(t *testing.T) {
	for _, legacy := range []string{config.EnvLegacyManagedK8SNodeVCPU, config.EnvLegacyManagedK8SNodeRAMGB} {
		t.Run(legacy, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(legacy, "12")

			_, err := config.Load()
			if err == nil || !strings.Contains(err.Error(), legacy) {
				t.Fatalf("ошибка %v, ожидалось имя %s", err, legacy)
			}
		})
	}
}

func Test_LoadConfig_WithOverflowingClusterCapacity_ReturnsVariableError(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvManagedK8SNodeCapacityVCPU, "9223372036854775807")

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), config.EnvManagedK8SNodeCapacityVCPU) {
		t.Fatalf("ошибка %v, ожидалось имя %s", err, config.EnvManagedK8SNodeCapacityVCPU)
	}
}

func setValidEnv(t *testing.T) {
	t.Helper()

	t.Setenv(config.EnvDatabaseURL, "postgres://user:pass@postgres:45432/managed_valkey")
	t.Setenv(config.EnvJWTSecret, "test-secret")
	t.Setenv(config.EnvKubernetesBackgroundSync, "true")
	t.Setenv(config.EnvValkeyBaseDomain, "valkey.localhost")
	t.Setenv(config.EnvValkeyPublicPort, "41379")
	t.Setenv(config.EnvValkeyInstanceMaxVCPU, "4")
	t.Setenv(config.EnvValkeyInstanceMaxRAMGB, "16")
	t.Setenv(config.EnvValkeyVCPUPriceCoinsPerHour, "125")
	t.Setenv(config.EnvValkeyRAMGBPriceCoinsPerHour, "50")
	t.Setenv(config.EnvValkeyMetricsRetention, "168h")
	t.Setenv(config.EnvManagedK8SNodeCount, "3")
	t.Setenv(config.EnvManagedK8SNodeCapacityVCPU, "4")
	t.Setenv(config.EnvManagedK8SNodeCapacityRAMGB, "16")
	t.Setenv(config.EnvManagedK8SNodeReservedCPUMilli, "500")
	t.Setenv(config.EnvManagedK8SNodeReservedRAMMiB, "1024")
	t.Setenv(config.EnvLegacyManagedK8SNodeVCPU, "")
	t.Setenv(config.EnvLegacyManagedK8SNodeRAMGB, "")
}
