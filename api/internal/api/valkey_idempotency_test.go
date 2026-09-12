package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

func Test_CreateValkey_WithCanonicalReplayAndExpiredKey_ReplaysOrCreatesAsExpected(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	key := uuid.NewString()
	headers := mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": key})
	firstBody := `{
		"name":"idempotent-cache",
		"prefix":"idem",
		"mode":"single",
		"vcpu":1,
		"ram_gb":1,
		"password":"` + testValkeyPassword + `",
		"is_whitelist_enabled":true,
		"whitelist_cidrs":["192.0.2.1/24","192.0.2.0/24"]
	}`
	secondBody := `{"whitelist_cidrs":["192.0.2.0/24"],"password":"` + testValkeyPassword +
		`","ram_gb":1,"vcpu":1,"mode":"single","prefix":"idem","name":"idempotent-cache","is_whitelist_enabled":true,"maintenance":null}`

	first := app.requestRaw(t, http.MethodPost, "/v1/managed/valkey/instances", firstBody, headers)
	assertStatus(t, first, http.StatusAccepted)
	replayed := app.requestRaw(t, http.MethodPost, "/v1/managed/valkey/instances", secondBody, headers)
	assertStatus(t, replayed, http.StatusAccepted)
	if string(first.Body) != string(replayed.Body) {
		t.Fatalf("канонический повтор изменил ответ: %s != %s", first.Body, replayed.Body)
	}
	created := decodeResponse[valkeydomain.Instance](t, first)
	assertDatabaseCount(t, app.database.DB().Model(&store.ValkeyInstance{}).Where("user_id = ?", account.ID), 1)
	assertDatabaseCount(t, app.database.DB().Model(&store.AuditLog{}).Where(
		"resource_id = ? AND action = ?", created.ID, audit.ActionInstanceCreate,
	), 1)
	assertDatabaseCount(t, app.database.DB().Model(&store.BillingPeriod{}).Where("resource_id = ?", created.ID), 1)

	mismatch := app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", map[string]any{
		"name": "different-cache", "prefix": "idem", "mode": "single", "vcpu": 1, "ram_gb": 1,
		"password": testValkeyPassword,
	}, headers)
	assertError(t, mismatch, http.StatusUnprocessableEntity, string(apierr.CodeIdempotencyMismatch))

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
	afterDelete := app.requestRaw(t, http.MethodPost, "/v1/managed/valkey/instances", secondBody, headers)
	assertStatus(t, afterDelete, http.StatusAccepted)
	if string(afterDelete.Body) != string(first.Body) {
		t.Fatalf("повтор после удаления изменил ответ: %s != %s", afterDelete.Body, first.Body)
	}

	other := app.registerAccount(t, "")
	otherResponse := app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", map[string]any{
		"name": "other-owner", "prefix": "idem", "mode": "single", "vcpu": 1, "ram_gb": 1,
		"password": testValkeyPassword,
	}, mergeHeaders(bearer(other.Token), map[string]string{"Idempotency-Key": key}))
	assertStatus(t, otherResponse, http.StatusAccepted)

	expiringKey := uuid.NewString()
	expiringHeaders := mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": expiringKey})
	expiring := app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", map[string]any{
		"name": "expiring-cache", "prefix": "idem", "mode": "single", "vcpu": 1, "ram_gb": 1,
		"password": testValkeyPassword,
	}, expiringHeaders)
	assertStatus(t, expiring, http.StatusAccepted)
	if err := app.database.DB().Model(&store.IdempotencyKey{}).
		Where("user_id = ? AND key = ?", account.ID, expiringKey).
		UpdateColumn("created_at", time.Now().UTC().Add(-24*time.Hour)).Error; err != nil {
		t.Fatalf("состарить ключ: %v", err)
	}
	newRequest := app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", map[string]any{
		"name": "after-expiry", "prefix": "idem", "mode": "single", "vcpu": 1, "ram_gb": 1,
		"password": testValkeyPassword,
	}, expiringHeaders)
	assertStatus(t, newRequest, http.StatusAccepted)
}

