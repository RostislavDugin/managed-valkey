package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

const testValkeyPassword = "0123456789abcdefghijklmnopqrstuv"

type valkeyListResponse struct {
	Items []valkeydomain.Instance `json:"items"`
}

func Test_RunValkeyLifecycle_WithHttpAndPostgreSql_PersistsStateBillingAuditAndSafeCredentials(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")

	sizes := app.requestJSON(t, http.MethodGet, "/v1/managed/valkey/sizes", nil, bearer(account.Token))
	assertStatus(t, sizes, http.StatusOK)
	catalog := decodeResponse[valkeydomain.Catalog](t, sizes)
	if len(catalog.Items) == 0 || catalog.Pricing.HoursPerMonth != valkeydomain.HoursPerMonth ||
		catalog.Connection.Domain != "valkey.localhost" || catalog.Connection.Port != 41379 {
		t.Fatalf("неожиданный каталог: %+v", catalog)
	}

	empty := app.requestJSON(t, http.MethodGet, "/v1/managed/valkey/instances", nil, bearer(account.Token))
	assertStatus(t, empty, http.StatusOK)
	if items := decodeResponse[valkeyListResponse](t, empty).Items; len(items) != 0 {
		t.Fatalf("непустой начальный список: %+v", items)
	}

	created := createValkey(t, app, account, map[string]any{
		"name": "primary-cache", "prefix": "shop", "mode": "single", "vcpu": 1, "ram_gb": 2,
	})
	if created.Status != "provisioning" || created.DesiredGeneration != 1 || created.ObservedGeneration != 0 ||
		created.AppliedVCPU != 0 || created.AppliedRAMGB != 0 ||
		!created.IsStale || !created.IsUpdating || created.HostRO != nil ||
		created.Host != created.Slug+".valkey.localhost" || created.Port != 41379 {
		t.Fatalf("неожиданное начальное состояние: %+v", created)
	}
	if created.ID.Version() != 7 || created.CreatedAt != created.UpdatedAt ||
		created.CreatedAt != created.ConfigurationRequestedAt {
		t.Fatalf("неожиданные ID или времена: %+v", created)
	}

	var record store.ValkeyInstance
	if err := app.database.DB().Where("id = ?", created.ID).First(&record).Error; err != nil {
		t.Fatalf("прочитать созданный инстанс: %v", err)
	}
	digest := sha256.Sum256([]byte(testValkeyPassword))
	if record.AppPasswordHash != hex.EncodeToString(digest[:]) ||
		strings.Contains(record.AppPasswordHash, testValkeyPassword) {
		t.Fatalf("неожиданный хеш пароля %q", record.AppPasswordHash)
	}

	assertDatabaseCount(t, app.database.DB().Model(&store.BillingPeriod{}).Where("resource_id = ?", created.ID), 1)
	assertDatabaseCount(
		t,
		app.database.DB().Model(&store.AuditLog{}).
			Where("resource_id = ? AND action = ?", created.ID, audit.ActionInstanceCreate),
		1,
	)
	assertDatabaseCount(t, app.database.DB().Model(&store.IdempotencyKey{}).Where("user_id = ?", account.ID), 1)

	secondServer := newHTTPTestAPI(t, testAPIConfig{})
	detail := secondServer.requestJSON(
		t,
		http.MethodGet,
		"/v1/managed/valkey/instances/"+created.ID.String(),
		nil,
		bearer(account.Token),
	)
	assertStatus(t, detail, http.StatusOK)
	fields := decodeResponse[map[string]any](t, detail)
	if _, exists := fields["prefix"]; exists {
		t.Fatalf("ответ содержит сохранённый prefix: %s", detail.Body)
	}
	if _, exists := fields["applied_mode"]; exists {
		t.Fatalf("ответ содержит applied_mode: %s", detail.Body)
	}
	if persistent := decodeResponse[valkeydomain.Instance](t, detail); persistent.ID != created.ID {
		t.Fatalf("другой сервер вернул %+v", persistent)
	}

	credentials := secondServer.requestJSON(
		t,
		http.MethodGet,
		"/v1/managed/valkey/instances/"+created.ID.String()+"/credentials",
		nil,
		bearer(account.Token),
	)
	assertStatus(t, credentials, http.StatusOK)
	credentialsBody := decodeResponse[valkeydomain.Credentials](t, credentials)
	if credentialsBody.Username != "app" || credentialsBody.PasswordHint != testValkeyPassword[:4]+"*****" ||
		containsAny(string(credentials.Body), testValkeyPassword, record.AppPasswordHash) {
		t.Fatalf("небезопасный ответ credentials: %s", credentials.Body)
	}

	me := app.requestJSON(t, http.MethodGet, "/v1/me", nil, bearer(account.Token))
	current := decodeResponse[currentUserResponse](t, me)
	if current.Usage.UsedVCPU != 1 || current.Usage.UsedRAMGB != 2 {
		t.Fatalf("неверный резерв в /v1/me: %+v", current.Usage)
	}

	deleted := app.requestJSON(
		t,
		http.MethodDelete,
		"/v1/managed/valkey/instances/"+created.ID.String(),
		nil,
		bearer(account.Token),
	)
	assertStatus(t, deleted, http.StatusAccepted)
	if len(deleted.Body) != 0 {
		t.Fatalf("DELETE вернул тело %q", deleted.Body)
	}
	me = app.requestJSON(t, http.MethodGet, "/v1/me", nil, bearer(account.Token))
	current = decodeResponse[currentUserResponse](t, me)
	if current.Usage.UsedVCPU != 1 || current.Usage.UsedRAMGB != 2 {
		t.Fatalf("запрошенное удаление освободило резерв: %+v", current.Usage)
	}

	now := time.Now().UTC()
	if err := app.database.DB().Model(&store.ValkeyInstance{}).Where("id = ?", created.ID).
		UpdateColumn("deleted_at", now).Error; err != nil {
		t.Fatalf("подтвердить soft delete: %v", err)
	}
	assertError(
		t,
		app.requestJSON(
			t,
			http.MethodGet,
			"/v1/managed/valkey/instances/"+created.ID.String(),
			nil,
			bearer(account.Token),
		),
		http.StatusNotFound,
		string(apierr.CodeNotFound),
	)
	list := app.requestJSON(t, http.MethodGet, "/v1/managed/valkey/instances", nil, bearer(account.Token))
	if items := decodeResponse[valkeyListResponse](t, list).Items; len(items) != 0 {
		t.Fatalf("soft delete остался в списке: %+v", items)
	}
	assertStatus(
		t,
		app.requestJSON(
			t,
			http.MethodDelete,
			"/v1/managed/valkey/instances/"+created.ID.String(),
			nil,
			bearer(account.Token),
		),
		http.StatusAccepted,
	)
	me = app.requestJSON(t, http.MethodGet, "/v1/me", nil, bearer(account.Token))
	current = decodeResponse[currentUserResponse](t, me)
	if current.Usage.UsedVCPU != 0 || current.Usage.UsedRAMGB != 0 {
		t.Fatalf("soft delete не освободил резерв: %+v", current.Usage)
	}

	assertDatabaseCount(t, app.database.DB().Unscoped().Model(&store.ValkeyInstance{}).Where("id = ?", created.ID), 1)
	assertDatabaseCount(t, app.database.DB().Model(&store.BillingPeriod{}).Where("resource_id = ?", created.ID), 1)
	assertDatabaseCount(t, app.database.DB().Model(&store.AuditLog{}).Where("resource_id = ?", created.ID), 2)
}

