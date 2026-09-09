package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/RostislavDugin/managed-valkey/api/internal/config"
)

func TestLoadRequiresDatabaseURL(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvDatabaseURL, "")

	_, err := config.Load()
	if !errors.Is(err, config.ErrDatabaseURLRequired) {
		t.Fatalf("ошибка %v, ожидалась %v", err, config.ErrDatabaseURLRequired)
	}
}

func TestLoadRequiresJWTSecret(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvJWTSecret, "")

	_, err := config.Load()
	if !errors.Is(err, config.ErrJWTSecretRequired) {
		t.Fatalf("ошибка %v, ожидалась %v", err, config.ErrJWTSecretRequired)
	}
}

func TestLoadUsesDefaults(t *testing.T) {
	setValidEnv(t)
	t.Setenv(config.EnvHTTPAddr, "")
	t.Setenv(config.EnvValkeyPublicPort, "")
	t.Setenv(config.EnvValkeyVCPUPriceCoinsPerHour, "")
	t.Setenv(config.EnvValkeyRAMGBPriceCoinsPerHour, "")

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
	if cfg.ValkeyPublicPort != config.DefaultValkeyPublicPort ||
		cfg.ValkeyVCPUPriceCoinsPerHour != config.DefaultValkeyVCPUPriceCoinsPerHour ||
		cfg.ValkeyRAMGBPriceCoinsPerHour != config.DefaultValkeyRAMGBPriceCoinsPerHour {
		t.Errorf("неожиданные значения Valkey по умолчанию: %+v", cfg)
	}
}

func TestLoadValidatesValkeyConfiguration(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		value string
	}{
		{name: "общий CPU отсутствует", env: config.EnvManagedK8SNodeVCPU, value: ""},
		{name: "общая RAM отрицательная", env: config.EnvManagedK8SNodeRAMGB, value: "-1"},
		{name: "максимум CPU дробный", env: config.EnvValkeyInstanceMaxVCPU, value: "1.5"},
		{name: "максимум RAM нулевой", env: config.EnvValkeyInstanceMaxRAMGB, value: "0"},
		{name: "порт нулевой", env: config.EnvValkeyPublicPort, value: "0"},
		{name: "порт слишком большой", env: config.EnvValkeyPublicPort, value: "65536"},
		{name: "ставка отрицательная", env: config.EnvValkeyVCPUPriceCoinsPerHour, value: "-1"},
		{name: "домен повреждён", env: config.EnvValkeyBaseDomain, value: "bad_domain"},
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

func TestLoadKeepsClusterBudgetAsTotal(t *testing.T) {
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
	t.Setenv(config.EnvValkeyBaseDomain, "valkey.localhost")
	t.Setenv(config.EnvValkeyPublicPort, "41379")
	t.Setenv(config.EnvValkeyInstanceMaxVCPU, "4")
	t.Setenv(config.EnvValkeyInstanceMaxRAMGB, "16")
	t.Setenv(config.EnvValkeyVCPUPriceCoinsPerHour, "125")
	t.Setenv(config.EnvValkeyRAMGBPriceCoinsPerHour, "50")
	t.Setenv(config.EnvManagedK8SNodeVCPU, "24")
	t.Setenv(config.EnvManagedK8SNodeRAMGB, "48")
}
