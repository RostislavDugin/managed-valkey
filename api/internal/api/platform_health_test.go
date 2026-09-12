package api_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/RostislavDugin/managed-valkey/api/internal/api"
	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/platformhealth"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

type platformHealthStub struct {
	report platformhealth.PlatformReport
}

func (s platformHealthStub) Check(context.Context) platformhealth.PlatformReport {
	return s.report
}

type valkeyHealthStub struct {
	report  platformhealth.ValkeyReport
	err     error
	primary string
	read    string
	calls   int
}

func (s *valkeyHealthStub) Check(_ context.Context, primary, read string) (platformhealth.ValkeyReport, error) {
	s.calls++
	s.primary = primary
	s.read = read

	return s.report, s.err
}

func Test_CheckPlatformHealth_WithHttp_ReturnsStatusAndDisablesCaching(t *testing.T) {
	tests := []struct {
		name       string
		status     platformhealth.Status
		httpStatus int
	}{
		{name: "успешное состояние возвращает 200", status: platformhealth.StatusOK, httpStatus: http.StatusOK},
		{name: "предупреждение возвращает 200", status: platformhealth.StatusWarning, httpStatus: http.StatusOK},
		{name: "ошибка возвращает 503", status: platformhealth.StatusFail, httpStatus: http.StatusServiceUnavailable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := healthyPlatformReport()
			report.Status = test.status
			app := newHTTPTestAPI(t, testAPIConfig{platformHealth: platformHealthStub{report: report}})

			response := app.requestJSON(t, http.MethodGet, api.PathHealth, nil, nil)

			assertStatus(t, response, test.httpStatus)
			if response.Header.Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control %q, ожидался no-store", response.Header.Get("Cache-Control"))
			}
			body := decodeResponse[platformhealth.PlatformReport](t, response)
			if body.Status != test.status || body.Checks.PostgreSQL.Status != platformhealth.StatusOK ||
				body.Checks.Kubernetes.Status != platformhealth.StatusOK {
				t.Errorf("неверный ответ: %+v", body)
			}
		})
	}
}

func Test_CheckPlatformHealth_WhenKubernetesIsDisabled_DoesNotChangeReadiness(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})

	healthResponse := app.requestJSON(t, http.MethodGet, api.PathHealth, nil, nil)
	readyResponse := app.requestJSON(t, http.MethodGet, api.PathReady, nil, nil)

	assertStatus(t, healthResponse, http.StatusServiceUnavailable)
	health := decodeResponse[platformhealth.PlatformReport](t, healthResponse)
	if health.Checks.Kubernetes.Status != platformhealth.StatusFail ||
		len(health.Checks.Kubernetes.Issues) != 1 || health.Checks.Kubernetes.Issues[0].Code != "disabled" {
		t.Errorf("неверное состояние выключенного Kubernetes: %+v", health)
	}
	assertStatus(t, readyResponse, http.StatusOK)
}

func Test_CheckValkeyHealth_WithHttp_PassesHeadersAndReturnsBothResults(t *testing.T) {
	service := &valkeyHealthStub{report: platformhealth.ValkeyReport{
		Status:    platformhealth.StatusOK,
		CheckedAt: time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC),
		Checks: platformhealth.ValkeyChecks{
			Primary: platformhealth.ValkeyTargetCheck{Status: platformhealth.StatusOK, LatencyMS: 4},
			Read:    &platformhealth.ValkeyTargetCheck{Status: platformhealth.StatusOK, LatencyMS: 5},
		},
	}}
	app := newHTTPTestAPI(t, testAPIConfig{valkeyHealth: service})
	primary := "rediss://app:primary-secret@cache.valkey.localhost:41379"
	read := "rediss://app:read-secret@cache-ro.valkey.localhost:41379"

	response := app.requestJSON(t, http.MethodGet, api.PathValkeyHealth, nil, map[string]string{
		api.HeaderValkeyPrimary: primary,
		api.HeaderValkeyRead:    read,
	})

	assertStatus(t, response, http.StatusOK)
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q, ожидался no-store", response.Header.Get("Cache-Control"))
	}
	if service.primary != primary || service.read != read {
		t.Errorf("сервис получил другие URI: primary=%q read=%q", service.primary, service.read)
	}
	body := decodeResponse[platformhealth.ValkeyReport](t, response)
	if body.Checks.Read == nil || body.Checks.Read.Status != platformhealth.StatusOK {
		t.Errorf("read отсутствует в ответе: %+v", body)
	}
}

