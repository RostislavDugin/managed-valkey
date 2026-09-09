package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

func TestValkeyAuditPagesUseHTTPAndPostgreSQL(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	owner := app.registerAccount(t, "")
	stranger := app.registerAccount(t, "")
	instance := createValkey(t, app, owner, map[string]any{"name": "audited-cache"})
	strangerInstance := createValkey(t, app, stranger, map[string]any{"name": "other-cache"})
	createdAt := time.Date(2026, time.September, 9, 10, 11, 12, 345678000, time.UTC)
	service := domain.ManagedServiceValkey
	otherService := domain.ManagedService("other")

	replaceAuditLogs(t, app, instance.ID, []store.AuditLog{
		auditRecord(
			owner,
			instance.ID,
			service,
			"01993000-0000-7000-8000-000000000001",
			audit.ActionInstanceCreate,
			createdAt,
		),
		auditRecord(
			owner,
			instance.ID,
			service,
			"01993000-0000-7000-8000-000000000002",
			audit.ActionInstanceUpdate,
			createdAt,
		),
		auditRecord(
			owner,
			instance.ID,
			service,
			"01993000-0000-7000-8000-000000000003",
			audit.ActionInstanceResize,
			createdAt,
		),
		auditRecord(
			owner,
			instance.ID,
			service,
			"01993000-0000-7000-8000-000000000004",
			audit.ActionInstanceWhitelistUpdate,
			createdAt,
		),
		auditRecord(
			owner,
			instance.ID,
			otherService,
			"01993000-0000-7000-8000-000000000005",
			audit.ActionInstanceDelete,
			createdAt,
		),
	})

	base := "/v1/managed/valkey/instances/" + instance.ID.String() + "/audit"
	firstResponse := app.requestJSON(t, http.MethodGet, base+"?limit=2", nil, bearer(owner.Token))
	assertStatus(t, firstResponse, http.StatusOK)
	first := decodeResponse[audit.Page](t, firstResponse)
	if got := auditIDs(first.Items); !slices.Equal(got, []string{
		"01993000-0000-7000-8000-000000000004",
		"01993000-0000-7000-8000-000000000003",
	}) {
		t.Fatalf("неверная первая страница: %v", got)
	}
	if first.NextCursor == nil || strings.Contains(*first.NextCursor, "=") {
		t.Fatalf("неверный курсор: %v", first.NextCursor)
	}
	assertPublicAuditFields(t, firstResponse)

	secondResponse := app.requestJSON(
		t,
		http.MethodGet,
		base+"?limit=2&before="+*first.NextCursor,
		nil,
		bearer(owner.Token),
	)
	assertStatus(t, secondResponse, http.StatusOK)
	second := decodeResponse[audit.Page](t, secondResponse)
	if got := auditIDs(second.Items); !slices.Equal(got, []string{
		"01993000-0000-7000-8000-000000000002",
		"01993000-0000-7000-8000-000000000001",
	}) {
		t.Fatalf("неверная вторая страница: %v", got)
	}
	if second.NextCursor != nil {
		t.Fatalf("последняя страница вернула курсор: %q", *second.NextCursor)
	}
	if !second.Items[0].CreatedAt.Equal(createdAt) {
		t.Fatalf("время потеряло точность: %s", second.Items[0].CreatedAt)
	}

	foreign := app.requestJSON(
		t,
		http.MethodGet,
		base+"?before="+*first.NextCursor,
		nil,
		bearer(stranger.Token),
	)
	assertError(t, foreign, http.StatusNotFound, string(apierr.CodeNotFound))

	emptyBase := "/v1/managed/valkey/instances/" + strangerInstance.ID.String() + "/audit"
	replaceAuditLogs(t, app, strangerInstance.ID, nil)
	emptyResponse := app.requestJSON(t, http.MethodGet, emptyBase, nil, bearer(stranger.Token))
	assertStatus(t, emptyResponse, http.StatusOK)
	empty := decodeResponse[audit.Page](t, emptyResponse)
	if empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != nil {
		t.Fatalf("неверный пустой журнал: %+v", empty)
	}
}