func Test_AccessValkeyResources_WithDifferentOwner_ReturnsNotFoundForEveryOperation(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	owner := app.registerAccount(t, "")
	stranger := app.registerAccount(t, "")
	instance := createValkey(t, app, owner, map[string]any{"name": "private-cache"})
	base := "/v1/managed/valkey/instances/" + instance.ID.String()

	tests := []struct {
		method  string
		path    string
		body    any
		headers map[string]string
	}{
		{method: http.MethodGet, path: base},
		{method: http.MethodGet, path: base + "/credentials"},
		{method: http.MethodPatch, path: base, body: map[string]any{"name": "stolen"}},
		{
			method:  http.MethodPost,
			path:    base + "/resize",
			body:    map[string]any{"vcpu": 1, "ram_gb": 2},
			headers: map[string]string{"Idempotency-Key": uuid.NewString()},
		},
		{
			method: http.MethodPut,
			path:   base + "/whitelist",
			body:   map[string]any{"is_whitelist_enabled": false, "whitelist_cidrs": []string{}},
		},
		{
			method:  http.MethodPost,
			path:    base + "/credentials/rotate",
			body:    map[string]any{"password": testValkeyPassword, "expected_password_version": 1},
			headers: map[string]string{"Idempotency-Key": uuid.NewString()},
		},
		{method: http.MethodDelete, path: base},
	}

	for _, testCase := range tests {
		t.Run("пользователь другого аккаунта получает 404 для "+testCase.method+" "+testCase.path, func(t *testing.T) {
			headers := bearer(stranger.Token)
			for key, value := range testCase.headers {
				headers[key] = value
			}
			response := app.requestJSON(t, testCase.method, testCase.path, testCase.body, headers)
			assertError(t, response, http.StatusNotFound, string(apierr.CodeNotFound))
		})
	}
}

