package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/api"
	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

type expectedAuditEvent struct {
	email     string
	requestID string
	createdAt time.Time
}

func Test_RunValkeyLifecycle_WithActorChangesAndReplays_PreservesAuditHistoryAndHidesSecrets(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	setUserQuota(t, app, account.ID, 16, 64)
	createKey := uuid.NewString()
	createBody := map[string]any{
		"name": "secure-cache", "prefix": "secure", "mode": "single", "vcpu": 1, "ram_gb": 1,
		"password": testValkeyPassword, "is_whitelist_enabled": false, "whitelist_cidrs": []string{},
	}
	createResponse := app.requestJSON(
		t,
		http.MethodPost,
		"/v1/managed/valkey/instances",
		createBody,
		idempotentRequestHeaders(account.Token, createKey, "create-request"),
	)
	assertStatus(t, createResponse, http.StatusAccepted)
	created := decodeResponse[valkeydomain.Instance](t, createResponse)
	base := instancePath(created.ID)
	responses := []testResponse{createResponse}

	initialRecord := loadValkey(t, app, created.ID)
	initialHash := initialRecord.AppPasswordHash
	if initialRecord.UserEmail != account.Email {
		t.Fatalf("снимок email при создании = %q, ожидался %q", initialRecord.UserEmail, account.Email)
	}
	makeValkeyReady(t, app, created.ID)

	originalEmail := account.Email
	changedEmail := "changed-" + uuid.NewString() + "@example.com"
	if err := app.database.DB().Model(&store.User{}).Where("id = ?", account.ID).
		UpdateColumn("email", changedEmail).Error; err != nil {
		t.Fatalf("изменить email автора: %v", err)
	}
	t.Cleanup(func() {
		if err := app.database.DB().Model(&store.User{}).Where("id = ?", account.ID).
			UpdateColumn("email", originalEmail).Error; err != nil {
			t.Errorf("вернуть email автора: %v", err)
		}
	})

	patchBody := map[string]any{"name": "secure-cache-renamed"}
	patchResponse := app.requestJSON(
		t,
		http.MethodPatch,
		base,
		patchBody,
		requestHeaders(account.Token, "patch-request"),
	)
	assertStatus(t, patchResponse, http.StatusOK)
	patched := decodeResponse[valkeydomain.Instance](t, patchResponse)
	responses = append(responses, patchResponse)

	noOpPatch := app.requestJSON(t, http.MethodPatch, base, patchBody, bearer(account.Token))
	assertStatus(t, noOpPatch, http.StatusOK)
	responses = append(responses, noOpPatch)

	whitelistResponse := app.requestJSON(
		t,
		http.MethodPut,
		base+"/whitelist",
		map[string]any{
			"is_whitelist_enabled": true,
			"whitelist_cidrs":      []string{"192.0.2.1/24"},
		},
		requestHeaders(account.Token, "whitelist-request"),
	)
	assertStatus(t, whitelistResponse, http.StatusAccepted)
	whitelisted := decodeResponse[valkeydomain.Instance](t, whitelistResponse)
	responses = append(responses, whitelistResponse)

	makeValkeyReady(t, app, created.ID)
	resizeKey := uuid.NewString()
	resizeBody := map[string]any{"vcpu": 1, "ram_gb": 2}
	resizeHeaders := idempotentRequestHeaders(account.Token, resizeKey, "resize-request")
	resizeResponse := app.requestJSON(t, http.MethodPost, base+"/resize", resizeBody, resizeHeaders)
	assertStatus(t, resizeResponse, http.StatusAccepted)
	resized := decodeResponse[valkeydomain.Instance](t, resizeResponse)
	resizeReplay := app.requestJSON(t, http.MethodPost, base+"/resize", resizeBody, resizeHeaders)
	assertStatus(t, resizeReplay, http.StatusAccepted)
	responses = append(responses, resizeResponse, resizeReplay)

	makeValkeyReady(t, app, created.ID)
	rotateKey := uuid.NewString()
	rotateBody := map[string]any{
		"password": rotatedValkeyPassword, "expected_password_version": 1,
	}
	rotateHeaders := idempotentRequestHeaders(account.Token, rotateKey, "rotate-request")
	rotateResponse := app.requestJSON(
		t,
		http.MethodPost,
		base+"/credentials/rotate",
		rotateBody,
		rotateHeaders,
	)
	assertStatus(t, rotateResponse, http.StatusAccepted)
	rotateReplay := app.requestJSON(
		t,
		http.MethodPost,
		base+"/credentials/rotate",
		rotateBody,
		rotateHeaders,
	)
	assertStatus(t, rotateReplay, http.StatusAccepted)
	rotatedRecord := loadValkey(t, app, created.ID)
	rotatedHash := rotatedRecord.AppPasswordHash
	responses = append(responses, rotateResponse, rotateReplay)

	responses = append(
		responses,
		app.requestJSON(t, http.MethodGet, "/v1/managed/valkey/instances", nil, bearer(account.Token)),
		app.requestJSON(t, http.MethodGet, base, nil, bearer(account.Token)),
		app.requestJSON(t, http.MethodGet, base+"/credentials", nil, bearer(account.Token)),
	)
	for _, response := range responses[len(responses)-3:] {
		assertStatus(t, response, http.StatusOK)
	}

	deleteResponse := app.requestJSON(
		t,
		http.MethodDelete,
		base,
		nil,
		requestHeaders(account.Token, "delete-request"),
	)
	assertStatus(t, deleteResponse, http.StatusAccepted)
	responses = append(responses, deleteResponse)
	deletedRecord := loadValkey(t, app, created.ID)
	if deletedRecord.UserEmail != originalEmail {
		t.Fatalf("мутации изменили снимок email: %q", deletedRecord.UserEmail)
	}

	deletedAt := time.Now().UTC()
	if err := app.database.DB().Model(&store.ValkeyInstance{}).Where("id = ?", created.ID).
		UpdateColumn("deleted_at", deletedAt).Error; err != nil {
		t.Fatalf("подтвердить soft delete: %v", err)
	}

	createReplay := app.requestJSON(
		t,
		http.MethodPost,
		"/v1/managed/valkey/instances",
		createBody,
		idempotentRequestHeaders(account.Token, createKey, "ignored-create-replay"),
	)
	assertStatus(t, createReplay, http.StatusAccepted)
	resizeAfterDelete := app.requestJSON(t, http.MethodPost, base+"/resize", resizeBody, resizeHeaders)
	assertStatus(t, resizeAfterDelete, http.StatusAccepted)
	rotateAfterDelete := app.requestJSON(
		t,
		http.MethodPost,
		base+"/credentials/rotate",
		rotateBody,
		rotateHeaders,
	)
	assertStatus(t, rotateAfterDelete, http.StatusAccepted)
	responses = append(responses, createReplay, resizeAfterDelete, rotateAfterDelete)

	deletedList := app.requestJSON(t, http.MethodGet, "/v1/managed/valkey/instances", nil, bearer(account.Token))
	assertStatus(t, deletedList, http.StatusOK)
	if items := decodeResponse[valkeyListResponse](t, deletedList).Items; len(items) != 0 {
		t.Fatalf("soft delete остался в списке: %+v", items)
	}
	deletedDetail := app.requestJSON(t, http.MethodGet, base, nil, bearer(account.Token))
	assertError(t, deletedDetail, http.StatusNotFound, string(apierr.CodeNotFound))
	deletedCredentials := app.requestJSON(t, http.MethodGet, base+"/credentials", nil, bearer(account.Token))
	assertError(t, deletedCredentials, http.StatusNotFound, string(apierr.CodeNotFound))
	responses = append(responses, deletedList, deletedDetail, deletedCredentials)

	expected := map[audit.EventAction]expectedAuditEvent{
		audit.ActionInstanceCreate: {
			email: originalEmail, requestID: "create-request", createdAt: created.CreatedAt,
		},
		audit.ActionInstanceUpdate: {
			email: changedEmail, requestID: "patch-request", createdAt: patched.UpdatedAt,
		},
		audit.ActionInstanceWhitelistUpdate: {
			email: changedEmail, requestID: "whitelist-request", createdAt: whitelisted.UpdatedAt,
		},
		audit.ActionInstanceResize: {
			email: changedEmail, requestID: "resize-request", createdAt: resized.UpdatedAt,
		},
		audit.ActionInstancePasswordRotate: {
			email: changedEmail, requestID: "rotate-request", createdAt: rotatedRecord.UpdatedAt,
		},
		audit.ActionInstanceDelete: {
			email: changedEmail, requestID: "delete-request", createdAt: *deletedRecord.DeletionRequestedAt,
		},
	}
	var events []store.AuditLog
	if err := app.database.DB().
		Where("resource_id = ?", created.ID).
		Order("created_at, id").
		Find(&events).
		Error; err != nil {
		t.Fatalf("прочитать аудит Valkey: %v", err)
	}
	if len(events) != len(expected) {
		t.Fatalf("событий аудита %d, ожидалось %d: %+v", len(events), len(expected), events)
	}
	for _, event := range events {
		want, ok := expected[event.Action]
		if !ok {
			t.Fatalf("неожиданное действие аудита %q", event.Action)
		}
		if event.UserID != account.ID || event.UserEmail != want.email || event.RequestID != want.requestID ||
			event.Service == nil || *event.Service != domain.ManagedServiceValkey || event.ResourceID == nil ||
			*event.ResourceID != created.ID || !event.CreatedAt.Equal(want.createdAt) {
			t.Fatalf("неверное событие аудита: got=%+v want=%+v", event, want)
		}
	}

	encodedAudit, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("закодировать аудит: %v", err)
	}
	unsafe := []string{
		testValkeyPassword,
		rotatedValkeyPassword,
		initialHash,
		rotatedHash,
		account.Password,
		account.Token,
	}
	for _, response := range responses {
		if containsAny(string(response.Body), unsafe...) {
			t.Fatalf("HTTP-ответ содержит секрет: %s", response.Body)
		}
		if containsAny(string(response.Body), "user_email", originalEmail, changedEmail) {
			t.Fatalf("HTTP-ответ содержит снимок email: %s", response.Body)
		}
	}
	if containsAny(string(encodedAudit), unsafe...) || containsAny(app.logs.String(), unsafe...) {
		t.Fatalf("аудит или журнал содержит секрет")
	}
	assertDatabaseCount(t, app.database.DB().Unscoped().Model(&store.ValkeyInstance{}).Where("id = ?", created.ID), 1)
	assertDatabaseCount(t, app.database.DB().Model(&store.AuditLog{}).Where("resource_id = ?", created.ID), 6)
	assertDatabaseCount(t, app.database.DB().Model(&store.BillingPeriod{}).Where("resource_id = ?", created.ID), 2)
}

func requestHeaders(token, requestID string) map[string]string {
	return mergeHeaders(bearer(token), map[string]string{api.HeaderRequestID: requestID})
}

func idempotentRequestHeaders(token, key, requestID string) map[string]string {
	return mergeHeaders(
		requestHeaders(token, requestID),
		map[string]string{"Idempotency-Key": key},
	)
}
