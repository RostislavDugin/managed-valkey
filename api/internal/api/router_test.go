package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	clockutils "k8s.io/utils/clock"

	"github.com/RostislavDugin/managed-valkey/api/internal/api"
	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	valkeysync "github.com/RostislavDugin/managed-valkey/api/internal/sync"
)

type stubProbe struct {
	err error
}

func (p stubProbe) Ping(context.Context) error {
	return p.err
}

type countingProbe struct {
	calls int
}

func (p *countingProbe) Ping(context.Context) error {
	p.calls++

	return nil
}

func TestHealthUsesHTTP(t *testing.T) {
	t.Run("livez не зависит от PostgreSQL", func(t *testing.T) {
		app := newHTTPTestAPI(t, testAPIConfig{probe: stubProbe{err: errors.New("нет соединения")}})

		response := app.requestJSON(t, http.MethodGet, api.PathLive, nil, nil)

		assertStatus(t, response, http.StatusOK)
	})

	t.Run("readyz использует настоящую PostgreSQL", func(t *testing.T) {
		app := newHTTPTestAPI(t, testAPIConfig{})

		response := app.requestJSON(t, http.MethodGet, api.PathReady, nil, nil)

		assertStatus(t, response, http.StatusOK)
	})

	t.Run("readyz возвращает ошибку зависимости", func(t *testing.T) {
		app := newHTTPTestAPI(t, testAPIConfig{probe: stubProbe{err: errors.New("нет соединения")}})

		response := app.requestJSON(t, http.MethodGet, api.PathReady, nil, nil)

		assertError(t, response, http.StatusServiceUnavailable, string(apierr.CodeUnavailable))
	})

	t.Run("readyz проверяет только PostgreSQL", func(t *testing.T) {
		probe := &countingProbe{}
		app := newHTTPTestAPI(t, testAPIConfig{probe: probe})

		response := app.requestJSON(t, http.MethodGet, api.PathReady, nil, nil)

		assertStatus(t, response, http.StatusOK)
		if probe.calls != 1 {
			t.Errorf("проверок базы %d, ожидалась одна", probe.calls)
		}
	})
}

func TestMetricCleanupFailureDoesNotChangeReadiness(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	cleanupStarted := make(chan struct{})
	blocked := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	runner := valkeysync.NewRunner(
		time.Hour,
		24*time.Hour,
		clockutils.RealClock{},
		app.logger,
		blocked,
		blocked,
		func(context.Context) error {
			close(cleanupStarted)
			return errors.New("cleanup failed")
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	runner.Start(ctx)
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("очистка не началась")
	}

	response := app.requestJSON(t, http.MethodGet, api.PathReady, nil, nil)
	assertStatus(t, response, http.StatusOK)

	cancel()
	runner.Wait()
}

func TestRequestIDUsesHTTPHeaders(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})

	generated := app.requestJSON(t, http.MethodGet, api.PathLive, nil, nil).Header.Get(api.HeaderRequestID)
	if generated == "" {
		t.Fatal("заголовок с идентификатором запроса пуст")
	}

	response := app.requestJSON(t, http.MethodGet, api.PathLive, nil, map[string]string{
		api.HeaderRequestID: "заданный-клиентом",
	})
	if got := response.Header.Get(api.HeaderRequestID); got != "заданный-клиентом" {
		t.Errorf("идентификатор %q, ожидался заданный клиентом", got)
	}
}

func TestRequestLogKeepsHTTPSecretsOut(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})

	response := app.requestRaw(t, http.MethodGet, api.PathLive, `{"password":"тайна-в-теле"}`, map[string]string{
		"Authorization": "Bearer токен-доступа",
		"Cookie":        "session=значение-сессии",
	})
	assertStatus(t, response, http.StatusOK)
	written := app.logs.String()

	if containsAny(written, "тайна-в-теле", "токен-доступа", "значение-сессии", "Authorization") {
		t.Errorf("в лог попали данные запроса: %s", written)
	}
	if !strings.Contains(written, api.AttrRequestID) {
		t.Errorf("в логе нет %s: %s", api.AttrRequestID, written)
	}
}

func TestRecoveryUsesCommonHTTPError(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{addRoutes: func(router *gin.Engine) {
		router.GET("/panic", func(*gin.Context) {
			panic("Authorization: Bearer секрет-из-паники")
		})
	}})

	response := app.requestJSON(t, http.MethodGet, "/panic", nil, nil)

	assertError(t, response, http.StatusInternalServerError, string(apierr.CodeInternal))
	if !strings.Contains(app.logs.String(), "паника при обработке запроса") {
		t.Errorf("паника не записана в лог: %s", app.logs.String())
	}
	if strings.Contains(app.logs.String(), "секрет-из-паники") {
		t.Errorf("значение panic попало в лог: %s", app.logs.String())
	}
}

func TestClientIPIgnoresForwardedHTTPHeader(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})

	response := app.requestJSON(t, http.MethodGet, api.PathLive, nil, map[string]string{
		"X-Forwarded-For": "203.0.113.10",
	})
	assertStatus(t, response, http.StatusOK)
	written := app.logs.String()

	if strings.Contains(written, `"client_ip":"203.0.113.10"`) {
		t.Errorf("API принял адрес из X-Forwarded-For: %s", written)
	}
	if !strings.Contains(written, `"client_ip":"127.0.0.1"`) {
		t.Errorf("в логе нет адреса прямого HTTP-клиента: %s", written)
	}
}