func Test_RunValkeyHttpScenario_WithoutKubernetes_CompletesCreateReadRenameQuotaAndDelete(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{clusterVCPU: 1, clusterRAMGB: 1})
	account := app.registerAccount(t, "")

	created := createValkey(t, app, account, map[string]any{
		"name": "e2e-cache", "prefix": "e2e", "vcpu": 1, "ram_gb": 1,
	})
	base := "/v1/managed/valkey/instances/" + created.ID.String()
	read := app.requestJSON(t, http.MethodGet, base, nil, bearer(account.Token))
	assertStatus(t, read, http.StatusOK)
	if got := decodeResponse[valkeydomain.Instance](t, read); got.ID != created.ID {
		t.Fatalf("прочитана другая база: %+v", got)
	}

	renamed := app.requestJSON(
		t,
		http.MethodPatch,
		base,
		map[string]any{"name": "e2e-renamed"},
		bearer(account.Token),
	)
	assertStatus(t, renamed, http.StatusOK)
	if got := decodeResponse[valkeydomain.Instance](t, renamed); got.Name != "e2e-renamed" {
		t.Fatalf("имя не изменилось: %+v", got)
	}

	rejected := createValkeyResponse(t, app, account, "over-budget", "e2e")
	errorBody := assertError(
		t,
		rejected,
		http.StatusUnprocessableEntity,
		string(apierr.CodeNotEnoughResources),
	)
	if errorBody.Error.Details["reason"] != "cluster_quota" {
		t.Fatalf("неверная причина отказа по бюджету: %+v", errorBody)
	}

	assertStatus(t, app.requestJSON(t, http.MethodDelete, base, nil, bearer(account.Token)), http.StatusAccepted)
	if record := loadValkey(t, app, created.ID); record.DeletionRequestedAt == nil {
		t.Fatal("запрос удаления не сохранён")
	}
}

func createValkey(
	t *testing.T,
	app *testAPI,
	account testAccount,
	overrides map[string]any,
) valkeydomain.Instance {
	t.Helper()

	body := map[string]any{
		"name": "cache-" + uuid.NewString()[:8], "prefix": "test", "mode": "single",
		"vcpu": 1, "ram_gb": 1, "password": testValkeyPassword,
		"is_whitelist_enabled": false, "whitelist_cidrs": []string{},
	}
	for key, value := range overrides {
		body[key] = value
	}

	response := app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", body, mergeHeaders(
		bearer(account.Token),
		map[string]string{"Idempotency-Key": uuid.NewString()},
	))
	assertStatus(t, response, http.StatusAccepted)
	if containsAny(string(response.Body), testValkeyPassword, "app_password_hash") {
		t.Fatalf("ответ создания раскрывает секрет: %s", response.Body)
	}

	return decodeResponse[valkeydomain.Instance](t, response)
}

func mergeHeaders(groups ...map[string]string) map[string]string {
	merged := map[string]string{}
	for _, group := range groups {
		for key, value := range group {
			merged[key] = value
		}
	}

	return merged
}
