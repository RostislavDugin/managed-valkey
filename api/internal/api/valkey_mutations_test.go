package api_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

const rotatedValkeyPassword = "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"

type observedSnapshot struct {
	Phase                     domain.ValkeyInstancePhase
	PhaseReason               *string
	ObservedGeneration        int
	ObservedAt                *time.Time
	IsRecoveryRequired        bool
	NetworkVerificationStatus domain.ValkeyNetworkVerificationStatus
	NetworkVerifiedAt         *time.Time
	AppliedPasswordVersion    int
	AppliedVCPU               int
	AppliedRAMGB              int
}

func TestValkeyMutationsPreserveObservedStateAndBilling(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	created := createValkey(t, app, account, map[string]any{
		"name": "mutable-cache", "vcpu": 1, "ram_gb": 2,
	})
	ready := makeValkeyReady(t, app, created.ID)
	observed := observedState(ready)
	base := "/v1/managed/valkey/instances/" + created.ID.String()

	patchedResponse := app.requestJSON(t, http.MethodPatch, base, map[string]any{
		"name":        "renamed-cache",
		"maintenance": map[string]any{"dow": 0, "hour_utc": 0, "duration_min": 30},
	}, mergeHeaders(bearer(account.Token), map[string]string{"X-Request-Id": "patch-request"}))
	assertStatus(t, patchedResponse, http.StatusOK)
	patched := decodeResponse[valkeydomain.Instance](t, patchedResponse)
	if patched.Name != "renamed-cache" || patched.Maintenance == nil || patched.Maintenance.DOW != 0 ||
		patched.Maintenance.HourUTC != 0 || patched.DesiredGeneration != 1 ||
		!patched.ConfigurationRequestedAt.Equal(created.ConfigurationRequestedAt) {
		t.Fatalf("неожиданный PATCH: %+v", patched)
	}
	assertObservedState(t, loadValkey(t, app, created.ID), observed)

	auditCount := countValkeyAudit(t, app, created.ID)
	noOpPatch := app.requestJSON(t, http.MethodPatch, base, map[string]any{
		"name":        "renamed-cache",
		"maintenance": map[string]any{"dow": 0, "hour_utc": 0, "duration_min": 30},
	}, bearer(account.Token))
	assertStatus(t, noOpPatch, http.StatusOK)
	if countValkeyAudit(t, app, created.ID) != auditCount {
		t.Fatal("PATCH без изменения добавил аудит")
	}

	whitelistResponse := app.requestJSON(t, http.MethodPut, base+"/whitelist", map[string]any{
		"is_whitelist_enabled": true,
		"whitelist_cidrs":      []string{"198.51.100.5", "192.0.2.1/24", "192.0.2.0/24"},
	}, bearer(account.Token))
	assertStatus(t, whitelistResponse, http.StatusAccepted)
	whitelist := decodeResponse[valkeydomain.Instance](t, whitelistResponse)
	wantCIDRs := []string{"192.0.2.0/24", "198.51.100.5/32"}
	if !whitelist.IsWhitelistEnabled || !slices.Equal(whitelist.WhitelistCIDRs, wantCIDRs) ||
		whitelist.DesiredGeneration != 2 {
		t.Fatalf("неожиданный whitelist: %+v", whitelist)
	}
	assertObservedState(t, loadValkey(t, app, created.ID), observed)
	assertDatabaseCount(t, app.database.DB().Model(&store.BillingPeriod{}).Where("resource_id = ?", created.ID), 1)

	makeValkeyReady(t, app, created.ID)
	beforeResize := loadValkey(t, app, created.ID)
	resizeKey := uuid.NewString()
	resizeBody := map[string]any{"vcpu": 2, "ram_gb": 8}
	resizeResponse := app.requestJSON(t, http.MethodPost, base+"/resize", resizeBody, mergeHeaders(
		bearer(account.Token),
		map[string]string{"Idempotency-Key": resizeKey},
	))
	assertStatus(t, resizeResponse, http.StatusAccepted)
	resized := decodeResponse[valkeydomain.Instance](t, resizeResponse)
	if resized.VCPU != 2 || resized.RAMGB != 8 || resized.DesiredGeneration != 3 ||
		resized.ObservedGeneration != beforeResize.ObservedGeneration || !resized.IsUpdating {
		t.Fatalf("неожиданный resize: %+v", resized)
	}
	assertObservedState(t, loadValkey(t, app, created.ID), observedState(beforeResize))

	var periods []store.BillingPeriod
	if err := app.database.DB().
		Where("resource_id = ?", created.ID).
		Order("started_at").
		Find(&periods).
		Error; err != nil {
		t.Fatalf("прочитать биллинговые периоды: %v", err)
	}
	if len(periods) != 2 || periods[0].EndedAt == nil || !periods[0].EndedAt.Equal(periods[1].StartedAt) ||
		!periods[1].StartedAt.Equal(resized.UpdatedAt) || !resized.UpdatedAt.Equal(resized.ConfigurationRequestedAt) ||
		periods[0].PriceCoinsPerHour != 225 || periods[1].PriceCoinsPerHour != 650 {
		t.Fatalf("неверные границы или цены периодов: %+v", periods)
	}

	replayedResize := app.requestJSON(t, http.MethodPost, base+"/resize", resizeBody, mergeHeaders(
		bearer(account.Token),
		map[string]string{"Idempotency-Key": resizeKey},
	))
	assertStatus(t, replayedResize, http.StatusAccepted)
	if string(replayedResize.Body) != string(resizeResponse.Body) {
		t.Fatalf("повтор resize изменил ответ: %s != %s", replayedResize.Body, resizeResponse.Body)
	}
	assertDatabaseCount(t, app.database.DB().Model(&store.BillingPeriod{}).Where("resource_id = ?", created.ID), 2)
	resizeMismatch := app.requestJSON(t, http.MethodPost, base+"/resize", map[string]any{
		"vcpu": 1, "ram_gb": 4,
	}, mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": resizeKey}))
	assertError(t, resizeMismatch, http.StatusUnprocessableEntity, string(apierr.CodeIdempotencyMismatch))

	makeValkeyReady(t, app, created.ID)
	beforeRotate := loadValkey(t, app, created.ID)
	rotateKey := uuid.NewString()
	rotateBody := map[string]any{
		"password": rotatedValkeyPassword, "expected_password_version": beforeRotate.PasswordVersion,
	}
	rotateResponse := app.requestJSON(t, http.MethodPost, base+"/credentials/rotate", rotateBody, mergeHeaders(
		bearer(account.Token),
		map[string]string{"Idempotency-Key": rotateKey},
	))
	assertStatus(t, rotateResponse, http.StatusAccepted)
	credentials := decodeResponse[valkeydomain.Credentials](t, rotateResponse)
	if credentials.PasswordVersion != 2 || credentials.AppliedPasswordVersion != 1 ||
		credentials.PasswordHint != rotatedValkeyPassword[:4]+"*****" {
		t.Fatalf("неожиданный rotate: %+v", credentials)
	}
	rotated := loadValkey(t, app, created.ID)
	if rotated.DesiredGeneration != 4 || rotated.AppliedPasswordVersion != beforeRotate.AppliedPasswordVersion {
		t.Fatalf("неверные версии после rotate: %+v", rotated)
	}
	assertObservedState(t, rotated, observedState(beforeRotate))
	assertDatabaseCount(t, app.database.DB().Model(&store.BillingPeriod{}).Where("resource_id = ?", created.ID), 2)

	replayedRotate := app.requestJSON(t, http.MethodPost, base+"/credentials/rotate", rotateBody, mergeHeaders(
		bearer(account.Token),
		map[string]string{"Idempotency-Key": rotateKey},
	))
	assertStatus(t, replayedRotate, http.StatusAccepted)
	if string(replayedRotate.Body) != string(rotateResponse.Body) {
		t.Fatalf("повтор rotate изменил ответ: %s != %s", replayedRotate.Body, rotateResponse.Body)
	}
	rotateMismatch := app.requestJSON(t, http.MethodPost, base+"/credentials/rotate", map[string]any{
		"password": testValkeyPassword, "expected_password_version": beforeRotate.PasswordVersion,
	}, mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": rotateKey}))
	assertError(t, rotateMismatch, http.StatusUnprocessableEntity, string(apierr.CodeIdempotencyMismatch))

	beforeDelete := loadValkey(t, app, created.ID)
	deleteResponse := app.requestJSON(t, http.MethodDelete, base, nil, bearer(account.Token))
	assertStatus(t, deleteResponse, http.StatusAccepted)
	deleted := loadValkey(t, app, created.ID)
	if deleted.DeletionRequestedAt == nil || deleted.DesiredGeneration != beforeDelete.DesiredGeneration ||
		!deleted.ConfigurationRequestedAt.Equal(beforeDelete.ConfigurationRequestedAt) {
		t.Fatalf("DELETE изменил поколение или границу конфигурации: %+v", deleted)
	}
	assertObservedState(t, deleted, observedState(beforeDelete))
	assertDatabaseCount(
		t,
		app.database.DB().Model(&store.BillingPeriod{}).Where("resource_id = ? AND ended_at IS NULL", created.ID),
		0,
	)

	auditCount = countValkeyAudit(t, app, created.ID)
	assertStatus(t, app.requestJSON(t, http.MethodDelete, base, nil, bearer(account.Token)), http.StatusAccepted)
	if countValkeyAudit(t, app, created.ID) != auditCount {
		t.Fatal("повтор DELETE добавил аудит")
	}
	afterDeleteResize := app.requestJSON(t, http.MethodPost, base+"/resize", resizeBody, mergeHeaders(
		bearer(account.Token),
		map[string]string{"Idempotency-Key": resizeKey},
	))
	assertStatus(t, afterDeleteResize, http.StatusAccepted)
	afterDeleteRotate := app.requestJSON(t, http.MethodPost, base+"/credentials/rotate", rotateBody, mergeHeaders(
		bearer(account.Token),
		map[string]string{"Idempotency-Key": rotateKey},
	))
	assertStatus(t, afterDeleteRotate, http.StatusAccepted)
}

func TestResizeNoOpPrecedesReadinessAndIsIdempotent(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	created := createValkey(t, app, account, map[string]any{"name": "noop-cache", "vcpu": 1, "ram_gb": 2})
	base := "/v1/managed/valkey/instances/" + created.ID.String() + "/resize"
	key := uuid.NewString()
	body := map[string]any{"vcpu": 1, "ram_gb": 2}
	headers := mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": key})

	first := app.requestJSON(t, http.MethodPost, base, body, headers)
	assertStatus(t, first, http.StatusOK)
	replayed := app.requestJSON(t, http.MethodPost, base, body, headers)
	assertStatus(t, replayed, http.StatusOK)
	if string(first.Body) != string(replayed.Body) {
		t.Fatalf("повтор no-op resize изменил ответ: %s != %s", first.Body, replayed.Body)
	}
	assertDatabaseCount(t, app.database.DB().Model(&store.BillingPeriod{}).Where("resource_id = ?", created.ID), 1)
	assertDatabaseCount(t, app.database.DB().Model(&store.AuditLog{}).Where("resource_id = ?", created.ID), 1)
}

func makeValkeyReady(t *testing.T, app *testAPI, instanceID uuid.UUID) store.ValkeyInstance {
	t.Helper()

	record := loadValkey(t, app, instanceID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	reason := "TEST_OBSERVATION"
	values := map[string]any{
		"phase": "running", "phase_reason": reason,
		"observed_generation": record.DesiredGeneration, "observed_at": now,
		"is_recovery_required":        true,
		"network_verification_status": "verified", "network_verified_at": now,
		"applied_password_version": record.PasswordVersion,
		"applied_vcpu":             record.VCPU, "applied_ram_gb": record.RAMGB,
	}
	if err := app.database.DB().Model(&store.ValkeyInstance{}).Where("id = ?", instanceID).
		UpdateColumns(values).Error; err != nil {
		t.Fatalf("подготовить готовый инстанс: %v", err)
	}

	return loadValkey(t, app, instanceID)
}

func loadValkey(t *testing.T, app *testAPI, instanceID uuid.UUID) store.ValkeyInstance {
	t.Helper()

	var record store.ValkeyInstance
	if err := app.database.DB().Unscoped().Where("id = ?", instanceID).First(&record).Error; err != nil {
		t.Fatalf("прочитать инстанс Valkey: %v", err)
	}

	return record
}

func observedState(record store.ValkeyInstance) observedSnapshot {
	return observedSnapshot{
		Phase:                     record.Phase,
		PhaseReason:               record.PhaseReason,
		ObservedGeneration:        record.ObservedGeneration,
		ObservedAt:                record.ObservedAt,
		IsRecoveryRequired:        record.IsRecoveryRequired,
		NetworkVerificationStatus: record.NetworkVerificationStatus,
		NetworkVerifiedAt:         record.NetworkVerifiedAt,
		AppliedPasswordVersion:    record.AppliedPasswordVersion,
		AppliedVCPU:               record.AppliedVCPU,
		AppliedRAMGB:              record.AppliedRAMGB,
	}
}

func assertObservedState(t *testing.T, record store.ValkeyInstance, want observedSnapshot) {
	t.Helper()

	got := observedState(record)
	if got.Phase != want.Phase || !equalPointers(got.PhaseReason, want.PhaseReason) ||
		got.ObservedGeneration != want.ObservedGeneration || !equalTimePointers(got.ObservedAt, want.ObservedAt) ||
		got.IsRecoveryRequired != want.IsRecoveryRequired ||
		got.NetworkVerificationStatus != want.NetworkVerificationStatus ||
		!equalTimePointers(got.NetworkVerifiedAt, want.NetworkVerifiedAt) ||
		got.AppliedPasswordVersion != want.AppliedPasswordVersion || got.AppliedVCPU != want.AppliedVCPU ||
		got.AppliedRAMGB != want.AppliedRAMGB {
		t.Fatalf("наблюдаемые поля изменены: got=%+v want=%+v", got, want)
	}
}

func countValkeyAudit(t *testing.T, app *testAPI, instanceID uuid.UUID) int64 {
	t.Helper()

	var count int64
	if err := app.database.DB().
		Model(&store.AuditLog{}).
		Where("resource_id = ?", instanceID).
		Count(&count).
		Error; err != nil {
		t.Fatalf("посчитать аудит Valkey: %v", err)
	}

	return count
}

func equalPointers[T comparable](left, right *T) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func equalTimePointers(left, right *time.Time) bool {
	return left == nil && right == nil || left != nil && right != nil && left.Equal(*right)
}
