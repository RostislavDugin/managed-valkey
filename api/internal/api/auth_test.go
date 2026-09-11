package api_test

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/api"
	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/auth"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

type emailExistsResponse struct {
	Exists bool `json:"exists"`
}

func Test_CompleteAuthenticationFlow_WithHttpAndPostgreSql_ReturnsAccountAndSafeLogs(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	email := "  User-" + uuid.NewString() + "@Example.COM "
	normalizedEmail := auth.NormalizeEmail(email)

	missing := app.requestJSON(t, http.MethodPost, "/v1/auth/check-email", map[string]string{"email": email}, nil)
	assertStatus(t, missing, http.StatusOK)
	if decodeResponse[emailExistsResponse](t, missing).Exists {
		t.Fatal("неизвестная почта найдена до регистрации")
	}

	account := app.registerAccount(t, email)
	existing := app.requestJSON(t, http.MethodPost, "/v1/auth/check-email", map[string]string{"email": email}, nil)
	assertStatus(t, existing, http.StatusOK)
	if !decodeResponse[emailExistsResponse](t, existing).Exists {
		t.Fatal("зарегистрированная почта не найдена")
	}

	me := app.requestJSON(t, http.MethodGet, "/v1/me", nil, bearer(account.Token))
	assertStatus(t, me, http.StatusOK)
	current := decodeResponse[currentUserResponse](t, me)
	if current.User.ID != account.ID || current.User.Email != normalizedEmail || current.Quota.MaxVCPU != 4 ||
		current.Quota.MaxRAMGB != 12 || current.Usage.UsedVCPU != 0 || current.Usage.UsedRAMGB != 0 {
		t.Errorf("неожиданный /v1/me: %+v", current)
	}

	login := app.requestJSON(t, http.MethodPost, "/v1/auth/login", map[string]string{
		"email": normalizedEmail, "password": account.Password,
	}, map[string]string{api.HeaderRequestID: "login-over-http"})
	assertStatus(t, login, http.StatusOK)
	loginToken := decodeResponse[tokenResponse](t, login).Token
	if loginToken == "" {
		t.Fatal("вход вернул пустой JWT")
	}
	assertStatus(t, app.requestJSON(t, http.MethodGet, "/v1/me", nil, bearer(loginToken)), http.StatusOK)

	assertDatabaseCount(
		t,
		app.database.DB().Model(&store.AuditLog{}).Where("user_id = ? AND action = ?", account.ID, "user.register"),
		1,
	)
	assertDatabaseCount(
		t,
		app.database.DB().Model(&store.AuditLog{}).Where("user_id = ? AND action = ?", account.ID, "user.login"),
		1,
	)

	writtenLogs := app.logs.String()
	if !strings.Contains(writtenLogs, `"user_id":"`+account.ID.String()+`"`) {
		t.Errorf("в логе нет user_id: %s", writtenLogs)
	}
	if containsAny(writtenLogs, account.Password, account.Token, loginToken, "Authorization") {
		t.Errorf("в лог попали данные авторизации: %s", writtenLogs)
	}
}

