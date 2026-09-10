//go:build integration

package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const maximumHTTPResponseBytes = 1 << 20

type apiClient struct {
	baseURL string
	client  *http.Client
}

type account struct {
	ID    string
	Email string
	Token string
}

type instance struct {
	ID                     string     `json:"id"`
	Name                   string     `json:"name"`
	Slug                   string     `json:"slug"`
	Mode                   string     `json:"mode"`
	VCPU                   int        `json:"vcpu"`
	RAMGB                  int        `json:"ram_gb"`
	AppliedVCPU            int        `json:"applied_vcpu"`
	AppliedRAMGB           int        `json:"applied_ram_gb"`
	Host                   string     `json:"host"`
	Port                   int        `json:"port"`
	PasswordVersion        int        `json:"password_version"`
	AppliedPasswordVersion int        `json:"applied_password_version"`
	Status                 string     `json:"status"`
	DesiredGeneration      int        `json:"desired_generation"`
	ObservedGeneration     int        `json:"observed_generation"`
	ObservedAt             *time.Time `json:"observed_at"`
	IsStale                bool       `json:"is_stale"`
	IsUpdating             bool       `json:"is_updating"`
}

type credentials struct {
	Host                   string `json:"host"`
	Port                   int    `json:"port"`
	Username               string `json:"username"`
	PasswordVersion        int    `json:"password_version"`
	AppliedPasswordVersion int    `json:"applied_password_version"`
}

