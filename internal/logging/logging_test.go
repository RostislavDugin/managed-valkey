package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RostislavDugin/managed-valkey/internal/logging"
)

func TestNewWithWriterAddsServiceAndEnv(t *testing.T) {
	var out bytes.Buffer

	logger, shutdown := logging.NewWithWriter(&out, logging.Config{
		ServiceName: "api",
		Environment: logging.EnvironmentProd,
	})
	logger.Info("сообщение")

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("завершение экспорта: %v", err)
	}

	record := decodeSingleRecord(t, out.String())

	if got := record[logging.AttrServiceName]; got != "api" {
		t.Errorf("service.name = %v, ожидался api", got)
	}

	if got := record[logging.AttrEnv]; got != logging.EnvironmentProd {
		t.Errorf("env = %v, ожидался %s", got, logging.EnvironmentProd)
	}
}

func TestNewWithWriterUsesTextOnDev(t *testing.T) {
	var out bytes.Buffer

	logger, _ := logging.NewWithWriter(&out, logging.Config{
		ServiceName: "operator",
		Environment: logging.EnvironmentDev,
	})
	logger.Info("сообщение")

	line := out.String()

	if strings.HasPrefix(strings.TrimSpace(line), "{") {
		t.Fatalf("на dev ожидался текстовый формат, получено %q", line)
	}

	if !strings.Contains(line, "service.name=operator") || !strings.Contains(line, "env=dev") {
		t.Errorf("в записи нет service.name и env: %q", line)
	}
}

func TestNewWithWriterWritesTimeInUTC(t *testing.T) {
	var out bytes.Buffer

	logger, _ := logging.NewWithWriter(&out, logging.Config{
		ServiceName: "api",
		Environment: logging.EnvironmentProd,
	})
	logger.Info("сообщение")

	record := decodeSingleRecord(t, out.String())

	raw, ok := record[slog.TimeKey].(string)
	if !ok {
		t.Fatalf("в записи нет отметки времени: %v", record)
	}

	stamp, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("разобрать время %q: %v", raw, err)
	}

	if _, offset := stamp.Zone(); offset != 0 {
		t.Errorf("смещение времени %d, ожидался UTC", offset)
	}
}

func TestNewWithWriterAppliesLevel(t *testing.T) {
	cases := []struct {
		level     string
		wantDebug bool
		wantWarn  bool
	}{
		{level: "debug", wantDebug: true, wantWarn: true},
		{level: "", wantDebug: false, wantWarn: true},
		{level: "error", wantDebug: false, wantWarn: false},
	}

	for _, testCase := range cases {
		t.Run("level="+testCase.level, func(t *testing.T) {
			var out bytes.Buffer

			logger, _ := logging.NewWithWriter(&out, logging.Config{
				ServiceName: "api",
				Environment: logging.EnvironmentProd,
				Level:       testCase.level,
			})
			logger.Debug("отладка")
			logger.Warn("предупреждение")

			written := out.String()

			if got := strings.Contains(written, "отладка"); got != testCase.wantDebug {
				t.Errorf("запись debug = %v, ожидалось %v", got, testCase.wantDebug)
			}

			if got := strings.Contains(written, "предупреждение"); got != testCase.wantWarn {
				t.Errorf("запись warn = %v, ожидалось %v", got, testCase.wantWarn)
			}
		})
	}
}

func TestNewWithWriterAppliesLevelToOTLP(t *testing.T) {
	var requests atomic.Int32

	receiver := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		response.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	logger, shutdown := logging.NewWithWriter(&bytes.Buffer{}, logging.Config{
		ServiceName: "api",
		Environment: logging.EnvironmentProd,
		Level:       "info",
		OTLPURL:     receiver.URL,
	})
	logger.Debug("не должна экспортироваться")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := shutdown(ctx); err != nil {
		t.Fatalf("завершение экспорта: %v", err)
	}

	if got := requests.Load(); got != 0 {
		t.Errorf("OTLP-запросов %d, ожидалось 0", got)
	}
}

func TestNewWithWriterReportsUnknownLevel(t *testing.T) {
	var out bytes.Buffer

	logger, _ := logging.NewWithWriter(&out, logging.Config{
		ServiceName: "api",
		Environment: logging.EnvironmentProd,
		Level:       "загадочный",
	})
	logger.Info("сообщение")

	if !strings.Contains(out.String(), "неизвестный уровень логов") {
		t.Errorf("предупреждение об уровне не записано: %q", out.String())
	}
}

func TestNewWithWriterWorksWithoutOTLPURL(t *testing.T) {
	var out bytes.Buffer

	logger, shutdown := logging.NewWithWriter(&out, logging.Config{
		ServiceName: "api",
		Environment: logging.EnvironmentProd,
	})
	logger.Info("сообщение")

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("завершение без экспортёра вернуло ошибку: %v", err)
	}

	if strings.Contains(out.String(), "экспорт логов") {
		t.Errorf("без VL_OTLP_URL не должно быть сообщений об экспорте: %q", out.String())
	}
}

func TestNewWithWriterKeepsWorkingWhenExportFails(t *testing.T) {
	var out bytes.Buffer

	logger, shutdown := logging.NewWithWriter(&out, logging.Config{
		ServiceName: "api",
		Environment: logging.EnvironmentProd,
		OTLPURL:     "http://127.0.0.1:1/insert/opentelemetry/v1/logs",
	})
	logger.Info("сообщение")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_ = shutdown(ctx)

	record := decodeSingleRecord(t, out.String())

	if record["msg"] != "сообщение" {
		t.Errorf("запись stdout потеряна: %v", record)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv(logging.EnvAppEnv, logging.EnvironmentProd)
	t.Setenv(logging.EnvLogLevel, "debug")
	t.Setenv(logging.EnvOTLPURL, "http://victorialogs:9428/insert/opentelemetry/v1/logs")

	cfg := logging.ConfigFromEnv("operator")

	want := logging.Config{
		ServiceName: "operator",
		Environment: logging.EnvironmentProd,
		Level:       "debug",
		OTLPURL:     "http://victorialogs:9428/insert/opentelemetry/v1/logs",
	}

	if cfg != want {
		t.Errorf("конфигурация %+v, ожидалась %+v", cfg, want)
	}
}

func TestConfigFromEnvDefaultsToDev(t *testing.T) {
	t.Setenv(logging.EnvAppEnv, "")
	t.Setenv(logging.EnvLogLevel, "")
	t.Setenv(logging.EnvOTLPURL, "")

	cfg := logging.ConfigFromEnv("api")

	if cfg.Environment != logging.EnvironmentDev {
		t.Errorf("окружение %q, ожидалось %q", cfg.Environment, logging.EnvironmentDev)
	}
}

func decodeSingleRecord(t *testing.T, written string) map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(written), "\n")

	record := map[string]any{}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &record); err != nil {
		t.Fatalf("разобрать запись %q: %v", written, err)
	}

	return record
}
