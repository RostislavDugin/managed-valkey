package api_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

var errIdempotencyRejected = errors.New("управляемый отказ ключа идемпотентности")

type failingIdempotencyRepository struct {
	valkeydomain.Repository
}

func (failingIdempotencyRepository) CreateIdempotencyKey(
	context.Context,
	*gorm.DB,
	*store.IdempotencyKey,
) error {
	return errIdempotencyRejected
}

type valkeyDatabaseSnapshot struct {
	instances []store.ValkeyInstance
	periods   []store.BillingPeriod
	audit     []store.AuditLog
	keys      []store.IdempotencyKey
}

type atomicMutation struct {
	name          string
	action        audit.EventAction
	requiresReady bool
	execute       func(*testing.T, *testAPI, testAccount, uuid.UUID, string) testResponse
	success       int
}

func Test_MutateValkey_WhenAuditWriteFails_RollsBackEveryMutation(t *testing.T) {
	mutations := []atomicMutation{
		{
			name: "PATCH", action: audit.ActionInstanceUpdate, success: http.StatusOK,
			execute: func(t *testing.T, app *testAPI, owner testAccount, instanceID uuid.UUID, _ string) testResponse {
				return app.requestJSON(t, http.MethodPatch, instancePath(instanceID), map[string]any{
					"name": "renamed-after-audit",
				}, bearer(owner.Token))
			},
		},
		{
			name:          "resize",
			action:        audit.ActionInstanceResize,
			requiresReady: true,
			success:       http.StatusAccepted,
			execute: func(t *testing.T, app *testAPI, owner testAccount, instanceID uuid.UUID, key string) testResponse {
				return app.requestJSON(t, http.MethodPost, instancePath(instanceID)+"/resize", map[string]any{
					"vcpu": 2, "ram_gb": 8,
				}, idempotentBearer(owner.Token, key))
			},
		},
		{
			name: "whitelist", action: audit.ActionInstanceWhitelistUpdate, requiresReady: true,
			success: http.StatusAccepted,
			execute: func(t *testing.T, app *testAPI, owner testAccount, instanceID uuid.UUID, _ string) testResponse {
				return app.requestJSON(t, http.MethodPut, instancePath(instanceID)+"/whitelist", map[string]any{
					"is_whitelist_enabled": true, "whitelist_cidrs": []string{"192.0.2.0/24"},
				}, bearer(owner.Token))
			},
		},
		{
			name:          "rotate",
			action:        audit.ActionInstancePasswordRotate,
			requiresReady: true,
			success:       http.StatusAccepted,
			execute: func(t *testing.T, app *testAPI, owner testAccount, instanceID uuid.UUID, key string) testResponse {
				return app.requestJSON(
					t,
					http.MethodPost,
					instancePath(instanceID)+"/credentials/rotate",
					map[string]any{
						"password": rotatedValkeyPassword, "expected_password_version": 1,
					},
					idempotentBearer(owner.Token, key),
				)
			},
		},
		{
			name: "DELETE", action: audit.ActionInstanceDelete, success: http.StatusAccepted,
			execute: func(t *testing.T, app *testAPI, owner testAccount, instanceID uuid.UUID, _ string) testResponse {
				return app.requestJSON(t, http.MethodDelete, instancePath(instanceID), nil, bearer(owner.Token))
			},
		},
	}

	for _, mutation := range mutations {
		for _, afterWrite := range []bool{false, true} {
			failurePoint := "до вставки"
			if afterWrite {
				failurePoint = "после вставки"
			}
			t.Run(mutation.name+" "+failurePoint, func(t *testing.T) {
				setup := newHTTPTestAPI(t, testAPIConfig{})
				owner := setup.registerAccount(t, "")
				setUserQuota(t, setup, owner.ID, 16, 64)
				instance := createValkey(t, setup, owner, map[string]any{"name": "audit-rollback"})
				if mutation.requiresReady {
					makeValkeyReady(t, setup, instance.ID)
				}
				before := loadValkeyDatabaseSnapshot(t, setup, owner.ID)
				failing := newHTTPTestAPI(t, testAPIConfig{
					wrapAuditRepository: failAudit(mutation.action, afterWrite),
				})
				key := uuid.NewString()

				failed := mutation.execute(t, failing, owner, instance.ID, key)
				assertSafeInternal(t, failing, failed, owner.Token, testValkeyPassword, rotatedValkeyPassword)
				assertValkeyDatabaseSnapshot(t, failing, owner.ID, before)

				succeeded := mutation.execute(t, setup, owner, instance.ID, key)
				assertStatus(t, succeeded, mutation.success)
			})
		}
	}
}

