package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

func Test_CreateValkey_WhenClusterCapacityIsFragmented_ReturnsPlacementDetailsWithoutPartialRows(t *testing.T) {
	topology := valkeydomain.ClusterTopology{NodeCount: 3, NodeCPUMilli: 3000, NodeRAMMiB: 12288}
	app := newHTTPTestAPI(t, testAPIConfig{clusterTopology: &topology})
	seedOwner := app.registerAccount(t, "")
	candidateOwner := app.registerAccount(t, "")
	setUserQuota(t, app, seedOwner.ID, 32, 128)
	setUserQuota(t, app, candidateOwner.ID, 32, 128)
	for index := range 3 {
		createValkey(t, app, seedOwner, map[string]any{
			"name": "fragment-" + string(rune('a'+index)), "vcpu": 2, "ram_gb": 8,
		})
	}
	beforeInstances := countRows(t, app, &store.ValkeyInstance{}, "user_id = ?", candidateOwner.ID)
	beforeAudit := countRows(t, app, &store.AuditLog{}, "user_id = ?", candidateOwner.ID)
	beforePeriods := countRows(t, app, &store.BillingPeriod{}, "user_id = ?", candidateOwner.ID)
	beforeKeys := countRows(t, app, &store.IdempotencyKey{}, "user_id = ?", candidateOwner.ID)

	response := createValkeyWithKey(t, app, candidateOwner, uuid.NewString(), map[string]any{
		"name": "does-not-fit", "prefix": "place", "vcpu": 2, "ram_gb": 8,
	})
	errorBody := assertError(t, response, http.StatusUnprocessableEntity, string(apierr.CodeNotEnoughResources))
	if errorBody.Error.Details["reason"] != "placement_capacity" {
		t.Fatalf("неверная причина: %+v", errorBody)
	}
	missing := errorBody.Error.Details["missing"].(map[string]any)
	if missing["vcpu"] != float64(0) || missing["ram_gb"] != float64(0) {
		t.Fatalf("общий дефицит при фрагментации не равен нулю: %+v", missing)
	}
	placement := errorBody.Error.Details["placement"].(map[string]any)
	if placement["node_count"] != float64(3) || placement["required_distinct_nodes"] != float64(1) {
		t.Fatalf("неверная топология в ответе: %+v", placement)
	}
	process := placement["process"].(map[string]any)
	nodeBudget := placement["node_budget"].(map[string]any)
	if process["cpu_milli"] != float64(2000) || process["ram_mib"] != float64(8192) ||
		nodeBudget["cpu_milli"] != float64(3000) || nodeBudget["ram_mib"] != float64(12288) {
		t.Fatalf("неверные единицы размещения: %+v", placement)
	}
	for _, secret := range []string{seedOwner.ID.String(), "fragment-a", "fragment-b", "fragment-c"} {
		if strings.Contains(string(response.Body), secret) {
			t.Fatalf("ответ раскрыл чужой инстанс %q: %s", secret, response.Body)
		}
	}
	if countRows(t, app, &store.ValkeyInstance{}, "user_id = ?", candidateOwner.ID) != beforeInstances ||
		countRows(t, app, &store.AuditLog{}, "user_id = ?", candidateOwner.ID) != beforeAudit ||
		countRows(t, app, &store.BillingPeriod{}, "user_id = ?", candidateOwner.ID) != beforePeriods ||
		countRows(t, app, &store.IdempotencyKey{}, "user_id = ?", candidateOwner.ID) != beforeKeys {
		t.Fatal("отказ по размещению оставил частичные строки")
	}
}

func Test_CreateValkey_WithHAAndOnlyTwoNodes_ReturnsPlacementCapacity(t *testing.T) {
	topology := valkeydomain.ClusterTopology{NodeCount: 2, NodeCPUMilli: 4000, NodeRAMMiB: 16384}
	app := newHTTPTestAPI(t, testAPIConfig{clusterTopology: &topology})
	owner := app.registerAccount(t, "")
	setUserQuota(t, app, owner.ID, 32, 128)

	response := createValkeyWithKey(t, app, owner, uuid.NewString(), map[string]any{
		"name": "ha-needs-three", "prefix": "place", "mode": "ha", "vcpu": 1, "ram_gb": 4,
	})
	errorBody := assertError(t, response, http.StatusUnprocessableEntity, string(apierr.CodeNotEnoughResources))
	if errorBody.Error.Details["reason"] != "placement_capacity" {
		t.Fatalf("неверная причина: %+v", errorBody)
	}
	placement := errorBody.Error.Details["placement"].(map[string]any)
	if placement["required_distinct_nodes"] != float64(3) {
		t.Fatalf("неверное число разных нод: %+v", placement)
	}
}

