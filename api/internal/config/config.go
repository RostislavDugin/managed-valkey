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
	ServiceName            = "api"
	ShutdownTimeout        = 15 * time.Second
	SyncInterval           = time.Second
	MetricsCleanupInterval = 24 * time.Hour
	EnvDatabaseURL         = "DATABASE_URL"
	EnvJWTSecret           = "JWT_SECRET"
	EnvHTTPAddr            = "HTTP_ADDR"
	EnvKubernetesSync      = "KUBERNETES_SYNC_ENABLED"
	DefaultHTTPAddr        = ":8080"
	DefaultKubernetesSync  = true

	EnvValkeyBaseDomain                 = "VALKEY_BASE_DOMAIN"
	EnvValkeyPublicPort                 = "VALKEY_PUBLIC_PORT"
	EnvValkeyInstanceMaxVCPU            = "VALKEY_INSTANCE_MAX_VCPU"
	EnvValkeyInstanceMaxRAMGB           = "VALKEY_INSTANCE_MAX_RAM_GB"
	EnvValkeyVCPUPriceCoinsPerHour      = "VALKEY_INSTANCE_VCPU_PRICE_COINS_PER_HOUR"
	EnvValkeyRAMGBPriceCoinsPerHour     = "VALKEY_INSTANCE_RAM_GB_PRICE_COINS_PER_HOUR"
	EnvValkeyMetricsRetention           = "VALKEY_METRICS_RETENTION"
	EnvManagedK8SNodeVCPU               = "MANAGED_K8S_NODE_VCPU"
	EnvManagedK8SNodeRAMGB              = "MANAGED_K8S_NODE_RAM_GB"
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
	DatabaseURL                  string
	JWTSecret                    string
	HTTPAddr                     string
	KubernetesSyncEnabled        bool
	ValkeyBaseDomain             string
	ValkeyPublicPort             int
	ValkeyInstanceMaxVCPU        int
	ValkeyInstanceMaxRAMGB       int
	ValkeyVCPUPriceCoinsPerHour  int64
	ValkeyRAMGBPriceCoinsPerHour int64
	ValkeyMetricsRetention       time.Duration
	ManagedK8SNodeVCPU           int
	ManagedK8SNodeRAMGB          int
	Logging                      logging.Config
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

	kubernetesSyncEnabled, err := boolWithDefault(EnvKubernetesSync, DefaultKubernetesSync)
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

	managedK8SNodeVCPU, err := requiredPositiveInt(EnvManagedK8SNodeVCPU)
	if err != nil {
		return Config{}, err
	}

	managedK8SNodeRAMGB, err := requiredPositiveInt(EnvManagedK8SNodeRAMGB)
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
		DatabaseURL:                  databaseURL,
		JWTSecret:                    jwtSecret,
		HTTPAddr:                     httpAddr,
		KubernetesSyncEnabled:        kubernetesSyncEnabled,
		ValkeyBaseDomain:             valkeyBaseDomain,
		ValkeyPublicPort:             valkeyPublicPort,
		ValkeyInstanceMaxVCPU:        valkeyInstanceMaxVCPU,
		ValkeyInstanceMaxRAMGB:       valkeyInstanceMaxRAMGB,
		ValkeyVCPUPriceCoinsPerHour:  valkeyVCPUPrice,
		ValkeyRAMGBPriceCoinsPerHour: valkeyRAMGBPrice,
		ValkeyMetricsRetention:       valkeyMetricsRetention,
		ManagedK8SNodeVCPU:           managedK8SNodeVCPU,
		ManagedK8SNodeRAMGB:          managedK8SNodeRAMGB,
		Logging:                      logging.ConfigFromEnv(ServiceName),
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