func Test_CreateValkey_WhenAuditWriteFails_RollsBackTransaction(t *testing.T) {
	for _, afterWrite := range []bool{false, true} {
		name := "до вставки"
		if afterWrite {
			name = "после вставки"
		}
		t.Run(name, func(t *testing.T) {
			setup := newHTTPTestAPI(t, testAPIConfig{})
			owner := setup.registerAccount(t, "")
			before := loadValkeyDatabaseSnapshot(t, setup, owner.ID)
			failing := newHTTPTestAPI(t, testAPIConfig{
				wrapAuditRepository: failAudit(audit.ActionInstanceCreate, afterWrite),
			})
			key := uuid.NewString()
			body := createAtomicityBody("create-audit-rollback")

			failed := failing.requestJSON(
				t, http.MethodPost, "/v1/managed/valkey/instances", body, idempotentBearer(owner.Token, key),
			)
			assertSafeInternal(t, failing, failed, owner.Token, testValkeyPassword)
			assertValkeyDatabaseSnapshot(t, failing, owner.ID, before)

			succeeded := setup.requestJSON(
				t, http.MethodPost, "/v1/managed/valkey/instances", body, idempotentBearer(owner.Token, key),
			)
			assertStatus(t, succeeded, http.StatusAccepted)
		})
	}
}

func Test_MutateValkey_WhenIdempotencyWriteFails_RollsBackEarlierWrites(t *testing.T) {
	t.Run("отказ записи ключа идемпотентности при создании откатывает предшествующие записи", func(t *testing.T) {
		setup := newHTTPTestAPI(t, testAPIConfig{})
		owner := setup.registerAccount(t, "")
		before := loadValkeyDatabaseSnapshot(t, setup, owner.ID)
		failing := newHTTPTestAPI(t, testAPIConfig{wrapValkeyRepository: failIdempotencyWrite})
		body := createAtomicityBody("create-key-rollback")
		key := uuid.NewString()

		failed := failing.requestJSON(
			t, http.MethodPost, "/v1/managed/valkey/instances", body, idempotentBearer(owner.Token, key),
		)
		assertSafeInternal(t, failing, failed, owner.Token, testValkeyPassword)
		assertValkeyDatabaseSnapshot(t, failing, owner.ID, before)
		assertStatus(t, setup.requestJSON(
			t, http.MethodPost, "/v1/managed/valkey/instances", body, idempotentBearer(owner.Token, key),
		), http.StatusAccepted)
	})

	for _, mutation := range []atomicMutation{
		{
			name: "resize", requiresReady: true, success: http.StatusAccepted,
			execute: func(t *testing.T, app *testAPI, owner testAccount, instanceID uuid.UUID, key string) testResponse {
				return app.requestJSON(t, http.MethodPost, instancePath(instanceID)+"/resize", map[string]any{
					"vcpu": 2, "ram_gb": 8,
				}, idempotentBearer(owner.Token, key))
			},
		},
		{
			name: "rotate", requiresReady: true, success: http.StatusAccepted,
			execute: func(t *testing.T, app *testAPI, owner testAccount, instanceID uuid.UUID, key string) testResponse {
				return app.requestJSON(t, http.MethodPost, instancePath(instanceID)+"/credentials/rotate", map[string]any{
					"password": rotatedValkeyPassword, "expected_password_version": 1,
				}, idempotentBearer(owner.Token, key))
			},
		},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			setup := newHTTPTestAPI(t, testAPIConfig{})
			owner := setup.registerAccount(t, "")
			setUserQuota(t, setup, owner.ID, 16, 64)
			instance := createValkey(t, setup, owner, map[string]any{"name": mutation.name + "-key-rollback"})
			makeValkeyReady(t, setup, instance.ID)
			before := loadValkeyDatabaseSnapshot(t, setup, owner.ID)
			failing := newHTTPTestAPI(t, testAPIConfig{wrapValkeyRepository: failIdempotencyWrite})
			key := uuid.NewString()

			failed := mutation.execute(t, failing, owner, instance.ID, key)
			assertSafeInternal(t, failing, failed, owner.Token, testValkeyPassword, rotatedValkeyPassword)
			assertValkeyDatabaseSnapshot(t, failing, owner.ID, before)
			assertStatus(t, mutation.execute(t, setup, owner, instance.ID, key), mutation.success)
		})
	}
}