func Test_CreateValkey_WhenPersonalAndPlacementLimitsFail_ReturnsPersonalQuotaFirst(t *testing.T) {
	topology := valkeydomain.ClusterTopology{NodeCount: 2, NodeCPUMilli: 4000, NodeRAMMiB: 16384}
	app := newHTTPTestAPI(t, testAPIConfig{clusterTopology: &topology})
	owner := app.registerAccount(t, "")

	response := createValkeyWithKey(t, app, owner, uuid.NewString(), map[string]any{
		"name": "quota-first", "prefix": "place", "mode": "ha", "vcpu": 2, "ram_gb": 8,
	})
	errorBody := assertError(t, response, http.StatusUnprocessableEntity, string(apierr.CodeQuotaExceeded))
	if errorBody.Error.Details["reason"] != "user_quota" {
		t.Fatalf("неверный приоритет ошибки: %+v", errorBody)
	}
}

func Test_CreateValkey_WhenClusterAndPlacementLimitsFail_ReturnsClusterQuotaFirst(t *testing.T) {
	topology := valkeydomain.ClusterTopology{NodeCount: 2, NodeCPUMilli: 2000, NodeRAMMiB: 8192}
	app := newHTTPTestAPI(t, testAPIConfig{clusterTopology: &topology})
	owner := app.registerAccount(t, "")
	setUserQuota(t, app, owner.ID, 32, 128)

	response := createValkeyWithKey(t, app, owner, uuid.NewString(), map[string]any{
		"name": "cluster-first", "prefix": "place", "mode": "ha", "vcpu": 2, "ram_gb": 8,
	})
	errorBody := assertError(t, response, http.StatusUnprocessableEntity, string(apierr.CodeNotEnoughResources))
	if errorBody.Error.Details["reason"] != "cluster_quota" {
		t.Fatalf("неверный приоритет ошибки: %+v", errorBody)
	}
}

func Test_CreateValkey_WhenPlacementCheckTimesOut_ReturnsUnavailableWithoutPartialRows(t *testing.T) {
	topology := valkeydomain.ClusterTopology{
		NodeCount: 3, NodeCPUMilli: 4000, NodeRAMMiB: 16384, PlacementTimeout: -time.Nanosecond,
	}
	app := newHTTPTestAPI(t, testAPIConfig{clusterTopology: &topology})
	owner := app.registerAccount(t, "")
	key := uuid.NewString()

	response := createValkeyWithKey(t, app, owner, key, map[string]any{
		"name": "timeout", "prefix": "place", "vcpu": 1, "ram_gb": 1,
	})
	errorBody := assertError(t, response, http.StatusServiceUnavailable, string(apierr.CodeUnavailable))
	if errorBody.Error.Details["reason"] != "placement_check_timeout" {
		t.Fatalf("неверная причина: %+v", errorBody)
	}
	assertDatabaseCount(t, app.database.DB().Model(&store.ValkeyInstance{}).Where("user_id = ?", owner.ID), 0)
	assertDatabaseCount(t, app.database.DB().Model(&store.IdempotencyKey{}).Where("key = ?", key), 0)
}

func Test_ResizeValkey_WhenReplacingOwnReservation_DoesNotCountItTwice(t *testing.T) {
	topology := valkeydomain.ClusterTopology{NodeCount: 1, NodeCPUMilli: 2000, NodeRAMMiB: 8192}
	app := newHTTPTestAPI(t, testAPIConfig{clusterTopology: &topology})
	owner := app.registerAccount(t, "")
	setUserQuota(t, app, owner.ID, 8, 32)
	instance := createValkey(t, app, owner, map[string]any{"name": "replace-reserve", "vcpu": 1, "ram_gb": 4})
	makeValkeyReady(t, app, instance.ID)

	response := app.requestJSON(
		t,
		http.MethodPost,
		instancePath(instance.ID)+"/resize",
		map[string]any{"vcpu": 2, "ram_gb": 8},
		mergeHeaders(bearer(owner.Token), map[string]string{"Idempotency-Key": uuid.NewString()}),
	)

	assertStatus(t, response, http.StatusAccepted)
}

