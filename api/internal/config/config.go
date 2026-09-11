// Отсутствие обязательного значения останавливает запуск до подключения к
// внешним системам.
package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RostislavDugin/managed-valkey/internal/logging"
)

const (
	ServiceName                     = "api"
	ShutdownTimeout                 = 15 * time.Second
	SyncInterval                    = time.Second
	MetricsCleanupInterval          = 24 * time.Hour
	EnvDatabaseURL                  = "DATABASE_URL"
	EnvJWTSecret                    = "JWT_SECRET"
	EnvHTTPAddr                     = "HTTP_ADDR"
	EnvKubernetesBackgroundSync     = "KUBERNETES_BACKGROUND_SYNC_ENABLED"
	DefaultHTTPAddr                 = ":8080"
	DefaultKubernetesBackgroundSync = true

	EnvValkeyBaseDomain                 = "VALKEY_BASE_DOMAIN"
	EnvValkeyPublicPort                 = "VALKEY_PUBLIC_PORT"
	EnvValkeyInstanceMaxVCPU            = "VALKEY_INSTANCE_MAX_VCPU"
	EnvValkeyInstanceMaxRAMGB           = "VALKEY_INSTANCE_MAX_RAM_GB"
	EnvValkeyVCPUPriceCoinsPerHour      = "VALKEY_INSTANCE_VCPU_PRICE_COINS_PER_HOUR"
	EnvValkeyRAMGBPriceCoinsPerHour     = "VALKEY_INSTANCE_RAM_GB_PRICE_COINS_PER_HOUR"
	EnvValkeyMetricsRetention           = "VALKEY_METRICS_RETENTION"
	EnvManagedK8SNodeCount              = "MANAGED_K8S_NODE_COUNT"
	EnvManagedK8SNodeCapacityVCPU       = "MANAGED_K8S_NODE_CAPACITY_VCPU"
	EnvManagedK8SNodeCapacityRAMGB      = "MANAGED_K8S_NODE_CAPACITY_RAM_GB"
	EnvManagedK8SNodeReservedCPUMilli   = "MANAGED_K8S_NODE_RESERVED_CPU_MILLI"
	EnvManagedK8SNodeReservedRAMMiB     = "MANAGED_K8S_NODE_RESERVED_RAM_MIB"
	EnvLegacyManagedK8SNodeVCPU         = "MANAGED_K8S_NODE_VCPU"
	EnvLegacyManagedK8SNodeRAMGB        = "MANAGED_K8S_NODE_RAM_GB"
	DefaultValkeyPublicPort             = 41379
	DefaultValkeyVCPUPriceCoinsPerHour  = 125
	DefaultValkeyRAMGBPriceCoinsPerHour = 50
	DefaultValkeyMetricsRetention       = 168 * time.Hour
	MaxValkeyNodes                      = 3
)

var (
	ErrDatabaseURLRequired = errors.New("переменная " + EnvDatabaseURL + " обязательна")
	ErrJWTSecretRequired   = errors.New("переменная " + EnvJWTSecret + " обязательна")
)

type Config struct {
	DatabaseURL                        string
	JWTSecret                          string
	HTTPAddr                           string
	KubernetesBackgroundSyncEnabled    bool
	ValkeyBaseDomain                   string
	ValkeyPublicPort                   int
	ValkeyInstanceMaxVCPU              int
	ValkeyInstanceMaxRAMGB             int
	ValkeyVCPUPriceCoinsPerHour        int64
	ValkeyRAMGBPriceCoinsPerHour       int64
	ValkeyMetricsRetention             time.Duration
	ManagedK8SNodeCount                int
	ManagedK8SNodeCapacityCPUMilli     int64
	ManagedK8SNodeCapacityRAMMiB       int64
	ManagedK8SNodeReservedCPUMilli     int64
	ManagedK8SNodeReservedRAMMiB       int64
	ManagedK8SNodeAvailableCPUMilli    int64
	ManagedK8SNodeAvailableRAMMiB      int64
	ManagedK8SClusterAvailableCPUMilli int64
	ManagedK8SClusterAvailableRAMMiB   int64
	Logging                            logging.Config
}