func failIdempotencyWrite(repository valkeydomain.Repository) valkeydomain.Repository {
	return failingIdempotencyRepository{Repository: repository}
}

func loadValkeyDatabaseSnapshot(t *testing.T, app *testAPI, userID uuid.UUID) valkeyDatabaseSnapshot {
	t.Helper()

	snapshot := valkeyDatabaseSnapshot{}
	queries := []struct {
		name  string
		query *gorm.DB
	}{
		{
			name:  "инстансы",
			query: app.database.DB().Unscoped().Where("user_id = ?", userID).Order("id").Find(&snapshot.instances),
		},
		{name: "периоды", query: app.database.DB().Where("user_id = ?", userID).Order("id").Find(&snapshot.periods)},
		{name: "аудит", query: app.database.DB().Where("user_id = ?", userID).Order("id").Find(&snapshot.audit)},
		{name: "ключи", query: app.database.DB().Where("user_id = ?", userID).Order("key").Find(&snapshot.keys)},
	}
	for _, query := range queries {
		if query.query.Error != nil {
			t.Fatalf("прочитать %s для снимка: %v", query.name, query.query.Error)
		}
	}

	return snapshot
}

func assertValkeyDatabaseSnapshot(
	t *testing.T,
	app *testAPI,
	userID uuid.UUID,
	want valkeyDatabaseSnapshot,
) {
	t.Helper()

	if got := loadValkeyDatabaseSnapshot(t, app, userID); !reflect.DeepEqual(got, want) {
		t.Fatal("отказ оставил частичные записи в базе")
	}
}

func assertSafeInternal(t *testing.T, app *testAPI, response testResponse, secrets ...string) {
	t.Helper()

	body := assertError(t, response, http.StatusInternalServerError, string(apierr.CodeInternal))
	if body.Error.Message != "Внутренняя ошибка сервера" || len(body.Error.Details) != 0 {
		t.Fatalf("небезопасная внутренняя ошибка: %s", response.Body)
	}
	unsafeValues := append(secrets, errAuditRejected.Error(), errIdempotencyRejected.Error(), "audit_logs", "INSERT")
	if containsAny(string(response.Body), unsafeValues...) || containsAny(app.logs.String(), secrets...) {
		t.Fatalf("ответ или журнал содержит внутренние данные: response=%s", response.Body)
	}
}

func createAtomicityBody(name string) map[string]any {
	return map[string]any{
		"name": name, "prefix": "atomic", "mode": "single", "vcpu": 1, "ram_gb": 1,
		"password": testValkeyPassword, "is_whitelist_enabled": false, "whitelist_cidrs": []string{},
	}
}

func instancePath(instanceID uuid.UUID) string {
	return "/v1/managed/valkey/instances/" + instanceID.String()
}

func idempotentBearer(token, key string) map[string]string {
	return mergeHeaders(bearer(token), map[string]string{"Idempotency-Key": key})
}