func Test_CreateValkey_AfterUnconfirmedReduction_HoldsOldPlacementUntilAppliedSizeChanges(t *testing.T) {
	topology := valkeydomain.ClusterTopology{NodeCount: 2, NodeCPUMilli: 2500, NodeRAMMiB: 10240}
	app := newHTTPTestAPI(t, testAPIConfig{clusterTopology: &topology})
	owner := app.registerAccount(t, "")
	setUserQuota(t, app, owner.ID, 32, 128)
	shrinking := createValkey(t, app, owner, map[string]any{
		"name": "shrinking-reserve", "vcpu": 2, "ram_gb": 8,
	})
	makeValkeyReady(t, app, shrinking.ID)
	createValkey(t, app, owner, map[string]any{"name": "companion", "vcpu": 1, "ram_gb": 4})

	resize := app.requestJSON(
		t,
		http.MethodPost,
		instancePath(shrinking.ID)+"/resize",
		map[string]any{"vcpu": 1, "ram_gb": 4},
		mergeHeaders(bearer(owner.Token), map[string]string{"Idempotency-Key": uuid.NewString()}),
	)
	assertStatus(t, resize, http.StatusAccepted)

	rejected := createValkeyWithKey(t, app, owner, uuid.NewString(), map[string]any{
		"name": "held-capacity", "vcpu": 2, "ram_gb": 8,
	})
	errorBody := assertError(t, rejected, http.StatusUnprocessableEntity, string(apierr.CodeNotEnoughResources))
	if errorBody.Error.Details["reason"] != "placement_capacity" {
		t.Fatalf("неподтверждённое уменьшение освободило размещение: %+v", errorBody)
	}

	if err := app.database.DB().
		Model(&store.ValkeyInstance{}).
		Where("id = ?", shrinking.ID).
		UpdateColumns(map[string]any{
			"applied_vcpu": 1, "applied_ram_gb": 4,
		}).
		Error; err != nil {
		t.Fatalf("подтвердить применённый размер: %v", err)
	}
	accepted := createValkeyWithKey(t, app, owner, uuid.NewString(), map[string]any{
		"name": "released-capacity", "vcpu": 2, "ram_gb": 8,
	})
	assertStatus(t, accepted, http.StatusAccepted)
}

func Test_ResizeValkey_AfterClusterBudgetDecrease_AllowsNonIncreasingReservation(t *testing.T) {
	largeTopology := valkeydomain.ClusterTopology{NodeCount: 1, NodeCPUMilli: 4000, NodeRAMMiB: 16384}
	large := newHTTPTestAPI(t, testAPIConfig{clusterTopology: &largeTopology})
	owner := large.registerAccount(t, "")
	setUserQuota(t, large, owner.ID, 8, 32)
	instance := createValkey(t, large, owner, map[string]any{
		"name": "over-new-budget", "vcpu": 2, "ram_gb": 8,
	})
	makeValkeyReady(t, large, instance.ID)

	smallTopology := valkeydomain.ClusterTopology{NodeCount: 1, NodeCPUMilli: 1000, NodeRAMMiB: 4096}
	small := newHTTPTestAPI(t, testAPIConfig{clusterTopology: &smallTopology})
	response := small.requestJSON(
		t,
		http.MethodPost,
		instancePath(instance.ID)+"/resize",
		map[string]any{"vcpu": 1, "ram_gb": 4},
		mergeHeaders(bearer(owner.Token), map[string]string{"Idempotency-Key": uuid.NewString()}),
	)

	assertStatus(t, response, http.StatusAccepted)
}

