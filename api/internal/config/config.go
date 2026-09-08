// Отсутствие обязательного значения останавливает запуск до подключения к
// внешним системам.
package config

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/RostislavDugin/managed-valkey/internal/logging"
)

const (
	ServiceName     = "api"
	ShutdownTimeout = 15 * time.Second
	SyncInterval    = 5 * time.Second
	EnvDatabaseURL  = "DATABASE_URL"
	EnvJWTSecret    = "JWT_SECRET"
	EnvHTTPAddr     = "HTTP_ADDR"
	DefaultHTTPAddr = ":8080"
)

var (
	ErrDatabaseURLRequired = errors.New("переменная " + EnvDatabaseURL + " обязательна")
	ErrJWTSecretRequired   = errors.New("переменная " + EnvJWTSecret + " обязательна")
)

type Config struct {
	DatabaseURL string
	JWTSecret   string
	HTTPAddr    string
	Logging     logging.Config
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

	return Config{
		DatabaseURL: databaseURL,
		JWTSecret:   jwtSecret,
		HTTPAddr:    httpAddr,
		Logging:     logging.ConfigFromEnv(ServiceName),
	}, nil
}