func Test_RegisterUser_WithRepeatedIdempotencyKey_ReplaysOnePersistedResult(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	email := "idempotency-" + uuid.NewString() + "@example.com"
	password := "password1"
	key := uuid.NewString()
	app.cleanupUser(t, email)
	body := map[string]string{"email": email, "password": password}
	headers := map[string]string{"Idempotency-Key": key, api.HeaderRequestID: "registration-over-http"}

	first := app.requestJSON(t, http.MethodPost, "/v1/auth/register", body, headers)
	assertStatus(t, first, http.StatusOK)
	firstToken := decodeResponse[tokenResponse](t, first).Token
	firstMe := app.requestJSON(t, http.MethodGet, "/v1/me", nil, bearer(firstToken))
	assertStatus(t, firstMe, http.StatusOK)
	userID := decodeResponse[currentUserResponse](t, firstMe).User.ID

	replayed := app.requestJSON(t, http.MethodPost, "/v1/auth/register", body, headers)
	assertStatus(t, replayed, http.StatusOK)
	replayedToken := decodeResponse[tokenResponse](t, replayed).Token
	assertStatus(t, app.requestJSON(t, http.MethodGet, "/v1/me", nil, bearer(replayedToken)), http.StatusOK)

	mismatch := app.requestJSON(t, http.MethodPost, "/v1/auth/register", map[string]string{
		"email": email, "password": "different-password",
	}, headers)
	assertError(t, mismatch, http.StatusUnprocessableEntity, string(apierr.CodeIdempotencyMismatch))

	conflict := app.requestJSON(t, http.MethodPost, "/v1/auth/register", body, map[string]string{
		"Idempotency-Key": uuid.NewString(),
	})
	assertError(t, conflict, http.StatusConflict, string(apierr.CodeConflict))

	assertDatabaseCount(t, app.database.DB().Model(&store.User{}).Where("id = ?", userID), 1)
	assertDatabaseCount(t, app.database.DB().Model(&store.UserQuota{}).Where("user_id = ?", userID), 1)
	assertDatabaseCount(
		t,
		app.database.DB().Model(&store.AuditLog{}).Where("user_id = ? AND action = ?", userID, "user.register"),
		1,
	)
	assertDatabaseCount(t, app.database.DB().Model(&store.AuthRegistrationKey{}).Where("user_id = ?", userID), 1)
	var audit store.AuditLog
	if err := app.database.DB().
		Where("user_id = ? AND action = ?", userID, "user.register").
		First(&audit).
		Error; err != nil {
		t.Fatalf("прочитать аудит регистрации: %v", err)
	}
	if audit.RequestID != "registration-over-http" {
		t.Errorf("request_id аудита %q, ожидался заголовок HTTP-запроса", audit.RequestID)
	}
}

func Test_RegisterUser_WithExpiredIdempotencyKey_ReturnsConflict(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	email := "expired-" + uuid.NewString() + "@example.com"
	key := uuid.NewString()
	app.cleanupUser(t, email)
	body := map[string]string{"email": email, "password": "password1"}
	headers := map[string]string{"Idempotency-Key": key}

	created := app.requestJSON(t, http.MethodPost, "/v1/auth/register", body, headers)
	assertStatus(t, created, http.StatusOK)
	if err := app.database.DB().Model(&store.AuthRegistrationKey{}).
		Where("key = ?", key).
		Update("created_at", time.Now().UTC().Add(-25*time.Hour)).
		Error; err != nil {
		t.Fatalf("состарить ключ регистрации: %v", err)
	}

	replayed := app.requestJSON(t, http.MethodPost, "/v1/auth/register", body, headers)
	assertError(t, replayed, http.StatusConflict, string(apierr.CodeConflict))
}

func Test_RegisterUser_WithConcurrentRequests_CreatesOneAccountAndReturnsConflict(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	email := "concurrent-" + uuid.NewString() + "@example.com"
	app.cleanupUser(t, email)
	body, err := json.Marshal(map[string]string{"email": email, "password": "password1"})
	if err != nil {
		t.Fatalf("закодировать регистрацию: %v", err)
	}

	start := make(chan struct{})
	results := make(chan testResponse, 2)
	errorsReceived := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			response, requestErr := app.do(http.MethodPost, "/v1/auth/register", body, map[string]string{
				"Idempotency-Key": uuid.NewString(),
			})
			if requestErr != nil {
				errorsReceived <- requestErr

				return
			}
			results <- response
		}()
	}
	close(start)

	statuses := make([]int, 0, 2)
	for range 2 {
		select {
		case requestErr := <-errorsReceived:
			t.Fatalf("выполнить конкурентную регистрацию: %v", requestErr)
		case response := <-results:
			statuses = append(statuses, response.StatusCode)
		}
	}
	sort.Ints(statuses)
	if statuses[0] != http.StatusOK || statuses[1] != http.StatusConflict {
		t.Fatalf("статусы конкурентной регистрации %v, ожидались [200 409]", statuses)
	}

	var user store.User
	if err := app.database.DB().Where("email = ?", email).First(&user).Error; err != nil {
		t.Fatalf("найти зарегистрированного пользователя: %v", err)
	}
	assertDatabaseCount(t, app.database.DB().Model(&store.User{}).Where("email = ?", email), 1)
	assertDatabaseCount(t, app.database.DB().Model(&store.UserQuota{}).Where("user_id = ?", user.ID), 1)
	assertDatabaseCount(
		t,
		app.database.DB().Model(&store.AuditLog{}).Where("user_id = ? AND action = ?", user.ID, "user.register"),
		1,
	)
}

