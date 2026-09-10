package config_test

import (
	"testing"

	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

func Test_Load_WhenEnvironmentIsEmpty_UsesDefaults(t *testing.T) {
	for _, name := range []string{
		config.EnvSystemNamespace,
		config.EnvValkeyImage,
		config.EnvBaseDomain,
		config.EnvOperatorCIDRs,
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
	if cfg.BaseDomain != config.DefaultBaseDomain {
		t.Errorf("базовый домен %q, ожидался %q", cfg.BaseDomain, config.DefaultBaseDomain)
	}
	if cfg.EnvoyProcesses != config.DefaultEnvoyProcesses {
		t.Errorf("процессов Envoy %d, ожидалось %d", cfg.EnvoyProcesses, config.DefaultEnvoyProcesses)
	}

	if cfg.Logging.ServiceName != config.ServiceName {
		t.Errorf("service.name %q, ожидался %q", cfg.Logging.ServiceName, config.ServiceName)
	}
}

func Test_Load_WithOperatorCIDRs_NormalizesValidValuesAndRejectsInvalidValue(t *testing.T) {
	t.Setenv(config.EnvOperatorCIDRs, " 172.27.0.1/32,2001:db8::1/64,172.27.0.1/32 ")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузка CIDR оператора: %v", err)
	}
	if len(cfg.OperatorCIDRs) != 2 || cfg.OperatorCIDRs[0] != "172.27.0.1/32" ||
		cfg.OperatorCIDRs[1] != "2001:db8::/64" {
		t.Fatalf("CIDR оператора не нормализованы: %v", cfg.OperatorCIDRs)
	}

	t.Setenv(config.EnvOperatorCIDRs, "not-a-cidr")
	if _, err := config.Load(); err == nil {
		t.Fatal("некорректный CIDR оператора принят")
	}
}

func Test_Load_WithDatabaseURL_IgnoresDatabaseConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:pass@postgres:45432/managed_valkey")

	if _, err := config.Load(); err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}
}
