// Настроек PostgreSQL и адреса HTTP API сервиса здесь нет: оператор к ним не
// обращается.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"

	"github.com/RostislavDugin/managed-valkey/internal/logging"
)

const (
	ServiceName             = "operator"
	ProbeAddr               = ":8081"
	LeaderElection          = true
	MaxConcurrentReconciles = 4
	PrimaryFailureThreshold = 3
	DefaultEnvoyProcesses   = 2
	EnvSystemNamespace      = "VALKEY_SYSTEM_NAMESPACE"
	EnvValkeyImage          = "VALKEY_IMAGE"
	EnvBaseDomain           = "VALKEY_BASE_DOMAIN"
	EnvOperatorCIDRs        = "VALKEY_OPERATOR_CIDRS"
	DefaultSystemNamespace  = "valkey-system"
	DefaultValkeyImage      = "valkey/valkey:8.1.9"
	DefaultBaseDomain       = "valkey.localhost"
)

const LeaderElectionID = "managed-valkey-operator"

type Config struct {
	// SystemNamespace содержит общую инфраструктуру и Lease leader election.
	SystemNamespace string
	ValkeyImage     string
	BaseDomain      string
	OperatorCIDRs   []string
	EnvoyProcesses  int
	Logging         logging.Config
}

func Load() (Config, error) {
	operatorCIDRs, err := parseCIDRs(os.Getenv(EnvOperatorCIDRs))
	if err != nil {
		return Config{}, err
	}
	envoyProcesses, err := configuredEnvoyProcesses()
	if err != nil {
		return Config{}, err
	}

	return Config{
		SystemNamespace: stringOrDefault(EnvSystemNamespace, DefaultSystemNamespace),
		ValkeyImage:     stringOrDefault(EnvValkeyImage, DefaultValkeyImage),
		BaseDomain:      stringOrDefault(EnvBaseDomain, DefaultBaseDomain),
		OperatorCIDRs:   operatorCIDRs,
		EnvoyProcesses:  envoyProcesses,
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