type currentAccount struct {
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
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

type apiStatusError struct {
	Method     string
	Path       string
	StatusCode int
	Code       string
}

func (err *apiStatusError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d, code=%s", err.Method, err.Path, err.StatusCode, err.Code)
}

func newAPIClient(baseURL string) *apiClient {
	return &apiClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (client *apiClient) Register(ctx context.Context, email, password string) (account, error) {
	var response struct {
		Token string `json:"token"`
	}
	err := client.request(
		ctx,
		http.MethodPost,
		"/v1/auth/register",
		"",
		map[string]string{"email": email, "password": password},
		true,
		&response,
		http.StatusOK,
	)
	if err != nil {
		return account{}, err
	}
	current, err := client.Me(ctx, response.Token)
	if err != nil {
		return account{}, err
	}

	return account{ID: current.User.ID, Email: current.User.Email, Token: response.Token}, nil
}

func (client *apiClient) Me(ctx context.Context, token string) (currentAccount, error) {
	var response currentAccount
	err := client.request(ctx, http.MethodGet, "/v1/me", token, nil, false, &response, http.StatusOK)

	return response, err
}

func (client *apiClient) Create(
	ctx context.Context,
	token string,
	name string,
	prefix string,
	password string,
) (instance, error) {
	var response instance
	err := client.request(
		ctx,
		http.MethodPost,
		"/v1/managed/valkey/instances",
		token,
		map[string]any{
			"name": name, "prefix": prefix, "mode": "single", "vcpu": 1, "ram_gb": 1,
			"password": password, "is_whitelist_enabled": false, "whitelist_cidrs": []string{},
		},
		true,
		&response,
		http.StatusAccepted,
	)

	return response, err
}

func (client *apiClient) Get(ctx context.Context, token, instanceID string) (instance, error) {
	var response instance
	err := client.request(
		ctx,
		http.MethodGet,
		"/v1/managed/valkey/instances/"+instanceID,
		token,
		nil,
		false,
		&response,
		http.StatusOK,
	)

	return response, err
}

func (client *apiClient) List(ctx context.Context, token string) ([]instance, error) {
	var response struct {
		Items []instance `json:"items"`
	}
	err := client.request(
		ctx,
		http.MethodGet,
		"/v1/managed/valkey/instances",
		token,
		nil,
		false,
		&response,
		http.StatusOK,
	)

	return response.Items, err
}

func (client *apiClient) Credentials(ctx context.Context, token, instanceID string) (credentials, error) {
	var response credentials
	err := client.request(
		ctx,
		http.MethodGet,
		"/v1/managed/valkey/instances/"+instanceID+"/credentials",
		token,
		nil,
		false,
		&response,
		http.StatusOK,
	)

	return response, err
}

func (client *apiClient) Resize(
	ctx context.Context,
	token string,
	instanceID string,
	vcpu int,
	ramGB int,
) (instance, error) {
	var response instance
	err := client.request(
		ctx,
		http.MethodPost,
		"/v1/managed/valkey/instances/"+instanceID+"/resize",
		token,
		map[string]int{"vcpu": vcpu, "ram_gb": ramGB},
		true,
		&response,
		http.StatusAccepted,
	)

	return response, err
}

func (client *apiClient) RotatePassword(
	ctx context.Context,
	token string,
	instanceID string,
	password string,
	expectedVersion int,
) (credentials, error) {
	var response credentials
	err := client.request(
		ctx,
		http.MethodPost,
		"/v1/managed/valkey/instances/"+instanceID+"/credentials/rotate",
		token,
		map[string]any{"password": password, "expected_password_version": expectedVersion},
		true,
		&response,
		http.StatusAccepted,
	)

	return response, err
}

func (client *apiClient) Delete(ctx context.Context, token, instanceID string) error {
	return client.request(
		ctx,
		http.MethodDelete,
		"/v1/managed/valkey/instances/"+instanceID,
		token,
		nil,
		false,
		nil,
		http.StatusAccepted,
	)
}

func (client *apiClient) request(
	ctx context.Context,
	method string,
	path string,
	token string,
	body any,
	idempotent bool,
	result any,
	expectedStatus int,
) error {
	var encoded io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("закодировать JSON для %s %s: %w", method, path, err)
		}
		encoded = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, encoded)
	if err != nil {
		return fmt.Errorf("создать запрос %s %s: %w", method, path, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotent {
		request.Header.Set("Idempotency-Key", uuid.NewString())
	}

	response, err := client.client.Do(request)
	if err != nil {
		return fmt.Errorf("выполнить запрос %s %s: %w", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maximumHTTPResponseBytes+1))
	if err != nil {
		return fmt.Errorf("прочитать ответ %s %s: %w", method, path, err)
	}
	if len(responseBody) > maximumHTTPResponseBytes {
		return fmt.Errorf("ответ %s %s превышает допустимый размер", method, path)
	}
	if response.StatusCode != expectedStatus {
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(responseBody, &envelope)

		return &apiStatusError{
			Method: method, Path: path, StatusCode: response.StatusCode, Code: envelope.Error.Code,
		}
	}
	if result == nil || len(bytes.TrimSpace(responseBody)) == 0 {
		return nil
	}
	if err := json.Unmarshal(responseBody, result); err != nil {
		return fmt.Errorf("разобрать ответ %s %s: %w", method, path, err)
	}

	return nil
}

func Test_APIClient_WithLifecycleRequests_SendsHTTPHeadersAndDecodesResponses(t *testing.T) {
	const token = "integration-token"
	const firstPassword = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const secondPassword = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	instanceID := uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/v1/auth/register" && request.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("неверный Authorization для %s %s", request.Method, request.URL.Path)
		}
		if request.Method == http.MethodPost && request.Header.Get("Idempotency-Key") == "" {
			t.Errorf("нет Idempotency-Key для %s", request.URL.Path)
		}
		switch request.Method + " " + request.URL.Path {
		case http.MethodPost + " /v1/auth/register":
			assertRequestJSON(t, request, "password", firstPassword)
			_, _ = writer.Write([]byte(`{"token":"` + token + `"}`))
		case http.MethodGet + " /v1/me":
			_, _ = writer.Write(
				[]byte(
					`{"user":{"id":"user-id","email":"user@example.com"},"quota":{"max_vcpu":4,"max_ram_gb":16},"usage":{"used_vcpu":0,"used_ram_gb":0}}`,
				),
			)
		case http.MethodPost + " /v1/managed/valkey/instances":
			assertRequestJSON(t, request, "password", firstPassword)
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write([]byte(`{"id":"` + instanceID + `","slug":"cache-aaaaaa"}`))
		case http.MethodGet + " /v1/managed/valkey/instances/" + instanceID:
			_, _ = writer.Write([]byte(`{"id":"` + instanceID + `","slug":"cache-aaaaaa"}`))
		case http.MethodGet + " /v1/managed/valkey/instances":
			_, _ = writer.Write([]byte(`{"items":[{"id":"` + instanceID + `","slug":"cache-aaaaaa"}]}`))
		case http.MethodGet + " /v1/managed/valkey/instances/" + instanceID + "/credentials":
			_, _ = writer.Write(
				[]byte(
					`{"host":"cache.example","port":31379,"username":"app","password_version":1,"applied_password_version":1}`,
				),
			)
		case http.MethodPost + " /v1/managed/valkey/instances/" + instanceID + "/resize":
			assertRequestJSON(t, request, "ram_gb", float64(2))
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write([]byte(`{"id":"` + instanceID + `","vcpu":1,"ram_gb":2}`))
		case http.MethodPost + " /v1/managed/valkey/instances/" + instanceID + "/credentials/rotate":
			assertRequestJSON(t, request, "password", secondPassword)
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write(
				[]byte(
					`{"host":"cache.example","port":31379,"username":"app","password_version":2,"applied_password_version":1}`,
				),
			)
		case http.MethodDelete + " /v1/managed/valkey/instances/" + instanceID:
			writer.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	client := newAPIClient(server.URL)
	ctx := context.Background()
	registered, err := client.Register(ctx, "user@example.com", firstPassword)
	if err != nil || registered.ID != "user-id" || registered.Token != token {
		t.Fatalf("регистрация: account=%+v error=%v", registered, err)
	}
	created, err := client.Create(ctx, token, "cache", "cache", firstPassword)
	if err != nil || created.ID != instanceID {
		t.Fatalf("создание: instance=%+v error=%v", created, err)
	}
	if _, err := client.Get(ctx, token, instanceID); err != nil {
		t.Fatalf("чтение карточки: %v", err)
	}
	listed, err := client.List(ctx, token)
	if err != nil || len(listed) != 1 || listed[0].ID != instanceID {
		t.Fatalf("чтение списка: items=%+v error=%v", listed, err)
	}
	if _, err := client.Credentials(ctx, token, instanceID); err != nil {
		t.Fatalf("чтение реквизитов: %v", err)
	}
	if _, err := client.Resize(ctx, token, instanceID, 1, 2); err != nil {
		t.Fatalf("изменение ресурсов: %v", err)
	}
	if _, err := client.RotatePassword(ctx, token, instanceID, secondPassword, 1); err != nil {
		t.Fatalf("ротация пароля: %v", err)
	}
	if err := client.Delete(ctx, token, instanceID); err != nil {
		t.Fatalf("удаление: %v", err)
	}
}

func Test_APIClient_WhenServerReturnsSecretInError_DoesNotIncludeResponseBody(t *testing.T) {
	const secret = "cccccccccccccccccccccccccccccccc"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"error":{"code":"validation_failed","message":"` + secret + `"}}`))
	}))
	t.Cleanup(server.Close)

	_, err := newAPIClient(server.URL).Create(context.Background(), "token", "cache", "cache", secret)
	var statusError *apiStatusError
	if !errors.As(err, &statusError) || statusError.StatusCode != http.StatusBadRequest {
		t.Fatalf("неверная HTTP-ошибка: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("ошибка содержит пароль: %v", err)
	}
}

func assertRequestJSON(t *testing.T, request *http.Request, field string, expected any) {
	t.Helper()

	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatalf("разобрать тело запроса: %v", err)
	}
	if body[field] != expected {
		t.Errorf("поле %s равно %#v, ожидалось %#v", field, body[field], expected)
	}
}
