package config_test

import (
	"errors"
	"os"
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
			name:  "отсутствующая суммарная ёмкость CPU возвращает ошибку переменной окружения",
			env:   config.EnvManagedK8SClusterVCPU,
			value: "",
		},
		{
			name:  "отсутствующая суммарная ёмкость RAM возвращает ошибку переменной окружения",
			env:   config.EnvManagedK8SClusterRAMGB,
			value: "",
		},
		{
			name:  "нулевая суммарная ёмкость CPU возвращает ошибку переменной окружения",
			env:   config.EnvManagedK8SClusterVCPU,
			value: "0",
		},
		{
			name:  "отрицательная суммарная ёмкость RAM возвращает ошибку переменной окружения",
			env:   config.EnvManagedK8SClusterRAMGB,
			value: "-1",
		},
		{
			name:  "дробная суммарная ёмкость CPU возвращает ошибку переменной окружения",
			env:   config.EnvManagedK8SClusterVCPU,
			value: "1.5",
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

func Test_LoadConfig_WithTwelveVCPUAndFortyEightGiB_CalculatesAvailableBudgets(t *testing.T) {
	setValidEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузить конфигурацию: %v", err)
	}
	if cfg.ManagedK8SClusterVCPU != 12 ||
		cfg.ManagedK8SClusterRAMGB != 48 ||
		cfg.ManagedK8SClusterAvailableCPUMilli != 10200 ||
		cfg.ManagedK8SClusterAvailableRAMMiB != 44236 {
		t.Fatalf("неверный бюджет управляемого кластера: %+v", cfg)
	}
}

func Test_LoadConfig_WithFourVCPUAndSixteenGiB_CalculatesAvailableBudgets(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvManagedK8SClusterVCPU, "4")
	t.Setenv(config.EnvManagedK8SClusterRAMGB, "16")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузить конфигурацию: %v", err)
	}
	if cfg.ManagedK8SClusterAvailableCPUMilli != 3400 || cfg.ManagedK8SClusterAvailableRAMMiB != 14745 {
		t.Fatalf("неверный бюджет управляемого кластера: %+v", cfg)
	}
}

func Test_LoadConfig_WithFractionalPercentageResult_RoundsAvailableBudgetDown(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvManagedK8SClusterVCPU, "1")
	t.Setenv(config.EnvManagedK8SClusterRAMGB, "1")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузить конфигурацию: %v", err)
	}
	if cfg.ManagedK8SClusterAvailableCPUMilli != 850 || cfg.ManagedK8SClusterAvailableRAMMiB != 921 {
		t.Fatalf("неверно округлённый бюджет управляемого кластера: %+v", cfg)
	}
}

func Test_LoadConfig_WithDeprecatedManagedKubernetesVariable_ReturnsVariableError(t *testing.T) {
	deprecated := []string{
		config.EnvManagedK8SNodeCount,
		config.EnvManagedK8SNodeCapacityVCPU,
		config.EnvManagedK8SNodeCapacityRAMGB,
		config.EnvManagedK8SNodeReservedCPUMilli,
		config.EnvManagedK8SNodeReservedRAMMiB,
		config.EnvLegacyManagedK8SNodeVCPU,
		config.EnvLegacyManagedK8SNodeRAMGB,
	}
	for _, name := range deprecated {
		t.Run(name+" останавливает запуск", func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(name, "12")

			_, err := config.Load()
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("ошибка %v, ожидалось имя %s", err, name)
			}
		})
	}
}

func Test_LoadConfig_WithOverflowingClusterCapacity_ReturnsVariableError(t *testing.T) {
	for _, name := range []string{config.EnvManagedK8SClusterVCPU, config.EnvManagedK8SClusterRAMGB} {
		t.Run(name+" останавливает запуск", func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(name, "9223372036854775807")

			_, err := config.Load()
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("ошибка %v, ожидалось имя %s", err, name)
			}
		})
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
	t.Setenv(config.EnvManagedK8SClusterVCPU, "12")
	t.Setenv(config.EnvManagedK8SClusterRAMGB, "48")
	for _, name := range []string{
		config.EnvManagedK8SNodeCount,
		config.EnvManagedK8SNodeCapacityVCPU,
		config.EnvManagedK8SNodeCapacityRAMGB,
		config.EnvManagedK8SNodeReservedCPUMilli,
		config.EnvManagedK8SNodeReservedRAMMiB,
		config.EnvLegacyManagedK8SNodeVCPU,
		config.EnvLegacyManagedK8SNodeRAMGB,
	} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("удалить переменную %s: %v", name, err)
		}
	}
}
