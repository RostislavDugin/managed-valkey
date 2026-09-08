package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/RostislavDugin/managed-valkey/api/internal/api"
	"github.com/RostislavDugin/managed-valkey/api/internal/auth"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	"github.com/RostislavDugin/managed-valkey/internal/logging"
)

const testJWTSecret = "http-integration-secret"

type testAPIConfig struct {
	probe     api.Probe
	addRoutes func(*gin.Engine)
}

type testAPI struct {
	server   *httptest.Server
	client   *http.Client
	database *store.Store
	logs     *bytes.Buffer
}

type testAccount struct {
	ID       uuid.UUID
	Email    string
	Password string
	Token    string
}

type testResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

type tokenResponse struct {
	Token string `json:"token"`
}

type currentUserResponse struct {
	User struct {
		ID    uuid.UUID `json:"id"`
		Email string    `json:"email"`
	} `json:"user"`
	Quota struct {
		MaxVCPU  int `json:"max_vcpu"`
		MaxRAMGB int `json:"max_ram_gb"`
	} `json:"quota"`
	Usage struct {
		UsedVCPU  int `json:"used_vcpu"`
		UsedRAMGB int `json:"used_ram_gb"`
	} `json:"usage"`
}

type errorResponse struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func newHTTPTestAPI(t *testing.T, config testAPIConfig) *testAPI {
	t.Helper()

	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL не задан")
	}

	logs := &bytes.Buffer{}
	logger, _ := logging.NewWithWriter(logs, logging.Config{
		ServiceName: "api",
		Environment: logging.EnvironmentProd,
	})
	database, err := store.Open(context.Background(), databaseURL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("открыть тестовую базу: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("закрыть тестовую базу: %v", err)
		}
	})

	clock := auth.SystemClock{}
	authService, err := auth.NewService(database, auth.NewTokenService(testJWTSecret, clock), clock)
	if err != nil {
		t.Fatalf("создать сервис авторизации: %v", err)
	}
	probe := config.probe
	if probe == nil {
		probe = database
	}
	router, err := api.NewRouter(logger, probe, authService)
	if err != nil {
		t.Fatalf("создать маршрутизатор: %v", err)
	}
	if config.addRoutes != nil {
		config.addRoutes(router)
	}

	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return &testAPI{server: server, client: server.Client(), database: database, logs: logs}
}

func (app *testAPI) registerAccount(t *testing.T, email string) testAccount {
	t.Helper()

	if email == "" {
		email = "user-" + uuid.NewString() + "@example.com"
	}
	password := "password1"
	normalizedEmail := auth.NormalizeEmail(email)
	app.cleanupUser(t, normalizedEmail)

	response := app.requestJSON(t, http.MethodPost, "/v1/auth/register", map[string]string{
		"email": email, "password": password,
	}, map[string]string{"Idempotency-Key": uuid.NewString()})
	assertStatus(t, response, http.StatusOK)
	token := decodeResponse[tokenResponse](t, response).Token
	if token == "" {
		t.Fatal("регистрация вернула пустой JWT")
	}

	me := app.requestJSON(t, http.MethodGet, "/v1/me", nil, bearer(token))
	assertStatus(t, me, http.StatusOK)
	current := decodeResponse[currentUserResponse](t, me)

	return testAccount{ID: current.User.ID, Email: normalizedEmail, Password: password, Token: token}
}

func (app *testAPI) cleanupUser(t *testing.T, email string) {
	t.Helper()

	t.Cleanup(func() {
		var user store.User
		err := app.database.DB().Where("email = ?", email).First(&user).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return
		}
		if err != nil {
			t.Errorf("найти пользователя для очистки: %v", err)

			return
		}

		queries := []*gorm.DB{
			app.database.DB().Where("user_id = ?", user.ID).Delete(&store.AuditLog{}),
			app.database.DB().Where("user_id = ?", user.ID).Delete(&store.AuthRegistrationKey{}),
			app.database.DB().Where("user_id = ?", user.ID).Delete(&store.UserQuota{}),
			app.database.DB().Where("id = ?", user.ID).Delete(&store.User{}),
		}
		for _, query := range queries {
			if query.Error != nil {
				t.Errorf("очистить данные авторизации: %v", query.Error)
			}
		}
	})
}

func (app *testAPI) requestJSON(
	t *testing.T,
	method string,
	path string,
	body any,
	headers map[string]string,
) testResponse {
	t.Helper()

	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("закодировать JSON запроса: %v", err)
		}
	}

	response, err := app.do(method, path, encoded, headers)
	if err != nil {
		t.Fatalf("выполнить HTTP-запрос: %v", err)
	}

	return response
}

func (app *testAPI) requestRaw(
	t *testing.T,
	method string,
	path string,
	body string,
	headers map[string]string,
) testResponse {
	t.Helper()

	response, err := app.do(method, path, []byte(body), headers)
	if err != nil {
		t.Fatalf("выполнить HTTP-запрос: %v", err)
	}

	return response
}

func (app *testAPI) do(method, path string, body []byte, headers map[string]string) (testResponse, error) {
	request, err := http.NewRequestWithContext(context.Background(), method, app.server.URL+path, bytes.NewReader(body))
	if err != nil {
		return testResponse{}, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := app.client.Do(request)
	if err != nil {
		return testResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return testResponse{}, err
	}

	return testResponse{StatusCode: response.StatusCode, Header: response.Header.Clone(), Body: responseBody}, nil
}

func decodeResponse[T any](t *testing.T, response testResponse) T {
	t.Helper()
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type %q, ожидался application/json", response.Header.Get("Content-Type"))
	}

	var decoded T
	decoder := json.NewDecoder(bytes.NewReader(response.Body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("разобрать JSON-ответ %q: %v", response.Body, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("после JSON-ответа есть лишние данные: %q", response.Body)
	}

	return decoded
}

func assertStatus(t *testing.T, response testResponse, expected int) {
	t.Helper()

	if response.StatusCode != expected {
		t.Fatalf("HTTP-статус %d, ожидался %d, тело: %s", response.StatusCode, expected, response.Body)
	}
}

func assertError(t *testing.T, response testResponse, status int, code string) errorResponse {
	t.Helper()

	assertStatus(t, response, status)
	decoded := decodeResponse[errorResponse](t, response)
	if decoded.Error.Code != code {
		t.Fatalf("код ошибки %q, ожидался %q, тело: %s", decoded.Error.Code, code, response.Body)
	}

	return decoded
}

func assertDatabaseCount(t *testing.T, query *gorm.DB, expected int64) {
	t.Helper()

	var count int64
	if err := query.Count(&count).Error; err != nil {
		t.Fatalf("посчитать строки в тестовой базе: %v", err)
	}
	if count != expected {
		t.Fatalf("строк %d, ожидалось %d", count, expected)
	}
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}

	return false
}
