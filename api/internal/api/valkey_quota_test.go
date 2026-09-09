package api_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

func TestValkeyPersonalQuotaCountsModeAndExactBoundary(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")

	createValkey(t, app, account, map[string]any{
		"name": "ha-cache", "mode": "ha", "vcpu": 1, "ram_gb": 4,
	})
	createValkey(t, app, account, map[string]any{
		"name": "boundary-cache", "mode": "single", "vcpu": 1, "ram_gb": 4,
	})

	me := decodeResponse[currentUserResponse](
		t,
		app.requestJSON(t, http.MethodGet, "/v1/me", nil, bearer(account.Token)),
	)
	if me.Usage.UsedVCPU != 4 || me.Usage.UsedRAMGB != 16 {
		t.Fatalf("неверный резерв HA и single: %+v", me.Usage)
	}

	rejected := app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", map[string]any{
		"name": "over-quota", "prefix": "quota", "mode": "single", "vcpu": 1, "ram_gb": 1,
		"password": testValkeyPassword,
	}, mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": uuid.NewString()}))
	errorBody := assertError(t, rejected, http.StatusUnprocessableEntity, string(apierr.CodeQuotaExceeded))
	if errorBody.Error.Details["reason"] != "user_quota" {
		t.Fatalf("неверная причина квоты: %+v", errorBody)
	}
	assertDatabaseCount(t, app.database.DB().Model(&store.ValkeyInstance{}).Where("user_id = ?", account.ID), 2)
}

func TestValkeyClusterQuotaRejectsImmediatelyWithoutPartialRows(t *testing.T) {
	tests := []struct {
		name        string
		clusterVCPU int
		clusterRAM  int
		firstRAM    int
		secondVCPU  int
		secondRAM   int
		missingVCPU float64
		missingRAM  float64
	}{
		{name: "CPU", clusterVCPU: 2, clusterRAM: 16, firstRAM: 1, secondVCPU: 2, secondRAM: 8, missingVCPU: 1},
		{name: "RAM", clusterVCPU: 16, clusterRAM: 2, firstRAM: 1, secondVCPU: 1, secondRAM: 2, missingRAM: 1},
		{
			name:        "оба",
			clusterVCPU: 1,
			clusterRAM:  1,
			firstRAM:    1,
			secondVCPU:  1,
			secondRAM:   1,
			missingVCPU: 1,
			missingRAM:  1,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			app := newHTTPTestAPI(t, testAPIConfig{
				clusterVCPU:  testCase.clusterVCPU,
				clusterRAMGB: testCase.clusterRAM,
			})
			account := app.registerAccount(t, "")
			setUserQuota(t, app, account.ID, 32, 128)
			createValkey(t, app, account, map[string]any{
				"name": "existing-cache", "vcpu": 1, "ram_gb": testCase.firstRAM,
			})
			beforeInstances := countRows(t, app, &store.ValkeyInstance{}, "user_id = ?", account.ID)
			beforeAudit := countRows(t, app, &store.AuditLog{}, "user_id = ?", account.ID)
			beforePeriods := countRows(t, app, &store.BillingPeriod{}, "user_id = ?", account.ID)
			beforeKeys := countRows(t, app, &store.IdempotencyKey{}, "user_id = ?", account.ID)

			key := uuid.NewString()
			response := app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", map[string]any{
				"name": "rejected-cache", "prefix": "quota", "mode": "single",
				"vcpu": testCase.secondVCPU, "ram_gb": testCase.secondRAM, "password": testValkeyPassword,
			}, mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": key}))
			errorBody := assertError(t, response, http.StatusUnprocessableEntity, string(apierr.CodeNotEnoughResources))
			if errorBody.Error.Details["reason"] != "cluster_quota" {
				t.Fatalf("неверная причина: %+v", errorBody)
			}
			missing := errorBody.Error.Details["missing"].(map[string]any)
			if missing["vcpu"] != testCase.missingVCPU || missing["ram_gb"] != testCase.missingRAM {
				t.Fatalf("неверный дефицит: %+v", missing)
			}

			if countRows(t, app, &store.ValkeyInstance{}, "user_id = ?", account.ID) != beforeInstances ||
				countRows(t, app, &store.AuditLog{}, "user_id = ?", account.ID) != beforeAudit ||
				countRows(t, app, &store.BillingPeriod{}, "user_id = ?", account.ID) != beforePeriods ||
				countRows(t, app, &store.IdempotencyKey{}, "user_id = ?", account.ID) != beforeKeys {
				t.Fatal("отказ по общему бюджету оставил частичные строки")
			}
		})
	}
}