func TestValkeyAuditValidatesQueryAndRejectsWrites(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	instance := createValkey(t, app, account, map[string]any{"name": "audit-query"})
	base := "/v1/managed/valkey/instances/" + instance.ID.String() + "/audit"

	for _, query := range []string{
		"?limit=",
		"?limit=0",
		"?limit=201",
		"?limit=1.5",
		"?limit=abc",
		"?before=broken",
		"?unknown=value",
		"?limit=1&limit=2",
		"?before=broken&before=again",
	} {
		t.Run(query, func(t *testing.T) {
			response := app.requestJSON(t, http.MethodGet, base+query, nil, bearer(account.Token))
			assertError(t, response, http.StatusBadRequest, string(apierr.CodeValidationFailed))
		})
	}

	before := countRows(t, app, &store.AuditLog{}, "resource_id = ?", instance.ID)
	response := app.requestJSON(t, http.MethodPost, base, map[string]any{
		"action": audit.ActionInstanceDelete,
	}, bearer(account.Token))
	assertStatus(t, response, http.StatusNotFound)
	if countRows(t, app, &store.AuditLog{}, "resource_id = ?", instance.ID) != before {
		t.Fatal("HTTP-запрос добавил событие аудита")
	}
}

func TestValkeyAuditRemainsReadableAfterSoftDelete(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	instance := createValkey(t, app, account, map[string]any{"name": "deleted-audit"})
	base := "/v1/managed/valkey/instances/" + instance.ID.String()

	assertStatus(
		t,
		app.requestJSON(t, http.MethodDelete, base, nil, bearer(account.Token)),
		http.StatusAccepted,
	)
	if err := app.database.DB().Model(&store.ValkeyInstance{}).Where("id = ?", instance.ID).
		UpdateColumn("deleted_at", time.Now().UTC()).Error; err != nil {
		t.Fatalf("подтвердить удаление: %v", err)
	}

	response := app.requestJSON(t, http.MethodGet, base+"/audit", nil, bearer(account.Token))
	assertStatus(t, response, http.StatusOK)
	page := decodeResponse[audit.Page](t, response)
	if len(page.Items) != 2 || page.Items[0].Action != audit.ActionInstanceDelete {
		t.Fatalf("история после удаления потеряна: %+v", page.Items)
	}
}

func replaceAuditLogs(t *testing.T, app *testAPI, resourceID uuid.UUID, records []store.AuditLog) {
	t.Helper()

	if err := app.database.DB().Where("resource_id = ?", resourceID).Delete(&store.AuditLog{}).Error; err != nil {
		t.Fatalf("очистить аудит инстанса: %v", err)
	}
	if len(records) > 0 {
		if err := app.database.DB().Create(&records).Error; err != nil {
			t.Fatalf("подготовить аудит инстанса: %v", err)
		}
	}
}

func auditRecord(
	account testAccount,
	resourceID uuid.UUID,
	service domain.ManagedService,
	id string,
	action audit.EventAction,
	createdAt time.Time,
) store.AuditLog {
	return store.AuditLog{
		ID: uuid.MustParse(id), UserID: account.ID, UserEmail: account.Email, Action: action,
		Service: &service, ResourceID: &resourceID, RequestID: id, CreatedAt: createdAt,
	}
}

func auditIDs(items []audit.LogEntry) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID.String())
	}

	return ids
}

func assertPublicAuditFields(t *testing.T, response testResponse) {
	t.Helper()

	var body struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatalf("разобрать поля аудита: %v", err)
	}
	want := []string{"action", "created_at", "id", "user_email"}
	got := make([]string, 0, len(body.Items[0]))
	for key := range body.Items[0] {
		got = append(got, key)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("неверные публичные поля: %v", got)
	}
}
