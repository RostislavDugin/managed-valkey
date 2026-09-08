// Package logging пишет в stdout и, когда задан адрес VictoriaLogs,
// дополнительно экспортирует по OTLP/HTTP. Недоступность экспортёра не
// останавливает приложение.
package logging

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

const (
	EnvLogLevel         = "LOG_LEVEL"
	EnvOTLPURL          = "VL_OTLP_URL"
	EnvAppEnv           = "APP_ENV"
	EnvOTLPUsername     = "VL_OTLP_USERNAME"
	EnvOTLPPasswordFile = "VL_OTLP_PASSWORD_FILE"
)

// Окружение выбирает формат stdout: читаемый текст на dev, JSON на prod.
const (
	EnvironmentDev  = "dev"
	EnvironmentProd = "prod"
)

const (
	AttrServiceName = "service.name"
	AttrEnv         = "env"
)

// Вызывающий передаёт контекст с собственным сроком: недоступный OTLP не
// должен задерживать остановку.
type ShutdownFunc func(ctx context.Context) error

type Config struct {
	ServiceName string
	Environment string
	Level       string

	// Полный адрес /insert/opentelemetry/v1/logs VictoriaLogs; пустое значение
	// отключает экспорт.
	OTLPURL          string
	OTLPUsername     string
	OTLPPasswordFile string
}

func ConfigFromEnv(serviceName string) Config {
	environment := strings.TrimSpace(os.Getenv(EnvAppEnv))
	if environment == "" {
		environment = EnvironmentDev
	}

	return Config{
		ServiceName:      serviceName,
		Environment:      environment,
		Level:            strings.TrimSpace(os.Getenv(EnvLogLevel)),
		OTLPURL:          strings.TrimSpace(os.Getenv(EnvOTLPURL)),
		OTLPUsername:     strings.TrimSpace(os.Getenv(EnvOTLPUsername)),
		OTLPPasswordFile: strings.TrimSpace(os.Getenv(EnvOTLPPasswordFile)),
	}
}

func New(cfg Config) (*slog.Logger, ShutdownFunc) {
	return NewWithWriter(os.Stdout, cfg)
}

// Отдельный конструктор нужен тестам: они читают тот же вывод, что уходит в
// stdout.
func NewWithWriter(out io.Writer, cfg Config) (*slog.Logger, ShutdownFunc) {
	level, levelErr := ParseLevel(cfg.Level)

	handlers := []slog.Handler{newStreamHandler(out, cfg.Environment)}
	shutdown := ShutdownFunc(func(context.Context) error { return nil })

	if cfg.OTLPURL != "" {
		otlpHandler, otlpShutdown, err := newOTLPHandler(cfg)
		switch {
		case err != nil:
			// Ошибка настройки экспортёра остаётся в stdout: приложение
			// продолжает работу без доставки логов в VictoriaLogs.
			slog.New(handlers[0]).Error("не удалось настроить экспорт логов", "error", err)
		default:
			handlers = append(handlers, otlpHandler)
			shutdown = otlpShutdown
		}
	}

	logger := slog.New(newLevelHandler(level, fanout(handlers))).With(
		AttrServiceName, cfg.ServiceName,
		AttrEnv, cfg.Environment,
	)

	if levelErr != nil {
		logger.Warn("неизвестный уровень логов, используется info", "error", levelErr)
	}

	return logger, shutdown
}

// Пустая строка и нераспознанное значение дают info; во втором случае
// возвращается ошибка, чтобы вызывающий сообщил о ней в лог.
func ParseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("неизвестный уровень логов %q", raw)
	}
}

func newStreamHandler(out io.Writer, environment string) slog.Handler {
	opts := &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: utcTime,
	}

	if environment == EnvironmentProd {
		return slog.NewJSONHandler(out, opts)
	}

	return slog.NewTextHandler(out, opts)
}

// Приложения не должны зависеть от часового пояса машины или контейнера.
func utcTime(groups []string, attr slog.Attr) slog.Attr {
	if len(groups) == 0 && attr.Key == slog.TimeKey {
		if value, ok := attr.Value.Any().(time.Time); ok {
			attr.Value = slog.TimeValue(value.UTC())
		}
	}

	return attr
}

func newOTLPHandler(cfg Config) (slog.Handler, ShutdownFunc, error) {
	options, err := otlpOptions(cfg)
	if err != nil {
		return nil, nil, err
	}

	exporter, err := otlploghttp.New(context.Background(), options...)
	if err != nil {
		return nil, nil, errors.New("не удалось создать OTLP-экспортёр")
	}

	res := resource.NewSchemaless(
		attribute.String(AttrServiceName, cfg.ServiceName),
		attribute.String(AttrEnv, cfg.Environment),
	)

	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
	)

	return otelslog.NewHandler(cfg.ServiceName, otelslog.WithLoggerProvider(provider)), provider.Shutdown, nil
}

func otlpOptions(cfg Config) ([]otlploghttp.Option, error) {
	endpoint, err := url.Parse(cfg.OTLPURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("некорректный адрес OTLP")
	}

	options := []otlploghttp.Option{
		otlploghttp.WithEndpointURL(cfg.OTLPURL),
		otlploghttp.WithHTTPClient(&http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}),
	}

	if cfg.OTLPUsername == "" && cfg.OTLPPasswordFile == "" {
		return options, nil
	}

	if endpoint.Scheme != "https" || cfg.OTLPUsername == "" || cfg.OTLPPasswordFile == "" ||
		strings.Contains(cfg.OTLPUsername, ":") {
		return nil, errors.New("авторизация OTLP требует HTTPS, имя пользователя и файл пароля")
	}

	password, err := os.ReadFile(cfg.OTLPPasswordFile)
	if err != nil {
		return nil, errors.New("не удалось прочитать файл пароля OTLP")
	}

	secret := strings.TrimRight(string(password), "\r\n")
	if secret == "" {
		return nil, errors.New("файл пароля OTLP пуст")
	}

	header := "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.OTLPUsername+":"+secret))

	return append(options, otlploghttp.WithHeaders(map[string]string{"Authorization": header})), nil
}