func Test_CreateValkey_WhenExistingInstanceIsErrorOrDeleting_HoldsCapacityUntilDeletedAt(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *testAPI, testAccount, uuid.UUID)
	}{
		{
			name: "фаза error удерживает резерв",
			prepare: func(t *testing.T, app *testAPI, _ testAccount, instanceID uuid.UUID) {
				t.Helper()
				if err := app.database.DB().Model(&store.ValkeyInstance{}).Where("id = ?", instanceID).
					UpdateColumn("phase", "error").Error; err != nil {
					t.Fatalf("подготовить фазу error: %v", err)
				}
			},
		},
		{
			name: "запрос DELETE удерживает резерв",
			prepare: func(t *testing.T, app *testAPI, owner testAccount, instanceID uuid.UUID) {
				t.Helper()
				response := app.requestJSON(t, http.MethodDelete, instancePath(instanceID), nil, bearer(owner.Token))
				assertStatus(t, response, http.StatusAccepted)
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			topology := valkeydomain.ClusterTopology{NodeCount: 1, NodeCPUMilli: 1000, NodeRAMMiB: 1024}
			app := newHTTPTestAPI(t, testAPIConfig{clusterTopology: &topology})
			owner := app.registerAccount(t, "")
			candidateOwner := app.registerAccount(t, "")
			instance := createValkey(t, app, owner, map[string]any{"name": "held-reserve"})
			testCase.prepare(t, app, owner, instance.ID)

			rejected := createValkeyWithKey(t, app, candidateOwner, uuid.NewString(), map[string]any{
				"name": "before-deleted-at",
			})
			errorBody := assertError(
				t,
				rejected,
				http.StatusUnprocessableEntity,
				string(apierr.CodeNotEnoughResources),
			)
			if errorBody.Error.Details["reason"] != "cluster_quota" {
				t.Fatalf("неверная причина удержания резерва: %+v", errorBody)
			}

			if loadValkey(t, app, instance.ID).DeletionRequestedAt == nil {
				response := app.requestJSON(t, http.MethodDelete, instancePath(instance.ID), nil, bearer(owner.Token))
				assertStatus(t, response, http.StatusAccepted)
			}
			deletedAt := time.Now().UTC()
			if err := app.database.DB().Model(&store.ValkeyInstance{}).Where("id = ?", instance.ID).
				UpdateColumn("deleted_at", deletedAt).Error; err != nil {
				t.Fatalf("подтвердить удаление: %v", err)
			}
			accepted := createValkeyWithKey(t, app, candidateOwner, uuid.NewString(), map[string]any{
				"name": "after-deleted-at",
			})
			assertStatus(t, accepted, http.StatusAccepted)
		})
	}
}

func Test_CreateValkey_WithConcurrentIdempotentPlacementRetry_ReservesOnce(t *testing.T) {
	topology := valkeydomain.ClusterTopology{NodeCount: 1, NodeCPUMilli: 1000, NodeRAMMiB: 1024}
	config := testAPIConfig{clusterTopology: &topology}
	first := newHTTPTestAPI(t, config)
	second := newHTTPTestAPI(t, config)
	owner := first.registerAccount(t, "")
	key := uuid.NewString()
	request := createSizedRequest(first, owner, "same-placement", 1, 1, key)
	repeated := request
	repeated.app = second

	responses := runConcurrentRequests(t, request, repeated)

	assertStatuses(t, responses, http.StatusAccepted, http.StatusAccepted)
	if string(responses[0].Body) != string(responses[1].Body) {
		t.Fatalf("идемпотентный повтор вернул разные ответы: %s != %s", responses[0].Body, responses[1].Body)
	}
	assertDatabaseCount(t, first.database.DB().Model(&store.ValkeyInstance{}).Where("user_id = ?", owner.ID), 1)
	assertDatabaseCount(t, first.database.DB().Model(&store.IdempotencyKey{}).Where("user_id = ?", owner.ID), 1)
}

func createValkeyWithKey(
	t *testing.T,
	app *testAPI,
	account testAccount,
	key string,
	overrides map[string]any,
) testResponse {
	t.Helper()

	body := map[string]any{
		"name": "cache-" + uuid.NewString()[:8], "prefix": "test", "mode": "single",
		"vcpu": 1, "ram_gb": 1, "password": testValkeyPassword,
		"is_whitelist_enabled": false, "whitelist_cidrs": []string{},
	}
	for field, value := range overrides {
		body[field] = value
	}

	return app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", body, mergeHeaders(
		bearer(account.Token),
		map[string]string{"Idempotency-Key": key},
	))
}