func Test_CheckValkeyHealth_WithInvalidHeaders_ReturnsValidationFailure(t *testing.T) {
	t.Run("обязательный заголовок отсутствует", func(t *testing.T) {
		app := newHTTPTestAPI(t, testAPIConfig{})

		response := app.requestJSON(t, http.MethodGet, api.PathValkeyHealth, nil, nil)

		assertError(t, response, http.StatusBadRequest, string(apierr.CodeValidationFailed))
	})

	t.Run("URI содержит адрес вне управляемого домена", func(t *testing.T) {
		app := newHTTPTestAPI(t, testAPIConfig{})

		response := app.requestJSON(t, http.MethodGet, api.PathValkeyHealth, nil, map[string]string{
			api.HeaderValkeyPrimary: "rediss://app:secret@127.0.0.1:41379",
		})

		assertError(t, response, http.StatusBadRequest, string(apierr.CodeValidationFailed))
	})

	t.Run("primary передан дважды", func(t *testing.T) {
		app := newHTTPTestAPI(t, testAPIConfig{})
		request, err := http.NewRequest(http.MethodGet, app.server.URL+api.PathValkeyHealth, nil)
		if err != nil {
			t.Fatalf("создать запрос: %v", err)
		}
		request.Header.Add(api.HeaderValkeyPrimary, "rediss://app:first@cache.valkey.localhost:41379")
		request.Header.Add(api.HeaderValkeyPrimary, "rediss://app:second@cache.valkey.localhost:41379")

		response, err := app.client.Do(request)
		if err != nil {
			t.Fatalf("выполнить запрос: %v", err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("прочитать ответ: %v", err)
		}

		assertError(
			t,
			testResponse{StatusCode: response.StatusCode, Header: response.Header, Body: body},
			http.StatusBadRequest,
			string(apierr.CodeValidationFailed),
		)
	})

	t.Run("read передан дважды", func(t *testing.T) {
		app := newHTTPTestAPI(t, testAPIConfig{})
		request, err := http.NewRequest(http.MethodGet, app.server.URL+api.PathValkeyHealth, nil)
		if err != nil {
			t.Fatalf("создать запрос: %v", err)
		}
		request.Header.Set(api.HeaderValkeyPrimary, "rediss://app:secret@cache.valkey.localhost:41379")
		request.Header.Add(api.HeaderValkeyRead, "rediss://app:first@cache-ro.valkey.localhost:41379")
		request.Header.Add(api.HeaderValkeyRead, "rediss://app:second@cache-ro.valkey.localhost:41379")

		response, err := app.client.Do(request)
		if err != nil {
			t.Fatalf("выполнить запрос: %v", err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("прочитать ответ: %v", err)
		}

		assertError(
			t,
			testResponse{StatusCode: response.StatusCode, Header: response.Header, Body: body},
			http.StatusBadRequest,
			string(apierr.CodeValidationFailed),
		)
	})

	t.Run("read передан с пустым значением", func(t *testing.T) {
		service := &valkeyHealthStub{}
		app := newHTTPTestAPI(t, testAPIConfig{valkeyHealth: service})

		response := app.requestJSON(t, http.MethodGet, api.PathValkeyHealth, nil, map[string]string{
			api.HeaderValkeyPrimary: "rediss://app:secret@cache.valkey.localhost:41379",
			api.HeaderValkeyRead:    "",
		})

		assertError(t, response, http.StatusBadRequest, string(apierr.CodeValidationFailed))
		if response.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("Cache-Control %q, ожидался no-store", response.Header.Get("Cache-Control"))
		}
		if service.calls != 0 {
			t.Errorf("сервис проверки вызван %d раз", service.calls)
		}
	})
}

func Test_CheckValkeyHealth_WhenProbeFails_DoesNotExposeCredentials(t *testing.T) {
	service := &valkeyHealthStub{report: platformhealth.ValkeyReport{
		Status: platformhealth.StatusFail,
		Checks: platformhealth.ValkeyChecks{
			Primary: platformhealth.ValkeyTargetCheck{
				Status:    platformhealth.StatusFail,
				LatencyMS: 7,
				Code:      "auth_failed",
			},
		},
	}}
	app := newHTTPTestAPI(t, testAPIConfig{valkeyHealth: service})
	secret := "unique-health-secret"
	host := "private-cache.valkey.localhost"

	response := app.requestJSON(t, http.MethodGet, api.PathValkeyHealth, nil, map[string]string{
		api.HeaderValkeyPrimary: "rediss://app:" + secret + "@" + host + ":41379",
	})

	assertStatus(t, response, http.StatusServiceUnavailable)
	output := string(response.Body) + app.logs.String()
	if strings.Contains(output, secret) || strings.Contains(output, host) || strings.Contains(output, "rediss://") {
		t.Errorf("ответ или лог раскрыл URI: %s", output)
	}
	if !strings.Contains(app.logs.String(), `"code":"auth_failed"`) {
		t.Errorf("в логе нет безопасного кода: %s", app.logs.String())
	}
}

func Test_ListValkeyInstancesForHealth_WithActiveAndDeletedRows_ReturnsOnlyActiveState(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "health-store@example.com")
	active := createValkey(t, app, account, map[string]any{"name": "health-active", "prefix": "healthactive"})
	deleted := createValkey(t, app, account, map[string]any{"name": "health-deleted", "prefix": "healthdeleted"})
	now := time.Now().UTC()
	if err := app.database.DB().Model(&store.ValkeyInstance{}).Where("id = ?", active.ID).Updates(map[string]any{
		"desired_generation":       3,
		"observed_generation":      2,
		"password_version":         4,
		"applied_password_version": 3,
		"phase":                    "updating",
		"observed_at":              now,
	}).Error; err != nil {
		t.Fatalf("подготовить активный инстанс: %v", err)
	}
	if err := app.database.DB().Model(&store.ValkeyInstance{}).Where("id = ?", deleted.ID).Updates(map[string]any{
		"deletion_requested_at": now.Add(-time.Second),
		"deleted_at":            now,
	}).Error; err != nil {
		t.Fatalf("пометить инстанс удалённым: %v", err)
	}

	instances, err := app.database.ListValkeyInstancesForHealth(context.Background())
	if err != nil {
		t.Fatalf("прочитать снимок состояния: %v", err)
	}
	stored, found := findHealthInstance(instances, active.ID.String())
	if !found {
		t.Fatalf("активный инстанс отсутствует: %+v", instances)
	}
	if stored.DesiredGeneration != 3 || stored.ObservedGeneration != 2 || stored.PasswordVersion != 4 ||
		stored.AppliedPasswordVersion != 3 || stored.Phase != "updating" || stored.ObservedAt == nil {
		t.Errorf("поля состояния прочитаны неверно: %+v", stored)
	}
	if _, found := findHealthInstance(instances, deleted.ID.String()); found {
		t.Fatal("удалённый инстанс попал в снимок")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := app.database.ListValkeyInstancesForHealth(canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("отменённый запрос вернул ошибку %v", err)
	}
}

func healthyPlatformReport() platformhealth.PlatformReport {
	empty := []platformhealth.Issue{}

	return platformhealth.PlatformReport{
		Status:    platformhealth.StatusOK,
		CheckedAt: time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC),
		Checks: platformhealth.PlatformChecks{
			PostgreSQL: platformhealth.Check{Status: platformhealth.StatusOK, Issues: empty},
			Kubernetes: platformhealth.Check{Status: platformhealth.StatusOK, Issues: empty},
			Operations: platformhealth.Check{Status: platformhealth.StatusOK, Issues: empty},
			Instances:  platformhealth.Check{Status: platformhealth.StatusOK, Issues: empty},
		},
	}
}

func findHealthInstance(instances []store.ValkeyInstance, id string) (store.ValkeyInstance, bool) {
	for _, instance := range instances {
		if instance.ID.String() == id {
			return instance, true
		}
	}

	return store.ValkeyInstance{}, false
}