func Load() (Config, error) {
	databaseURL := strings.TrimSpace(os.Getenv(EnvDatabaseURL))
	if databaseURL == "" {
		return Config{}, ErrDatabaseURLRequired
	}

	jwtSecret := strings.TrimSpace(os.Getenv(EnvJWTSecret))
	if jwtSecret == "" {
		return Config{}, ErrJWTSecretRequired
	}

	httpAddr := strings.TrimSpace(os.Getenv(EnvHTTPAddr))
	if httpAddr == "" {
		httpAddr = DefaultHTTPAddr
	}

	kubernetesBackgroundSyncEnabled, err := boolWithDefault(
		EnvKubernetesBackgroundSync,
		DefaultKubernetesBackgroundSync,
	)
	if err != nil {
		return Config{}, err
	}

	valkeyBaseDomain := strings.TrimSpace(os.Getenv(EnvValkeyBaseDomain))
	if !validDomain(valkeyBaseDomain) {
		return Config{}, fmt.Errorf("переменная %s должна содержать допустимый домен", EnvValkeyBaseDomain)
	}

	valkeyPublicPort, err := positiveIntWithDefault(EnvValkeyPublicPort, DefaultValkeyPublicPort)
	if err != nil {
		return Config{}, err
	}
	if valkeyPublicPort > 65535 {
		return Config{}, fmt.Errorf("переменная %s должна быть не больше 65535", EnvValkeyPublicPort)
	}

	valkeyInstanceMaxVCPU, err := requiredPositiveInt(EnvValkeyInstanceMaxVCPU)
	if err != nil {
		return Config{}, err
	}

	valkeyInstanceMaxRAMGB, err := requiredPositiveInt(EnvValkeyInstanceMaxRAMGB)
	if err != nil {
		return Config{}, err
	}

	managedK8STopology, err := loadManagedK8STopology()
	if err != nil {
		return Config{}, err
	}

	valkeyVCPUPrice, err := nonnegativeInt64WithDefault(
		EnvValkeyVCPUPriceCoinsPerHour,
		DefaultValkeyVCPUPriceCoinsPerHour,
	)
	if err != nil {
		return Config{}, err
	}

	valkeyRAMGBPrice, err := nonnegativeInt64WithDefault(
		EnvValkeyRAMGBPriceCoinsPerHour,
		DefaultValkeyRAMGBPriceCoinsPerHour,
	)
	if err != nil {
		return Config{}, err
	}

	valkeyMetricsRetention, err := positiveDurationWithDefault(
		EnvValkeyMetricsRetention,
		DefaultValkeyMetricsRetention,
	)
	if err != nil {
		return Config{}, err
	}

	if priceOverflows(valkeyVCPUPrice, valkeyRAMGBPrice, valkeyInstanceMaxVCPU, valkeyInstanceMaxRAMGB) {
		return Config{}, errors.New("ставки Valkey переполняют целочисленный расчёт цены")
	}

	return Config{
		DatabaseURL:                        databaseURL,
		JWTSecret:                          jwtSecret,
		HTTPAddr:                           httpAddr,
		KubernetesBackgroundSyncEnabled:    kubernetesBackgroundSyncEnabled,
		ValkeyBaseDomain:                   valkeyBaseDomain,
		ValkeyPublicPort:                   valkeyPublicPort,
		ValkeyInstanceMaxVCPU:              valkeyInstanceMaxVCPU,
		ValkeyInstanceMaxRAMGB:             valkeyInstanceMaxRAMGB,
		ValkeyVCPUPriceCoinsPerHour:        valkeyVCPUPrice,
		ValkeyRAMGBPriceCoinsPerHour:       valkeyRAMGBPrice,
		ValkeyMetricsRetention:             valkeyMetricsRetention,
		ManagedK8SNodeCount:                managedK8STopology.nodeCount,
		ManagedK8SNodeCapacityCPUMilli:     managedK8STopology.nodeCapacityCPUMilli,
		ManagedK8SNodeCapacityRAMMiB:       managedK8STopology.nodeCapacityRAMMiB,
		ManagedK8SNodeReservedCPUMilli:     managedK8STopology.nodeReservedCPUMilli,
		ManagedK8SNodeReservedRAMMiB:       managedK8STopology.nodeReservedRAMMiB,
		ManagedK8SNodeAvailableCPUMilli:    managedK8STopology.nodeAvailableCPUMilli,
		ManagedK8SNodeAvailableRAMMiB:      managedK8STopology.nodeAvailableRAMMiB,
		ManagedK8SClusterAvailableCPUMilli: managedK8STopology.clusterAvailableCPUMilli,
		ManagedK8SClusterAvailableRAMMiB:   managedK8STopology.clusterAvailableRAMMiB,
		Logging:                            logging.ConfigFromEnv(ServiceName),
	}, nil
}

type managedK8STopology struct {
	nodeCount                int
	nodeCapacityCPUMilli     int64
	nodeCapacityRAMMiB       int64
	nodeReservedCPUMilli     int64
	nodeReservedRAMMiB       int64
	nodeAvailableCPUMilli    int64
	nodeAvailableRAMMiB      int64
	clusterAvailableCPUMilli int64
	clusterAvailableRAMMiB   int64
}

