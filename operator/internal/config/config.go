// Настроек PostgreSQL и адреса HTTP API сервиса здесь нет: оператор к ним не
// обращается.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/RostislavDugin/managed-valkey/internal/logging"
)

const (
	ServiceName                       = "operator"
	DefaultProbeAddr                  = ":8081"
	LeaderElection                    = true
	MaxConcurrentReconciles           = 4
	PrimaryFailureThreshold           = 3
	DefaultEnvoyProcesses             = 2
	EnvSystemNamespace                = "VALKEY_SYSTEM_NAMESPACE"
	EnvValkeyImage                    = "VALKEY_IMAGE"
	EnvBaseDomain                     = "VALKEY_BASE_DOMAIN"
	EnvPublicPort                     = "VALKEY_PUBLIC_PORT"
	EnvOperatorCIDRs                  = "VALKEY_OPERATOR_CIDRS"
	EnvProbeAddr                      = "OPERATOR_PROBE_ADDR"
	EnvIntegrationRequestCPU          = "VALKEY_INTEGRATION_REQUEST_CPU"
	EnvIntegrationRequestMemory       = "VALKEY_INTEGRATION_REQUEST_MEMORY"
	DefaultSystemNamespace            = "valkey-system"
	DefaultValkeyImage                = "valkey/valkey:8.1.9"
	DefaultBaseDomain                 = "valkey.localhost"
	DefaultPublicPort           int32 = 41379
)

const LeaderElectionID = "managed-valkey-operator"

type Config struct {
	// SystemNamespace содержит общую инфраструктуру и Lease leader election.
	SystemNamespace  string
	ValkeyImage      string
	BaseDomain       string
	PublicPort       int32
	OperatorCIDRs    []string
	EnvoyProcesses   int
	ProbeAddr        string
	ResourceRequests corev1.ResourceList
	Logging          logging.Config
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
	publicPort, err := portOrDefault(EnvPublicPort, DefaultPublicPort)
	if err != nil {
		return Config{}, err
	}
	resourceRequests, err := configuredResourceRequests()
	if err != nil {
		return Config{}, err
	}

	return Config{
		SystemNamespace:  stringOrDefault(EnvSystemNamespace, DefaultSystemNamespace),
		ValkeyImage:      stringOrDefault(EnvValkeyImage, DefaultValkeyImage),
		BaseDomain:       stringOrDefault(EnvBaseDomain, DefaultBaseDomain),
		PublicPort:       publicPort,
		OperatorCIDRs:    operatorCIDRs,
		EnvoyProcesses:   envoyProcesses,
		ProbeAddr:        stringOrDefault(EnvProbeAddr, DefaultProbeAddr),
		ResourceRequests: resourceRequests,
		Logging:          logging.ConfigFromEnv(ServiceName),
	}, nil
}

func portOrDefault(name string, fallback int32) (int32, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	port, err := strconv.ParseInt(value, 10, 32)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s должен быть целым числом от 1 до 65535", name)
	}
	return int32(port), nil
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
