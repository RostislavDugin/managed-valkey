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
	t.Setenv(config.EnvKubernetesSync, "")

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
	if !cfg.KubernetesSyncEnabled {
		t.Error("синхронизация Kubernetes по умолчанию выключена")
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

func Test_LoadConfig_WithKubernetesSyncDisabled_DisablesSynchronization(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvKubernetesSync, "false")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузить конфигурацию: %v", err)
	}
	if cfg.KubernetesSyncEnabled {
		t.Fatal("синхронизация Kubernetes включена")
	}
}

func Test_LoadConfig_WithInvalidKubernetesSync_ReturnsVariableError(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvKubernetesSync, "sometimes")

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), config.EnvKubernetesSync) {
		t.Fatalf("ошибка %v, ожидалось имя %s", err, config.EnvKubernetesSync)
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
			name:  "отсутствующий общий CPU возвращает ошибку переменной окружения",
			env:   config.EnvManagedK8SNodeVCPU,
			value: "",
		},
		{
			name:  "отрицательная общая RAM возвращает ошибку переменной окружения",
			env:   config.EnvManagedK8SNodeRAMGB,
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

func Test_LoadConfig_WithClusterResources_PreservesTotalBudget(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvManagedK8SNodeVCPU, "24")
	t.Setenv(config.EnvManagedK8SNodeRAMGB, "48")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузить конфигурацию: %v", err)
	}
	if cfg.ManagedK8SNodeVCPU != 24 || cfg.ManagedK8SNodeRAMGB != 48 {
		t.Fatalf("общий бюджет изменён: %+v", cfg)
	}
}

func setValidEnv(t *testing.T) {
	t.Helper()

	t.Setenv(config.EnvDatabaseURL, "postgres://user:pass@postgres:45432/managed_valkey")
	t.Setenv(config.EnvJWTSecret, "test-secret")
	t.Setenv(config.EnvKubernetesSync, "true")
	t.Setenv(config.EnvValkeyBaseDomain, "valkey.localhost")
	t.Setenv(config.EnvValkeyPublicPort, "41379")
	t.Setenv(config.EnvValkeyInstanceMaxVCPU, "4")
	t.Setenv(config.EnvValkeyInstanceMaxRAMGB, "16")
	t.Setenv(config.EnvValkeyVCPUPriceCoinsPerHour, "125")
	t.Setenv(config.EnvValkeyRAMGBPriceCoinsPerHour, "50")
	t.Setenv(config.EnvValkeyMetricsRetention, "168h")
	t.Setenv(config.EnvManagedK8SNodeVCPU, "24")
	t.Setenv(config.EnvManagedK8SNodeRAMGB, "48")
}
