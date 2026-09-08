package api_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/RostislavDugin/managed-valkey/api/internal/api"
	"github.com/RostislavDugin/managed-valkey/internal/logging"
)

type stubProbe struct {
	err error
}

func (p stubProbe) Ping(context.Context) error {
	return p.err
}

func TestLivezSucceedsWhenDatabaseIsDown(t *testing.T) {
	router, _ := newTestRouter(t, stubProbe{err: errors.New("нет соединения")})

	response := do(router, http.MethodGet, api.PathLive, nil)

	if response.Code != http.StatusOK {
		t.Errorf("код %d, ожидался %d", response.Code, http.StatusOK)
	}
}

func TestReadyzFollowsDatabase(t *testing.T) {
	cases := []struct {
		name  string
		probe stubProbe
		want  int
	}{
		{name: "база доступна", probe: stubProbe{}, want: http.StatusOK},
		{
			name:  "база недоступна",
			probe: stubProbe{err: errors.New("нет соединения")},
			want:  http.StatusServiceUnavailable,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			router, _ := newTestRouter(t, testCase.probe)

			response := do(router, http.MethodGet, api.PathReady, nil)

			if response.Code != testCase.want {
				t.Errorf("код %d, ожидался %d", response.Code, testCase.want)
			}
		})
	}
}

func TestReadyzChecksOnlyPostgreSQL(t *testing.T) {
	probe := &countingProbe{}

	router, _ := newTestRouter(t, probe)

	if response := do(router, http.MethodGet, api.PathReady, nil); response.Code != http.StatusOK {
		t.Fatalf("код %d, ожидался %d", response.Code, http.StatusOK)
	}

	if probe.calls != 1 {
		t.Errorf("проверок базы %d, ожидалась одна", probe.calls)
	}
}

func TestRequestIDIsReturnedAndReused(t *testing.T) {
	router, _ := newTestRouter(t, stubProbe{})

	generated := do(router, http.MethodGet, api.PathLive, nil).Header().Get(api.HeaderRequestID)
	if generated == "" {
		t.Fatal("заголовок с идентификатором запроса пуст")
	}

	request := httptest.NewRequest(http.MethodGet, api.PathLive, nil)
	request.Header.Set(api.HeaderRequestID, "заданный-клиентом")

	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if got := response.Header().Get(api.HeaderRequestID); got != "заданный-клиентом" {
		t.Errorf("идентификатор %q, ожидался заданный клиентом", got)
	}
}

func TestRequestLogKeepsBodyAndSecretHeadersOut(t *testing.T) {
	router, logs := newTestRouter(t, stubProbe{})

	request := httptest.NewRequest(http.MethodGet, api.PathLive, strings.NewReader(`{"password":"тайна-в-теле"}`))
	request.Header.Set("Authorization", "Bearer токен-доступа")
	request.Header.Set("Cookie", "session=значение-сессии")

	router.ServeHTTP(httptest.NewRecorder(), request)

	written := logs.String()

	for _, secret := range []string{"тайна-в-теле", "токен-доступа", "значение-сессии", "Authorization"} {
		if strings.Contains(written, secret) {
			t.Errorf("в логах найдено %q: %s", secret, written)
		}
	}

	if !strings.Contains(written, api.AttrRequestID) {
		t.Errorf("в логах нет %s: %s", api.AttrRequestID, written)
	}
}

func TestRecoveryTurnsPanicIntoInternalError(t *testing.T) {
	router, logs := newTestRouter(t, stubProbe{})
	router.GET("/panic", func(*gin.Context) {
		panic("Authorization: Bearer секрет-из-паники")
	})

	response := do(router, http.MethodGet, "/panic", nil)

	if response.Code != http.StatusInternalServerError {
		t.Errorf("код %d, ожидался %d", response.Code, http.StatusInternalServerError)
	}

	if !strings.Contains(logs.String(), "паника при обработке запроса") {
		t.Errorf("паника не записана в лог: %s", logs.String())
	}

	if strings.Contains(logs.String(), "секрет-из-паники") {
		t.Errorf("значение panic попало в лог: %s", logs.String())
	}
}

func TestClientIPIgnoresForwardedHeader(t *testing.T) {
	router, logs := newTestRouter(t, stubProbe{})
	request := httptest.NewRequest(http.MethodGet, api.PathLive, nil)
	request.Header.Set("X-Forwarded-For", "203.0.113.10")

	router.ServeHTTP(httptest.NewRecorder(), request)

	if strings.Contains(logs.String(), `"client_ip":"203.0.113.10"`) {
		t.Errorf("API принял адрес от недоверенного прокси: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"client_ip":"192.0.2.1"`) {
		t.Errorf("в логе нет адреса прямого клиента: %s", logs.String())
	}
}

type countingProbe struct {
	calls int
}

func (p *countingProbe) Ping(context.Context) error {
	p.calls++

	return nil
}

func newTestRouter(t *testing.T, probe api.Probe) (*gin.Engine, *bytes.Buffer) {
	t.Helper()

	logs := &bytes.Buffer{}

	logger, _ := logging.NewWithWriter(logs, logging.Config{
		ServiceName: "api",
		Environment: logging.EnvironmentProd,
	})

	router, err := api.NewRouter(logger, probe)
	if err != nil {
		t.Fatalf("создать router: %v", err)
	}

	return router, logs
}

func do(router *gin.Engine, method, path string, body *strings.Reader) *httptest.ResponseRecorder {
	var request *http.Request
	if body == nil {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, body)
	}

	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	return response
}