func Test_LoginUser_WithInvalidCredentialsOrBlockedAccount_EnforcesAuthentication(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")

	wrongPassword := app.requestJSON(t, http.MethodPost, "/v1/auth/login", map[string]string{
		"email": account.Email, "password": "wrong-password",
	}, nil)
	wrongError := assertError(t, wrongPassword, http.StatusUnauthorized, string(apierr.CodeUnauthorized))
	unknownUser := app.requestJSON(t, http.MethodPost, "/v1/auth/login", map[string]string{
		"email": "missing-" + account.Email, "password": "wrong-password",
	}, nil)
	unknownError := assertError(t, unknownUser, http.StatusUnauthorized, string(apierr.CodeUnauthorized))
	if wrongError.Error.Message != unknownError.Error.Message {
		t.Errorf("сообщения различаются: %q и %q", wrongError.Error.Message, unknownError.Error.Message)
	}
	assertDatabaseCount(
		t,
		app.database.DB().Model(&store.AuditLog{}).Where("user_id = ? AND action = ?", account.ID, "user.login"),
		0,
	)

	login := app.requestJSON(t, http.MethodPost, "/v1/auth/login", map[string]string{
		"email": account.Email, "password": account.Password,
	}, nil)
	assertStatus(t, login, http.StatusOK)
	assertDatabaseCount(
		t,
		app.database.DB().Model(&store.AuditLog{}).Where("user_id = ? AND action = ?", account.ID, "user.login"),
		1,
	)

	if err := app.database.DB().
		Model(&store.User{}).
		Where("id = ?", account.ID).
		Update("is_blocked", true).
		Error; err != nil {
		t.Fatalf("заблокировать пользователя: %v", err)
	}
	blockedLogin := app.requestJSON(t, http.MethodPost, "/v1/auth/login", map[string]string{
		"email": account.Email, "password": account.Password,
	}, nil)
	assertError(t, blockedLogin, http.StatusUnauthorized, string(apierr.CodeUnauthorized))
	assertError(
		t,
		app.requestJSON(t, http.MethodGet, "/v1/me", nil, bearer(account.Token)),
		http.StatusUnauthorized,
		string(apierr.CodeUnauthorized),
	)
}