func Test_CreateValkey_WithMaintenance_ReplaysAndPersistsInitialWindow(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	key := uuid.NewString()
	body := map[string]any{
		"name": "maintained-cache", "prefix": "maintained", "mode": "single", "vcpu": 1, "ram_gb": 1,
		"password":    testValkeyPassword,
		"maintenance": map[string]any{"dow": 6, "hour_utc": 23, "duration_min": 1440},
	}
	headers := mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": key})

	first := app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", body, headers)
	assertStatus(t, first, http.StatusAccepted)
	created := decodeResponse[valkeydomain.Instance](t, first)
	if created.Maintenance == nil || created.Maintenance.DOW != 6 || created.Maintenance.HourUTC != 23 ||
		created.Maintenance.DurationMin != 1440 || created.DesiredGeneration != 1 {
		t.Fatalf("начальное окно обслуживания не сохранено: %+v", created)
	}

	replayed := app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", body, headers)
	assertStatus(t, replayed, http.StatusAccepted)
	if string(replayed.Body) != string(first.Body) {
		t.Fatalf("повтор создания изменил ответ: %s != %s", replayed.Body, first.Body)
	}
	record := loadValkey(t, app, created.ID)
	if record.MaintenanceDOW == nil || *record.MaintenanceDOW != 6 || record.MaintenanceHourUTC == nil ||
		*record.MaintenanceHourUTC != 23 || record.MaintenanceDurationMin == nil ||
		*record.MaintenanceDurationMin != 1440 {
		t.Fatalf("окно обслуживания отсутствует в строке инстанса: %+v", record)
	}
	assertDatabaseCount(t, app.database.DB().Model(&store.ValkeyInstance{}).Where("user_id = ?", account.ID), 1)
	assertDatabaseCount(t, app.database.DB().Model(&store.AuditLog{}).Where("resource_id = ?", created.ID), 1)
	assertDatabaseCount(t, app.database.DB().Model(&store.BillingPeriod{}).Where("resource_id = ?", created.ID), 1)
}

func Test_CreateValkey_AfterValidationError_ReusesIdempotencyKeySuccessfully(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	key := uuid.NewString()
	headers := mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": key})

	invalid := app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", map[string]any{
		"name": "retry-cache", "prefix": "retry", "mode": "single", "vcpu": 3, "ram_gb": 3,
		"password": testValkeyPassword,
	}, headers)
	assertError(t, invalid, http.StatusBadRequest, string(apierr.CodeValidationFailed))

	accepted := app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", map[string]any{
		"name": "retry-cache", "prefix": "retry", "mode": "single", "vcpu": 1, "ram_gb": 1,
		"password": testValkeyPassword,
	}, headers)
	assertStatus(t, accepted, http.StatusAccepted)
	assertDatabaseCount(
		t,
		app.database.DB().Model(&store.IdempotencyKey{}).Where("user_id = ? AND key = ?", account.ID, key),
		1,
	)
}

func Test_CreateValkey_WithBlockedOwner_DoesNotReplayIdempotentResponse(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	key := uuid.NewString()
	headers := mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": key})
	body := map[string]any{
		"name": "blocked-cache", "prefix": "blocked", "mode": "single", "vcpu": 1, "ram_gb": 1,
		"password": testValkeyPassword,
	}
	assertStatus(
		t,
		app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", body, headers),
		http.StatusAccepted,
	)
	if err := app.database.DB().Model(&store.User{}).Where("id = ?", account.ID).
		UpdateColumn("is_blocked", true).Error; err != nil {
		t.Fatalf("заблокировать пользователя: %v", err)
	}

	assertError(
		t,
		app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", body, headers),
		http.StatusUnauthorized,
		string(apierr.CodeUnauthorized),
	)
}
