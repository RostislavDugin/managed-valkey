package config_test

import (
	"errors"
	"testing"

	"github.com/RostislavDugin/managed-valkey/api/internal/config"
)

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv(config.EnvDatabaseURL, "")
	t.Setenv(config.EnvJWTSecret, "test-secret")

	_, err := config.Load()
	if !errors.Is(err, config.ErrDatabaseURLRequired) {
		t.Fatalf("ошибка %v, ожидалась %v", err, config.ErrDatabaseURLRequired)
	}
}

func TestLoadRequiresJWTSecret(t *testing.T) {
	t.Setenv(config.EnvDatabaseURL, "postgres://user:pass@postgres:45432/managed_valkey")
	t.Setenv(config.EnvJWTSecret, "")

	_, err := config.Load()
	if !errors.Is(err, config.ErrJWTSecretRequired) {
		t.Fatalf("ошибка %v, ожидалась %v", err, config.ErrJWTSecretRequired)
	}
}

func TestLoadUsesDefaults(t *testing.T) {
	t.Setenv(config.EnvDatabaseURL, "postgres://user:pass@postgres:45432/managed_valkey")
	t.Setenv(config.EnvJWTSecret, "test-secret")
	t.Setenv(config.EnvHTTPAddr, "")

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
}