func Test_HandleAuthenticationRequest_WithInvalidInputs_ReturnsCommonHttpErrorFormat(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	userID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("создать UUIDv7: %v", err)
	}
	expired := signedHTTPTestToken(t, jwt.SigningMethodHS256, userID, time.Now().UTC().Add(-time.Minute))
	wrongAlgorithm := signedHTTPTestToken(t, jwt.SigningMethodHS384, userID, time.Now().UTC().Add(time.Hour))

	tests := []struct {
		name    string
		method  string
		path    string
		body    string
		headers map[string]string
		status  int
		code    apierr.Code
	}{
		{
			name:   "повреждённый JSON возвращает общую ошибку валидации",
			method: http.MethodPost,
			path:   "/v1/auth/check-email",
			body:   "{",
			status: 400,
			code:   apierr.CodeValidationFailed,
		},
		{
			name:   "недопустимый адрес почты возвращает общую ошибку валидации",
			method: http.MethodPost,
			path:   "/v1/auth/check-email",
			body:   `{"email":"bad"}`,
			status: 400,
			code:   apierr.CodeValidationFailed,
		},
		{
			name:   "отсутствующий ключ регистрации возвращает общую ошибку валидации",
			method: http.MethodPost,
			path:   "/v1/auth/register",
			body:   `{}`,
			status: 400,
			code:   apierr.CodeValidationFailed,
		},
		{
			name:    "повреждённый ключ регистрации возвращает общую ошибку валидации",
			method:  http.MethodPost,
			path:    "/v1/auth/register",
			body:    `{}`,
			headers: map[string]string{"Idempotency-Key": "bad"},
			status:  400,
			code:    apierr.CodeValidationFailed,
		},
		{
			name:   "неверные данные входа возвращают общую ошибку авторизации",
			method: http.MethodPost,
			path:   "/v1/auth/login",
			body:   `{"email":"missing@example.com","password":"password1"}`,
			status: 401,
			code:   apierr.CodeUnauthorized,
		},
		{
			name:   "отсутствующий Bearer-токен возвращает общую ошибку авторизации",
			method: http.MethodGet,
			path:   "/v1/me",
			status: 401,
			code:   apierr.CodeUnauthorized,
		},
		{
			name:    "повреждённый JWT возвращает общую ошибку авторизации",
			method:  http.MethodGet,
			path:    "/v1/me",
			headers: bearer("damaged"),
			status:  401,
			code:    apierr.CodeUnauthorized,
		},
		{
			name:    "просроченный JWT возвращает общую ошибку авторизации",
			method:  http.MethodGet,
			path:    "/v1/me",
			headers: bearer(expired),
			status:  401,
			code:    apierr.CodeUnauthorized,
		},
		{
			name:    "JWT с другим алгоритмом возвращает общую ошибку авторизации",
			method:  http.MethodGet,
			path:    "/v1/me",
			headers: bearer(wrongAlgorithm),
			status:  401,
			code:    apierr.CodeUnauthorized,
		},
		{
			name:   "отсутствующий маршрут возвращает общую ошибку NotFound",
			method: http.MethodGet,
			path:   "/v1/missing",
			status: 404,
			code:   apierr.CodeNotFound,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			response := app.requestRaw(t, testCase.method, testCase.path, testCase.body, testCase.headers)
			assertError(t, response, testCase.status, string(testCase.code))
		})
	}
}

func Test_CallAuthenticationRoutes_WithSameClient_SharesSlidingRateLimit(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})

	for index := range api.AuthRateLimit {
		response := app.requestJSON(
			t,
			http.MethodPost,
			"/v1/auth/check-email",
			map[string]string{"email": "rate-limit@example.com"},
			map[string]string{"X-Forwarded-For": "203.0.113." + string(rune('0'+index))},
		)
		assertStatus(t, response, http.StatusOK)
	}

	limited := app.requestJSON(
		t,
		http.MethodPost,
		"/v1/auth/login",
		map[string]string{"email": "rate-limit@example.com", "password": "password1"},
		nil,
	)
	errorBody := assertError(t, limited, http.StatusTooManyRequests, string(apierr.CodeRateLimited))
	if limited.Header.Get("Retry-After") != "60" || errorBody.Error.Details["retry_after"] != float64(60) {
		t.Errorf("неожиданный срок ожидания: заголовок=%q тело=%v", limited.Header.Get("Retry-After"), errorBody)
	}
}

func signedHTTPTestToken(t *testing.T, method jwt.SigningMethod, userID uuid.UUID, expiresAt time.Time) string {
	t.Helper()

	token := jwt.NewWithClaims(method, jwt.RegisteredClaims{
		Subject: userID.String(), ExpiresAt: jwt.NewNumericDate(expiresAt),
	})
	raw, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("подписать тестовый JWT: %v", err)
	}

	return raw
}