func loadManagedK8STopology() (managedK8STopology, error) {
	for _, legacy := range []string{EnvLegacyManagedK8SNodeVCPU, EnvLegacyManagedK8SNodeRAMGB} {
		if strings.TrimSpace(os.Getenv(legacy)) != "" {
			return managedK8STopology{}, fmt.Errorf("переменная %s устарела", legacy)
		}
	}

	nodeCount, err := requiredPositiveInt(EnvManagedK8SNodeCount)
	if err != nil {
		return managedK8STopology{}, err
	}
	nodeCapacityVCPU, err := requiredPositiveInt64(EnvManagedK8SNodeCapacityVCPU)
	if err != nil {
		return managedK8STopology{}, err
	}
	nodeCapacityRAMGB, err := requiredPositiveInt64(EnvManagedK8SNodeCapacityRAMGB)
	if err != nil {
		return managedK8STopology{}, err
	}
	nodeReservedCPUMilli, err := requiredNonnegativeInt64(EnvManagedK8SNodeReservedCPUMilli)
	if err != nil {
		return managedK8STopology{}, err
	}
	nodeReservedRAMMiB, err := requiredNonnegativeInt64(EnvManagedK8SNodeReservedRAMMiB)
	if err != nil {
		return managedK8STopology{}, err
	}

	nodeCapacityCPUMilli, ok := multiplyInt64(nodeCapacityVCPU, 1000)
	if !ok {
		return managedK8STopology{}, fmt.Errorf(
			"переменная %s переполняет расчёт тысячных долей CPU",
			EnvManagedK8SNodeCapacityVCPU,
		)
	}
	nodeCapacityRAMMiB, ok := multiplyInt64(nodeCapacityRAMGB, 1024)
	if !ok {
		return managedK8STopology{}, fmt.Errorf("переменная %s переполняет расчёт MiB", EnvManagedK8SNodeCapacityRAMGB)
	}
	if nodeReservedCPUMilli >= nodeCapacityCPUMilli {
		return managedK8STopology{}, fmt.Errorf(
			"переменная %s должна оставлять положительный бюджет CPU",
			EnvManagedK8SNodeReservedCPUMilli,
		)
	}
	if nodeReservedRAMMiB >= nodeCapacityRAMMiB {
		return managedK8STopology{}, fmt.Errorf(
			"переменная %s должна оставлять положительный бюджет RAM",
			EnvManagedK8SNodeReservedRAMMiB,
		)
	}

	nodeAvailableCPUMilli := nodeCapacityCPUMilli - nodeReservedCPUMilli
	nodeAvailableRAMMiB := nodeCapacityRAMMiB - nodeReservedRAMMiB
	clusterAvailableCPUMilli, ok := multiplyInt64(nodeAvailableCPUMilli, int64(nodeCount))
	if !ok {
		return managedK8STopology{}, fmt.Errorf("переменная %s переполняет общий бюджет CPU", EnvManagedK8SNodeCount)
	}
	clusterAvailableRAMMiB, ok := multiplyInt64(nodeAvailableRAMMiB, int64(nodeCount))
	if !ok {
		return managedK8STopology{}, fmt.Errorf("переменная %s переполняет общий бюджет RAM", EnvManagedK8SNodeCount)
	}

	return managedK8STopology{
		nodeCount:                nodeCount,
		nodeCapacityCPUMilli:     nodeCapacityCPUMilli,
		nodeCapacityRAMMiB:       nodeCapacityRAMMiB,
		nodeReservedCPUMilli:     nodeReservedCPUMilli,
		nodeReservedRAMMiB:       nodeReservedRAMMiB,
		nodeAvailableCPUMilli:    nodeAvailableCPUMilli,
		nodeAvailableRAMMiB:      nodeAvailableRAMMiB,
		clusterAvailableCPUMilli: clusterAvailableCPUMilli,
		clusterAvailableRAMMiB:   clusterAvailableRAMMiB,
	}, nil
}

var domainPattern = regexp.MustCompile(
	`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`,
)

func validDomain(value string) bool {
	return value != "" && len(value) <= 253 && domainPattern.MatchString(value)
}

func requiredPositiveInt(name string) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("переменная %s должна быть положительным целым числом", name)
	}

	return parsed, nil
}

func requiredPositiveInt64(name string) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("переменная %s должна быть положительным целым числом", name)
	}

	return parsed, nil
}

func requiredNonnegativeInt64(name string) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("переменная %s должна быть неотрицательным целым числом", name)
	}

	return parsed, nil
}

func multiplyInt64(left, right int64) (int64, bool) {
	if left != 0 && right > math.MaxInt64/left {
		return 0, false
	}

	return left * right, true
}

func positiveIntWithDefault(name string, defaultValue int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return defaultValue, nil
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("переменная %s должна быть положительным целым числом", name)
	}

	return parsed, nil
}

func nonnegativeInt64WithDefault(name string, defaultValue int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return defaultValue, nil
	}

	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("переменная %s должна быть неотрицательным целым числом", name)
	}

	return parsed, nil
}

func positiveDurationWithDefault(name string, defaultValue time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return defaultValue, nil
	}

	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("переменная %s должна быть положительной длительностью", name)
	}

	return parsed, nil
}

func boolWithDefault(name string, defaultValue bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return defaultValue, nil
	}

	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("переменная %s должна быть логическим значением: %w", name, err)
	}

	return parsed, nil
}

func priceOverflows(vcpuRate, ramRate int64, maxVCPU, maxRAMGB int) bool {
	if vcpuRate > math.MaxInt64/int64(maxVCPU) || ramRate > math.MaxInt64/int64(maxRAMGB) {
		return true
	}

	perNode := vcpuRate*int64(maxVCPU) + ramRate*int64(maxRAMGB)
	return perNode < 0 || perNode > math.MaxInt64/MaxValkeyNodes
}
