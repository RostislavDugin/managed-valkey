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
		config.EnvPublicPort,
		config.EnvOperatorCIDRs,
		config.EnvProbeAddr,
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
	if cfg.PublicPort != config.DefaultPublicPort {
		t.Errorf("публичный порт %d, ожидался %d", cfg.PublicPort, config.DefaultPublicPort)
	}
	if cfg.EnvoyProcesses != config.DefaultEnvoyProcesses {
		t.Errorf("процессов Envoy %d, ожидалось %d", cfg.EnvoyProcesses, config.DefaultEnvoyProcesses)
	}
	if cfg.ProbeAddr != config.DefaultProbeAddr {
		t.Errorf("адрес health probe %q, ожидался %q", cfg.ProbeAddr, config.DefaultProbeAddr)
	}

	if cfg.Logging.ServiceName != config.ServiceName {
		t.Errorf("service.name %q, ожидался %q", cfg.Logging.ServiceName, config.ServiceName)
	}
}

func Test_Load_WithPublicPort_UsesConfiguredValueAndRejectsInvalidValues(t *testing.T) {
	t.Setenv(config.EnvPublicPort, "31379")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузка публичного порта: %v", err)
	}
	if cfg.PublicPort != 31379 {
		t.Errorf("публичный порт %d, ожидался 31379", cfg.PublicPort)
	}

	for _, value := range []string{"0", "65536", "not-a-port"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(config.EnvPublicPort, value)
			if _, err := config.Load(); err == nil {
				t.Fatalf("некорректный порт %q принят", value)
			}
		})
	}
}

func Test_Load_WithProbeAddr_UsesConfiguredLoopbackAddress(t *testing.T) {
	t.Setenv(config.EnvProbeAddr, "127.0.0.1:0")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("загрузка конфигурации: %v", err)
	}
	if cfg.ProbeAddr != "127.0.0.1:0" {
		t.Errorf("адрес health probe %q, ожидался тестовый loopback-адрес", cfg.ProbeAddr)
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
