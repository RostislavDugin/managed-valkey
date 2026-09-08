// Настроек PostgreSQL и адреса HTTP API сервиса здесь нет: оператор к ним не
// обращается.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"slices"
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
	EnvBaseDomain              = "VALKEY_BASE_DOMAIN"
	EnvOperatorCIDRs           = "VALKEY_OPERATOR_CIDRS"
	DefaultSystemNamespace     = "valkey-system"
	DefaultValkeyImage         = "valkey/valkey:8.1.9"
	DefaultBaseDomain          = "valkey.localhost"
)

const LeaderElectionID = "managed-valkey-operator"

type Config struct {
	// SystemNamespace содержит общую инфраструктуру и Lease leader election.
	SystemNamespace string
	ValkeyImage     string
	BaseDomain      string
	OperatorCIDRs   []string
	Logging         logging.Config
}

func Load() (Config, error) {
	operatorCIDRs, err := parseCIDRs(os.Getenv(EnvOperatorCIDRs))
	if err != nil {
		return Config{}, err
	}

	return Config{
		SystemNamespace: stringOrDefault(EnvSystemNamespace, DefaultSystemNamespace),
		ValkeyImage:     stringOrDefault(EnvValkeyImage, DefaultValkeyImage),
		BaseDomain:      stringOrDefault(EnvBaseDomain, DefaultBaseDomain),
		OperatorCIDRs:   operatorCIDRs,
		Logging:         logging.ConfigFromEnv(ServiceName),
	}, nil
}

func parseCIDRs(value string) ([]string, error) {
	seen := make(map[netip.Prefix]struct{})
	for item := range strings.SplitSeq(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(item)
		if err != nil {
			return nil, fmt.Errorf("разобрать %s: %w", EnvOperatorCIDRs, err)
		}
		seen[prefix.Masked()] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for prefix := range seen {
		result = append(result, prefix.String())
	}
	slices.Sort(result)

	return result, nil
}

func stringOrDefault(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}

	return value
}
