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
	EnvHTTPAddr     = "HTTP_ADDR"
	DefaultHTTPAddr = ":8080"
)

var ErrDatabaseURLRequired = errors.New("переменная " + EnvDatabaseURL + " обязательна")

type Config struct {
	DatabaseURL string
	HTTPAddr    string
	Logging     logging.Config
}

func Load() (Config, error) {
	databaseURL := strings.TrimSpace(os.Getenv(EnvDatabaseURL))
	if databaseURL == "" {
		return Config{}, ErrDatabaseURLRequired
	}

	httpAddr := strings.TrimSpace(os.Getenv(EnvHTTPAddr))
	if httpAddr == "" {
		httpAddr = DefaultHTTPAddr
	}

	return Config{
		DatabaseURL: databaseURL,
		HTTPAddr:    httpAddr,
		Logging:     logging.ConfigFromEnv(ServiceName),
	}, nil
}
