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
	EnvManagedK8SClusterVCPU            = "MANAGED_K8S_CLUSTER_VCPU"
	EnvManagedK8SClusterRAMGB           = "MANAGED_K8S_CLUSTER_RAM_GB"
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
	managedK8SAvailableCPUPercent       = 85
	managedK8SAvailableRAMPercent       = 90
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
	ManagedK8SClusterVCPU              int64
	ManagedK8SClusterRAMGB             int64
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

	managedK8SCapacity, err := loadManagedK8SCapacity()
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
		ManagedK8SClusterVCPU:              managedK8SCapacity.clusterVCPU,
		ManagedK8SClusterRAMGB:             managedK8SCapacity.clusterRAMGB,
		ManagedK8SClusterAvailableCPUMilli: managedK8SCapacity.availableCPUMilli,
		ManagedK8SClusterAvailableRAMMiB:   managedK8SCapacity.availableRAMMiB,
		Logging:                            logging.ConfigFromEnv(ServiceName),
	}, nil
}

type managedK8SCapacity struct {
	clusterVCPU       int64
	clusterRAMGB      int64
	availableCPUMilli int64
	availableRAMMiB   int64
}

func loadManagedK8SCapacity() (managedK8SCapacity, error) {
	deprecated := [...]string{
		EnvManagedK8SNodeCount,
		EnvManagedK8SNodeCapacityVCPU,
		EnvManagedK8SNodeCapacityRAMGB,
		EnvManagedK8SNodeReservedCPUMilli,
		EnvManagedK8SNodeReservedRAMMiB,
		EnvLegacyManagedK8SNodeVCPU,
		EnvLegacyManagedK8SNodeRAMGB,
	}
	for _, name := range deprecated {
		if _, exists := os.LookupEnv(name); exists {
			return managedK8SCapacity{}, fmt.Errorf("переменная %s устарела", name)
		}
	}

	clusterVCPU, err := requiredPositiveInt64(EnvManagedK8SClusterVCPU)
	if err != nil {
		return managedK8SCapacity{}, err
	}
	clusterRAMGB, err := requiredPositiveInt64(EnvManagedK8SClusterRAMGB)
	if err != nil {
		return managedK8SCapacity{}, err
	}

	clusterCPUMilli, ok := multiplyInt64(clusterVCPU, 1000)
	if !ok {
		return managedK8SCapacity{}, fmt.Errorf(
			"переменная %s переполняет расчёт тысячных долей CPU",
			EnvManagedK8SClusterVCPU,
		)
	}
	clusterRAMMiB, ok := multiplyInt64(clusterRAMGB, 1024)
	if !ok {
		return managedK8SCapacity{}, fmt.Errorf("переменная %s переполняет расчёт MiB", EnvManagedK8SClusterRAMGB)
	}
	availableCPUMilli, ok := percentageFloor(clusterCPUMilli, managedK8SAvailableCPUPercent)
	if !ok {
		return managedK8SCapacity{}, fmt.Errorf(
			"переменная %s переполняет расчёт доступного CPU",
			EnvManagedK8SClusterVCPU,
		)
	}
	availableRAMMiB, ok := percentageFloor(clusterRAMMiB, managedK8SAvailableRAMPercent)
	if !ok {
		return managedK8SCapacity{}, fmt.Errorf(
			"переменная %s переполняет расчёт доступной RAM",
			EnvManagedK8SClusterRAMGB,
		)
	}

	return managedK8SCapacity{
		clusterVCPU:       clusterVCPU,
		clusterRAMGB:      clusterRAMGB,
		availableCPUMilli: availableCPUMilli,
		availableRAMMiB:   availableRAMMiB,
	}, nil
}

func percentageFloor(value, percentage int64) (int64, bool) {
	weighted, ok := multiplyInt64(value, percentage)
	if !ok {
		return 0, false
	}

	return weighted / 100, true
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
