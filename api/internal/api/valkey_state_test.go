package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

type fixedDatabaseClock struct {
	now time.Time
}

func (c fixedDatabaseClock) DatabaseTime(context.Context, *gorm.DB) (time.Time, error) {
	return c.now, nil
}

func TestValkeyFreshnessBoundaryAndStatePriorityUseDatabaseTime(t *testing.T) {
	fixedNow := time.Date(2026, time.September, 8, 12, 0, 0, 123456000, time.UTC)
	app := newHTTPTestAPI(t, testAPIConfig{
		wrapDatabaseClock: func(valkeydomain.DatabaseClock) valkeydomain.DatabaseClock {
			return fixedDatabaseClock{now: fixedNow}
		},
	})
	account := app.registerAccount(t, "")
	setUserQuota(t, app, account.ID, 32, 128)

	exact := createValkey(t, app, account, map[string]any{"name": "exact-fresh", "vcpu": 1, "ram_gb": 1})
	setValkeyState(t, app, exact.ID, "running", 1, fixedNow.Add(-time.Minute))
	exactResponse := resizeValkey(t, app, account, exact.ID, 1, 2)
	assertStatus(t, exactResponse, http.StatusAccepted)

	stale := createValkey(t, app, account, map[string]any{"name": "stale-cache", "vcpu": 1, "ram_gb": 1})
	setValkeyState(t, app, stale.ID, "running", 1, fixedNow.Add(-time.Minute-time.Microsecond))
	staleError := assertError(
		t,
		resizeValkey(t, app, account, stale.ID, 1, 2),
		http.StatusConflict,
		string(apierr.CodeInstanceNotReady),
	)
	if staleError.Error.Details["reason"] != "stale_observation" {
		t.Fatalf("неверный приоритет stale: %+v", staleError)
	}

	invalidPhase := createValkey(t, app, account, map[string]any{"name": "invalid-phase", "vcpu": 1, "ram_gb": 1})
	setValkeyState(t, app, invalidPhase.ID, "provisioning", 1, fixedNow)
	phaseError := assertError(
		t,
		resizeValkey(t, app, account, invalidPhase.ID, 1, 2),
		http.StatusConflict,
		string(apierr.CodeInstanceNotReady),
	)
	if phaseError.Error.Details["reason"] != "invalid_phase" {
		t.Fatalf("неверная причина фазы: %+v", phaseError)
	}

	inProgress := createValkey(t, app, account, map[string]any{"name": "in-progress", "vcpu": 1, "ram_gb": 1})
	setValkeyState(t, app, inProgress.ID, "running", 0, fixedNow.Add(-2*time.Minute))
	progressError := assertError(
		t,
		resizeValkey(t, app, account, inProgress.ID, 1, 2),
		http.StatusConflict,
		string(apierr.CodeOperationInProgress),
	)
	if progressError.Error.Details["desired_generation"] != float64(1) ||
		progressError.Error.Details["observed_generation"] != float64(0) {
		t.Fatalf("неверные details поколения: %+v", progressError)
	}

	deleting := createValkey(t, app, account, map[string]any{"name": "deleting-cache", "vcpu": 1, "ram_gb": 1})
	assertStatus(
		t,
		app.requestJSON(
			t,
			http.MethodDelete,
			"/v1/managed/valkey/instances/"+deleting.ID.String(),
			nil,
			bearer(account.Token),
		),
		http.StatusAccepted,
	)
	deletingError := assertError(
		t,
		resizeValkey(t, app, account, deleting.ID, 1, 1),
		http.StatusConflict,
		string(apierr.CodeInstanceNotReady),
	)
	if deletingError.Error.Details["reason"] != "deleting" {
		t.Fatalf("удаление не получило приоритет: %+v", deletingError)
	}
}

func TestValkeyPasswordValidationOrder(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	created := createValkey(t, app, account, map[string]any{"name": "password-state"})
	base := "/v1/managed/valkey/instances/" + created.ID.String() + "/credentials/rotate"

	inProgress := app.requestJSON(t, http.MethodPost, base, map[string]any{
		"password": rotatedValkeyPassword, "expected_password_version": 99,
	}, mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": uuid.NewString()}))
	assertError(t, inProgress, http.StatusConflict, string(apierr.CodeOperationInProgress))

	makeValkeyReady(t, app, created.ID)
	versionConflict := app.requestJSON(t, http.MethodPost, base, map[string]any{
		"password": rotatedValkeyPassword, "expected_password_version": 99,
	}, mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": uuid.NewString()}))
	errorBody := assertError(t, versionConflict, http.StatusConflict, string(apierr.CodeConflict))
	if errorBody.Error.Details["reason"] != "password_version_mismatch" ||
		errorBody.Error.Details["password_version"] != float64(1) {
		t.Fatalf("неверный конфликт версии: %+v", errorBody)
	}

	samePassword := app.requestJSON(t, http.MethodPost, base, map[string]any{
		"password": testValkeyPassword, "expected_password_version": 1,
	}, mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": uuid.NewString()}))
	assertError(t, samePassword, http.StatusBadRequest, string(apierr.CodeValidationFailed))
}

func resizeValkey(
	t *testing.T,
	app *testAPI,
	account testAccount,
	instanceID uuid.UUID,
	vcpu int,
	ramGB int,
) testResponse {
	t.Helper()

	return app.requestJSON(
		t,
		http.MethodPost,
		"/v1/managed/valkey/instances/"+instanceID.String()+"/resize",
		map[string]any{"vcpu": vcpu, "ram_gb": ramGB},
		mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": uuid.NewString()}),
	)
}

func setValkeyState(
	t *testing.T,
	app *testAPI,
	instanceID uuid.UUID,
	phase string,
	observedGeneration int,
	observedAt time.Time,
) {
	t.Helper()

	record := loadValkey(t, app, instanceID)
	if err := app.database.DB().Model(&store.ValkeyInstance{}).Where("id = ?", instanceID).UpdateColumns(map[string]any{
		"phase": phase, "observed_generation": observedGeneration, "observed_at": observedAt,
		"applied_vcpu": record.VCPU, "applied_ram_gb": record.RAMGB,
		"applied_password_version": record.PasswordVersion,
	}).Error; err != nil {
		t.Fatalf("подготовить состояние инстанса: %v", err)
	}
}