func TestValkeyClusterQuotaAcceptsExactBoundary(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{clusterVCPU: 2, clusterRAMGB: 3})
	account := app.registerAccount(t, "")
	setUserQuota(t, app, account.ID, 8, 32)
	createValkey(t, app, account, map[string]any{"name": "first-cache", "vcpu": 1, "ram_gb": 1})
	createValkey(t, app, account, map[string]any{"name": "exact-cache", "vcpu": 1, "ram_gb": 2})

	me := decodeResponse[currentUserResponse](
		t,
		app.requestJSON(t, http.MethodGet, "/v1/me", nil, bearer(account.Token)),
	)
	if me.Usage.UsedVCPU != 2 || me.Usage.UsedRAMGB != 3 {
		t.Fatalf("точная граница посчитана неверно: %+v", me.Usage)
	}
}

func TestValkeyResizeCanReduceAfterQuotaDecrease(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	setUserQuota(t, app, account.ID, 8, 32)
	created := createValkey(t, app, account, map[string]any{
		"name": "shrinking-cache", "vcpu": 4, "ram_gb": 16,
	})
	makeValkeyReady(t, app, created.ID)
	setUserQuota(t, app, account.ID, 1, 4)
	base := "/v1/managed/valkey/instances/" + created.ID.String() + "/resize"

	shrinking := app.requestJSON(t, http.MethodPost, base, map[string]any{"vcpu": 2, "ram_gb": 8}, mergeHeaders(
		bearer(account.Token),
		map[string]string{"Idempotency-Key": uuid.NewString()},
	))
	assertStatus(t, shrinking, http.StatusAccepted)
	makeValkeyReady(t, app, created.ID)

	stillOverLimit := app.requestJSON(t, http.MethodPost, base, map[string]any{"vcpu": 1, "ram_gb": 4}, mergeHeaders(
		bearer(account.Token),
		map[string]string{"Idempotency-Key": uuid.NewString()},
	))
	assertStatus(t, stillOverLimit, http.StatusAccepted)
	makeValkeyReady(t, app, created.ID)

	increase := app.requestJSON(t, http.MethodPost, base, map[string]any{"vcpu": 2, "ram_gb": 8}, mergeHeaders(
		bearer(account.Token),
		map[string]string{"Idempotency-Key": uuid.NewString()},
	))
	assertError(t, increase, http.StatusUnprocessableEntity, string(apierr.CodeQuotaExceeded))
}

func setUserQuota(t *testing.T, app *testAPI, userID uuid.UUID, maxVCPU, maxRAMGB int) {
	t.Helper()

	if err := app.database.DB().Model(&store.UserQuota{}).Where("user_id = ?", userID).UpdateColumns(map[string]any{
		"max_vcpu": maxVCPU, "max_ram_gb": maxRAMGB,
	}).Error; err != nil {
		t.Fatalf("изменить квоту пользователя: %v", err)
	}
}

func countRows(t *testing.T, app *testAPI, model any, condition string, args ...any) int64 {
	t.Helper()

	var count int64
	if err := app.database.DB().Model(model).Where(condition, args...).Count(&count).Error; err != nil {
		t.Fatalf("посчитать строки: %v", err)
	}

	return count
}
