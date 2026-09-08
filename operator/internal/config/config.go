// Настроек PostgreSQL и адреса HTTP API сервиса здесь нет: оператор к ним не
// обращается.
package config

import (
	"os"
	"strings"
	"time"

	"github.com/RostislavDugin/managed-valkey/internal/logging"
)

const (
	ServiceName                = "operator"
	ProbeAddr                  = ":8081"
	LeaderElection             = true
	MetricsInterval            = 10 * time.Second
	HealthCheckInterval        = 5 * time.Second
	ValkeyDialTimeout          = time.Second
	ValkeyCommandTimeout       = 3 * time.Second
	PrimaryFailureThreshold    = 3
	PrimaryFailureMinDuration  = 10 * time.Second
	ScriptBusyTimeout          = 30 * time.Second
	ProcessUnresponsiveTimeout = 60 * time.Second
	NetworkVerifyInterval      = 10 * time.Second
	NetworkVerifyTimeout       = 3 * time.Second
	ProvisionTimeout           = 10 * time.Minute
	UnschedulableTimeout       = 30 * time.Second
	NodeUnreachableTimeout     = 60 * time.Second
	EmptyPrimaryTimeout        = 2 * time.Minute
	FailoverTimeout            = 5 * time.Second
	EnvSystemNamespace         = "VALKEY_SYSTEM_NAMESPACE"
	EnvValkeyImage             = "VALKEY_IMAGE"
	DefaultSystemNamespace     = "valkey-system"
	DefaultValkeyImage         = "valkey/valkey:8.1.9"
)

const LeaderElectionID = "managed-valkey-operator"

type Config struct {
	// SystemNamespace содержит общую инфраструктуру и Lease leader election.
	SystemNamespace string
	ValkeyImage     string
	Logging         logging.Config
}

func Load() (Config, error) {
	return Config{
		SystemNamespace: stringOrDefault(EnvSystemNamespace, DefaultSystemNamespace),
		ValkeyImage:     stringOrDefault(EnvValkeyImage, DefaultValkeyImage),
		Logging:         logging.ConfigFromEnv(ServiceName),
	}, nil
}

func stringOrDefault(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}

	return value
}
