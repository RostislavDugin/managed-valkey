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
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	clockutils "k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/RostislavDugin/managed-valkey/api/internal/api"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/auth"
	apiconfig "github.com/RostislavDugin/managed-valkey/api/internal/config"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeysync "github.com/RostislavDugin/managed-valkey/api/internal/sync"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
	"github.com/RostislavDugin/managed-valkey/internal/logging"
)

const testJWTSecret = "http-integration-secret"

type testAPIConfig struct {
	probe                api.Probe
	addRoutes            func(*gin.Engine)
	wrapAuditRepository  func(audit.WriteRepository) audit.WriteRepository
	catalog              *valkeydomain.Catalog
	clusterVCPU          int
	clusterRAMGB         int
	clusterTopology      *valkeydomain.ClusterTopology
	slugGenerator        valkeydomain.SlugGenerator
	wrapDatabaseClock    func(valkeydomain.DatabaseClock) valkeydomain.DatabaseClock
	wrapValkeyRepository func(valkeydomain.Repository) valkeydomain.Repository
}

type testAPI struct {
	server          *httptest.Server
	client          *http.Client
	database        *store.Store
	logs            *synchronizedBuffer
	logger          *slog.Logger
	kubernetes      client.Client
	adminKubernetes client.Client
	syncService     *valkeysync.Service
	syncCancel      context.CancelFunc
	syncRunner      *valkeysync.Runner
	syncStopOnce    sync.Once
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(value)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
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

	logs := &synchronizedBuffer{}
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
	auditRepository := audit.WriteRepository(database)
	if config.wrapAuditRepository != nil {
		auditRepository = config.wrapAuditRepository(auditRepository)
	}
	auditService := audit.NewService(auditRepository, database)
	authService, err := auth.NewService(
		database,
		database,
		auditService,
		auth.NewTokenService(testJWTSecret, clock),
		clock,
	)
	if err != nil {
		t.Fatalf("создать сервис авторизации: %v", err)
	}
	catalog := config.catalog
	if catalog == nil {
		value, catalogErr := valkeydomain.NewCatalog(valkeydomain.CatalogConfig{
			MaxVCPU: 16, MaxRAMGB: 128, VCPUCoinsPerHour: 125, RAMGBCoinsPerHour: 50,
			Domain: "valkey.localhost", Port: 41379,
		})
		if catalogErr != nil {
			t.Fatalf("создать тестовый каталог Valkey: %v", catalogErr)
		}
		catalog = &value
	}
	topology := testClusterTopology(config)
	slugGenerator := config.slugGenerator
	if slugGenerator == nil {
		slugGenerator = valkeydomain.CryptoSlugGenerator{}
	}
	databaseClock := valkeydomain.DatabaseClock(database)
	if config.wrapDatabaseClock != nil {
		databaseClock = config.wrapDatabaseClock(databaseClock)
	}
	valkeyRepository := valkeydomain.Repository(database)
	if config.wrapValkeyRepository != nil {
		valkeyRepository = config.wrapValkeyRepository(valkeyRepository)
	}
	valkeyService := valkeydomain.NewService(
		valkeyRepository,
		database,
		auditService,
		databaseClock,
		clock,
		*catalog,
		topology,
		slugGenerator,
	)
	probe := config.probe
	if probe == nil {
		probe = database
	}
	router, err := api.NewRouter(logger, probe, authService, valkeyService, auditService)
	if err != nil {
		t.Fatalf("создать маршрутизатор: %v", err)
	}
	if config.addRoutes != nil {
		config.addRoutes(router)
	}

	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return &testAPI{server: server, client: server.Client(), database: database, logs: logs, logger: logger}
}

func testClusterTopology(config testAPIConfig) valkeydomain.ClusterTopology {
	if config.clusterTopology != nil {
		return *config.clusterTopology
	}

	clusterVCPU := config.clusterVCPU
	clusterRAMGB := config.clusterRAMGB
	nodeCount := 1
	if clusterVCPU == 0 && clusterRAMGB == 0 {
		clusterVCPU = 1024
		clusterRAMGB = 8192
		nodeCount = 32
	}

	return valkeydomain.ClusterTopology{
		NodeCount:    nodeCount,
		NodeCPUMilli: int64(clusterVCPU * 1000 / nodeCount),
		NodeRAMMiB:   int64(clusterRAMGB * 1024 / nodeCount),
	}
}

func (app *testAPI) startSync(t *testing.T) {
	t.Helper()

	kubernetes, err := valkeysync.NewKubernetesClientFromFile(os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatalf("создать рабочий клиент Kubernetes: %v", err)
	}
	adminKubernetes, err := valkeysync.NewKubernetesClientFromFile(os.Getenv("ADMIN_KUBECONFIG"))
	if err != nil {
		t.Fatalf("создать административный клиент Kubernetes: %v", err)
	}

	service := valkeysync.NewService(
		app.database,
		kubernetes,
		app.logger,
		apiconfig.DefaultValkeyMetricsRetention,
	)
	runner := valkeysync.NewRunner(
		apiconfig.SyncInterval,
		apiconfig.MetricsCleanupInterval,
		clockutils.RealClock{},
		app.logger,
		service.RunDelivery,
		service.RunImport,
		func(ctx context.Context) error {
			return app.database.DeleteExpiredValkeyNodeMetrics(ctx, apiconfig.DefaultValkeyMetricsRetention)
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	runner.Start(ctx)
	t.Cleanup(func() {
		app.stopSync()
	})

	app.kubernetes = kubernetes
	app.adminKubernetes = adminKubernetes
	app.syncService = service
	app.syncCancel = cancel
	app.syncRunner = runner
}

func (app *testAPI) stopSync() {
	app.syncStopOnce.Do(func() {
		app.syncCancel()
		app.syncRunner.Wait()
	})
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
			app.database.DB().Exec(`
				DELETE FROM valkey_node_metrics
				WHERE instance_id IN (SELECT id FROM valkey_instances WHERE user_id = ?)
			`, user.ID),
			app.database.DB().Where("user_id = ?", user.ID).Delete(&store.IdempotencyKey{}),
			app.database.DB().Where("user_id = ?", user.ID).Delete(&store.BillingPeriod{}),
			app.database.DB().Where("user_id = ?", user.ID).Delete(&store.AuditLog{}),
			app.database.DB().Unscoped().Where("user_id = ?", user.ID).Delete(&store.ValkeyInstance{}),
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
	return app.doWithContext(context.Background(), method, path, body, headers)
}

func (app *testAPI) doWithContext(
	ctx context.Context,
	method string,
	path string,
	body []byte,
	headers map[string]string,
) (testResponse, error) {
	request, err := http.NewRequestWithContext(ctx, method, app.server.URL+path, bytes.NewReader(body))
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
