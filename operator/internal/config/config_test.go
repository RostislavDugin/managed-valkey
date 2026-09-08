package config_test

import (
	"testing"

	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

func TestLoadUsesDefaults(t *testing.T) {
	for _, name := range []string{
		config.EnvSystemNamespace,
		config.EnvValkeyImage,
	} {
		t.Setenv(name, "")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}

	if cfg.SystemNamespace != config.DefaultSystemNamespace {
		t.Errorf("namespace %q, ожидался %q", cfg.SystemNamespace, config.DefaultSystemNamespace)
	}

	if cfg.ValkeyImage != config.DefaultValkeyImage {
		t.Errorf("образ %q, ожидался %q", cfg.ValkeyImage, config.DefaultValkeyImage)
	}

	if cfg.Logging.ServiceName != config.ServiceName {
		t.Errorf("service.name %q, ожидался %q", cfg.Logging.ServiceName, config.ServiceName)
	}
}

func TestLoadIgnoresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:pass@postgres:45432/managed_valkey")

	if _, err := config.Load(); err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}
}
