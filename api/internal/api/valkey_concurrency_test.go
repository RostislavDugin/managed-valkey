package api_test

import (
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

type concurrentRequest struct {
	app     *testAPI
	method  string
	path    string
	body    string
	headers map[string]string
}

func Test_CreateValkeys_WithConcurrentRequestsAtResourceLimits_RespectsClusterAndPersonalQuotas(t *testing.T) {
	t.Run(
		"конкурентное создание двумя владельцами принимает один запрос в пределах общего остатка",
		func(t *testing.T) {
			first := newHTTPTestAPI(t, testAPIConfig{clusterVCPU: 1, clusterRAMGB: 1})
			second := newHTTPTestAPI(t, testAPIConfig{clusterVCPU: 1, clusterRAMGB: 1})
			firstOwner := first.registerAccount(t, "")
			secondOwner := first.registerAccount(t, "")

			responses := runConcurrentRequests(t,
				createRequest(first, firstOwner, "cluster-first", uuid.NewString()),
				createRequest(second, secondOwner, "cluster-second", uuid.NewString()),
			)
			assertStatuses(t, responses, http.StatusAccepted, http.StatusUnprocessableEntity)
			assertOneErrorCode(t, responses, apierr.CodeNotEnoughResources)
			assertDatabaseCount(t, first.database.DB().Model(&store.ValkeyInstance{}).
				Where("user_id IN ?", []uuid.UUID{firstOwner.ID, secondOwner.ID}), 1)
		},
	)

	t.Run(
		"конкурентное создание одним владельцем принимает один запрос в пределах личного остатка",
		func(t *testing.T) {
			first := newHTTPTestAPI(t, testAPIConfig{})
			second := newHTTPTestAPI(t, testAPIConfig{})
			owner := first.registerAccount(t, "")
			setUserQuota(t, first, owner.ID, 1, 1)

			responses := runConcurrentRequests(t,
				createRequest(first, owner, "user-first", uuid.NewString()),
				createRequest(second, owner, "user-second", uuid.NewString()),
			)
			assertStatuses(t, responses, http.StatusAccepted, http.StatusUnprocessableEntity)
			assertOneErrorCode(t, responses, apierr.CodeQuotaExceeded)
			assertDatabaseCount(t, first.database.DB().Model(&store.ValkeyInstance{}).Where("user_id = ?", owner.ID), 1)
		},
	)
}

func Test_CreateAndResizeValkey_WithConcurrentRequestsAtClusterLimit_ShareClusterBudget(t *testing.T) {
	config := testAPIConfig{clusterVCPU: 2, clusterRAMGB: 8}
	first := newHTTPTestAPI(t, config)
	second := newHTTPTestAPI(t, config)
	resizeOwner := first.registerAccount(t, "")
	createOwner := first.registerAccount(t, "")
	setUserQuota(t, first, resizeOwner.ID, 8, 32)
	setUserQuota(t, first, createOwner.ID, 8, 32)
	existing := createValkey(t, first, resizeOwner, map[string]any{"name": "resize-existing"})
	makeValkeyReady(t, first, existing.ID)

	responses := runConcurrentRequests(t,
		concurrentRequest{
			app: first, method: http.MethodPost,
			path: "/v1/managed/valkey/instances/" + existing.ID.String() + "/resize",
			body: `{"vcpu":2,"ram_gb":8}`,
			headers: mergeHeaders(bearer(resizeOwner.Token), map[string]string{
				"Content-Type": "application/json", "Idempotency-Key": uuid.NewString(),
			}),
		},
		createRequest(second, createOwner, "create-competing", uuid.NewString()),
	)
	assertStatuses(t, responses, http.StatusAccepted, http.StatusUnprocessableEntity)
	assertOneErrorCode(t, responses, apierr.CodeNotEnoughResources)

	var usedVCPU int
	if err := first.database.DB().Raw(`
		SELECT COALESCE(SUM(GREATEST(vcpu, applied_vcpu)), 0)
		FROM valkey_instances
		WHERE user_id IN ? AND deleted_at IS NULL`, []uuid.UUID{resizeOwner.ID, createOwner.ID}).Scan(&usedVCPU).Error; err != nil {
		t.Fatalf("посчитать общий резерв: %v", err)
	}
	if usedVCPU > 2 {
		t.Fatalf("конкурентные запросы превысили общий резерв: %d", usedVCPU)
	}
}

func Test_CreateValkeys_WithConcurrentRequestsAtPlacementLimit_AcceptsOnlyOne(t *testing.T) {
	topology := valkeydomain.ClusterTopology{NodeCount: 3, NodeCPUMilli: 3000, NodeRAMMiB: 12288}
	config := testAPIConfig{clusterTopology: &topology}
	first := newHTTPTestAPI(t, config)
	second := newHTTPTestAPI(t, config)
	seedOwner := first.registerAccount(t, "")
	firstCandidate := first.registerAccount(t, "")
	secondCandidate := first.registerAccount(t, "")
	setUserQuota(t, first, seedOwner.ID, 32, 128)
	setUserQuota(t, first, firstCandidate.ID, 8, 32)
	setUserQuota(t, first, secondCandidate.ID, 8, 32)
	createValkey(t, first, seedOwner, map[string]any{"name": "placement-seed-one", "vcpu": 2, "ram_gb": 8})
	createValkey(t, first, seedOwner, map[string]any{"name": "placement-seed-two", "vcpu": 2, "ram_gb": 8})

	responses := runConcurrentRequests(t,
		createSizedRequest(first, firstCandidate, "placement-first", 2, 8, uuid.NewString()),
		createSizedRequest(second, secondCandidate, "placement-second", 2, 8, uuid.NewString()),
	)

	assertStatuses(t, responses, http.StatusAccepted, http.StatusUnprocessableEntity)
	assertOnePlacementCapacityError(t, responses)
	assertDatabaseCount(t, first.database.DB().Model(&store.ValkeyInstance{}).Where(
		"user_id IN ?", []uuid.UUID{firstCandidate.ID, secondCandidate.ID},
	), 1)
	assertDatabaseCount(t, first.database.DB().Model(&store.IdempotencyKey{}).Where(
		"user_id IN ?", []uuid.UUID{firstCandidate.ID, secondCandidate.ID},
	), 1)
}

func Test_CreateAndResizeValkey_WithConcurrentRequestsAtPlacementLimit_AcceptsOnlyOne(t *testing.T) {
	topology := valkeydomain.ClusterTopology{NodeCount: 3, NodeCPUMilli: 3000, NodeRAMMiB: 12288}
	config := testAPIConfig{clusterTopology: &topology}
	first := newHTTPTestAPI(t, config)
	second := newHTTPTestAPI(t, config)
	fixedOwner := first.registerAccount(t, "")
	resizeOwner := first.registerAccount(t, "")
	createOwner := first.registerAccount(t, "")
	setUserQuota(t, first, fixedOwner.ID, 32, 128)
	setUserQuota(t, first, resizeOwner.ID, 8, 32)
	setUserQuota(t, first, createOwner.ID, 8, 32)
	createValkey(t, first, fixedOwner, map[string]any{"name": "fixed-one", "vcpu": 2, "ram_gb": 8})
	createValkey(t, first, fixedOwner, map[string]any{"name": "fixed-two", "vcpu": 2, "ram_gb": 8})
	resizable := createValkey(t, first, resizeOwner, map[string]any{
		"name": "resizable", "vcpu": 1, "ram_gb": 4,
	})
	makeValkeyReady(t, first, resizable.ID)

	responses := runConcurrentRequests(t,
		resizeRequest(2, 8)(first, resizeOwner, resizable.ID),
		createSizedRequest(second, createOwner, "create-competing-for-node", 2, 8, uuid.NewString()),
	)

	assertStatuses(t, responses, http.StatusAccepted, http.StatusUnprocessableEntity)
	assertOnePlacementCapacityError(t, responses)
	created := countRows(t, first, &store.ValkeyInstance{}, "user_id = ?", createOwner.ID)
	resized := loadValkey(t, first, resizable.ID)
	if (created == 1 && resized.VCPU != 1) || (created == 0 && resized.VCPU != 2) {
		t.Fatalf("приняты обе операции или ни одной: создано=%d, размер=%d/%d", created, resized.VCPU, resized.RAMGB)
	}
}

func Test_MutateValkeyConfiguration_WithConcurrentRequests_AcceptsOneGeneration(t *testing.T) {
	tests := []struct {
		name   string
		first  func(*testAPI, testAccount, uuid.UUID) concurrentRequest
		second func(*testAPI, testAccount, uuid.UUID) concurrentRequest
	}{
		{
			name:   "два конкурентных изменения размера увеличивают поколение один раз",
			first:  resizeRequest(2, 8),
			second: resizeRequest(4, 16),
		},
		{
			name:   "одновременные изменения размера и списка доступа увеличивают поколение один раз",
			first:  resizeRequest(2, 8),
			second: whitelistRequest("192.0.2.0/24"),
		},
		{
			name:   "одновременное изменение списка доступа и смена пароля увеличивают поколение один раз",
			first:  whitelistRequest("192.0.2.0/24"),
			second: rotateRequest(rotatedValkeyPassword),
		},
		{
			name:   "две конкурентные смены пароля увеличивают поколение один раз",
			first:  rotateRequest(rotatedValkeyPassword),
			second: rotateRequest("abcdefghijklmnopqrstuvwxyz012345"),
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			first := newHTTPTestAPI(t, testAPIConfig{})
			second := newHTTPTestAPI(t, testAPIConfig{})
			owner := first.registerAccount(t, "")
			setUserQuota(t, first, owner.ID, 16, 64)
			instance := createValkey(t, first, owner, map[string]any{"name": "config-race"})
			ready := makeValkeyReady(t, first, instance.ID)

			responses := runConcurrentRequests(t,
				testCase.first(first, owner, instance.ID),
				testCase.second(second, owner, instance.ID),
			)
			assertStatuses(t, responses, http.StatusAccepted, http.StatusConflict)
			assertOneErrorCode(t, responses, apierr.CodeOperationInProgress)
			after := loadValkey(t, first, instance.ID)
			if after.DesiredGeneration != ready.DesiredGeneration+1 {
				t.Fatalf("поколение выросло до %d вместо %d", after.DesiredGeneration, ready.DesiredGeneration+1)
			}
			assertDatabaseCount(
				t,
				first.database.DB().Model(&store.AuditLog{}).
					Where("resource_id = ? AND action <> ?", instance.ID, audit.ActionInstanceCreate),
				1,
			)
		})
	}
}

func Test_CreateValkey_WithConcurrentIdempotentRequests_CommitsOnce(t *testing.T) {
	first := newHTTPTestAPI(t, testAPIConfig{})
	second := newHTTPTestAPI(t, testAPIConfig{})
	owner := first.registerAccount(t, "")
	key := uuid.NewString()
	request := createRequest(first, owner, "same-request", key)
	requestOnSecond := request
	requestOnSecond.app = second

	responses := runConcurrentRequests(t, request, requestOnSecond)
	assertStatuses(t, responses, http.StatusAccepted, http.StatusAccepted)
	if string(responses[0].Body) != string(responses[1].Body) {
		t.Fatalf("конкурентный повтор вернул разные ответы: %s != %s", responses[0].Body, responses[1].Body)
	}
	assertDatabaseCount(t, first.database.DB().Model(&store.ValkeyInstance{}).Where("user_id = ?", owner.ID), 1)
	assertDatabaseCount(t, first.database.DB().Model(&store.IdempotencyKey{}).Where("user_id = ?", owner.ID), 1)

	mismatchKey := uuid.NewString()
	responses = runConcurrentRequests(t,
		createRequest(first, owner, "mismatch-first", mismatchKey),
		createRequest(second, owner, "mismatch-second", mismatchKey),
	)
	assertStatuses(t, responses, http.StatusAccepted, http.StatusUnprocessableEntity)
	assertOneErrorCode(t, responses, apierr.CodeIdempotencyMismatch)
}

func Test_RenameOrDeleteValkey_WithConcurrentRequests_DoesNotLoseChanges(t *testing.T) {
	t.Run("конкурентное переименование двух инстансов сохраняет новое имя только у одного", func(t *testing.T) {
		first := newHTTPTestAPI(t, testAPIConfig{})
		second := newHTTPTestAPI(t, testAPIConfig{})
		owner := first.registerAccount(t, "")
		setUserQuota(t, first, owner.ID, 8, 32)
		one := createValkey(t, first, owner, map[string]any{"name": "rename-one"})
		two := createValkey(t, first, owner, map[string]any{"name": "rename-two"})

		responses := runConcurrentRequests(t,
			patchNameRequest(first, owner, one.ID, "same-name"),
			patchNameRequest(second, owner, two.ID, "same-name"),
		)
		assertStatuses(t, responses, http.StatusOK, http.StatusConflict)
		assertOneErrorCode(t, responses, apierr.CodeConflict)
		assertDatabaseCount(t, first.database.DB().Model(&store.ValkeyInstance{}).
			Where("user_id = ? AND name = ?", owner.ID, "same-name"), 1)
	})

	t.Run("два конкурентных запроса удаления создают по одной записи аудита и биллинга", func(t *testing.T) {
		first := newHTTPTestAPI(t, testAPIConfig{})
		second := newHTTPTestAPI(t, testAPIConfig{})
		owner := first.registerAccount(t, "")
		instance := createValkey(t, first, owner, map[string]any{"name": "delete-race"})
		path := "/v1/managed/valkey/instances/" + instance.ID.String()

		responses := runConcurrentRequests(t,
			concurrentRequest{app: first, method: http.MethodDelete, path: path, headers: bearer(owner.Token)},
			concurrentRequest{app: second, method: http.MethodDelete, path: path, headers: bearer(owner.Token)},
		)
		assertStatuses(t, responses, http.StatusAccepted, http.StatusAccepted)
		assertDatabaseCount(t, first.database.DB().Model(&store.AuditLog{}).Where(
			"resource_id = ? AND action = ?", instance.ID, audit.ActionInstanceDelete,
		), 1)
		assertDatabaseCount(t, first.database.DB().Model(&store.BillingPeriod{}).Where(
			"resource_id = ? AND ended_reason = ?", instance.ID, "deleted",
		), 1)
	})
}

func Test_MutateValkeyConfiguration_AfterDeleteRequest_RejectsEveryMutation(t *testing.T) {
	first := newHTTPTestAPI(t, testAPIConfig{})
	second := newHTTPTestAPI(t, testAPIConfig{})
	owner := first.registerAccount(t, "")
	setUserQuota(t, first, owner.ID, 16, 64)
	builders := []func(*testAPI, testAccount, uuid.UUID) concurrentRequest{
		resizeRequest(2, 8),
		whitelistRequest("192.0.2.0/24"),
		rotateRequest(rotatedValkeyPassword),
	}

	for index, build := range builders {
		instance := createValkey(t, first, owner, map[string]any{"name": fmt.Sprintf("delete-first-%d", index)})
		makeValkeyReady(t, first, instance.ID)
		path := "/v1/managed/valkey/instances/" + instance.ID.String()
		assertStatus(t, first.requestJSON(t, http.MethodDelete, path, nil, bearer(owner.Token)), http.StatusAccepted)

		request := build(second, owner, instance.ID)
		response, err := request.app.do(request.method, request.path, []byte(request.body), request.headers)
		if err != nil {
			t.Fatalf("выполнить мутацию после DELETE: %v", err)
		}
		errorBody := assertError(t, response, http.StatusConflict, string(apierr.CodeInstanceNotReady))
		if errorBody.Error.Details["reason"] != "deleting" {
			t.Fatalf("неверная ошибка после DELETE: %+v", errorBody)
		}
	}
}

func Test_DeleteValkey_WhenQueuedBeforeConfigurationMutation_TakesPrecedence(t *testing.T) {
	builders := []struct {
		name  string
		build func(*testAPI, testAccount, uuid.UUID) concurrentRequest
	}{
		{name: "PATCH", build: func(app *testAPI, owner testAccount, instanceID uuid.UUID) concurrentRequest {
			return patchNameRequest(app, owner, instanceID, "queued-patch")
		}},
		{name: "resize", build: resizeRequest(2, 8)},
		{name: "whitelist", build: whitelistRequest("192.0.2.0/24")},
		{name: "rotate", build: rotateRequest(rotatedValkeyPassword)},
	}

	for _, testCase := range builders {
		t.Run(testCase.name, func(t *testing.T) {
			first := newHTTPTestAPI(t, testAPIConfig{})
			second := newHTTPTestAPI(t, testAPIConfig{})
			owner := first.registerAccount(t, "")
			setUserQuota(t, first, owner.ID, 16, 64)
			instance := createValkey(t, first, owner, map[string]any{"name": "queued-delete"})
			makeValkeyReady(t, first, instance.ID)
			holder := holdValkeyLock(t, first)

			deleteResponses, deleteErrors := startConcurrentRequest(concurrentRequest{
				app: first, method: http.MethodDelete, path: instancePath(instance.ID), headers: bearer(owner.Token),
			})
			waitForValkeyLockWaiters(t, first, 1)
			mutationResponses, mutationErrors := startConcurrentRequest(testCase.build(second, owner, instance.ID))
			waitForValkeyLockWaiters(t, first, 2)
			if err := holder.Commit(); err != nil {
				t.Fatalf("освободить очередь мутаций: %v", err)
			}

			assertStatus(t, receiveHTTPResult(t, deleteResponses, deleteErrors), http.StatusAccepted)
			failed := receiveHTTPResult(t, mutationResponses, mutationErrors)
			errorBody := assertError(t, failed, http.StatusConflict, string(apierr.CodeInstanceNotReady))
			if errorBody.Error.Details["reason"] != "deleting" {
				t.Fatalf("ожидавшая мутация не увидела DELETE: %+v", errorBody)
			}
			assertDatabaseCount(t, first.database.DB().Model(&store.AuditLog{}).Where(
				"resource_id = ? AND action = ?", instance.ID, audit.ActionInstanceDelete,
			), 1)
		})
	}
}

func Test_DeleteValkey_WhenQueuedAfterResize_PreservesResizeAndClosesNewBillingPeriod(t *testing.T) {
	first := newHTTPTestAPI(t, testAPIConfig{})
	second := newHTTPTestAPI(t, testAPIConfig{})
	owner := first.registerAccount(t, "")
	setUserQuota(t, first, owner.ID, 16, 64)
	instance := createValkey(t, first, owner, map[string]any{"name": "resize-before-delete"})
	makeValkeyReady(t, first, instance.ID)
	holder := holdValkeyLock(t, first)

	resizeResponses, resizeErrors := startConcurrentRequest(resizeRequest(2, 8)(first, owner, instance.ID))
	waitForValkeyLockWaiters(t, first, 1)
	deleteResponses, deleteErrors := startConcurrentRequest(concurrentRequest{
		app: second, method: http.MethodDelete, path: instancePath(instance.ID), headers: bearer(owner.Token),
	})
	waitForValkeyLockWaiters(t, first, 2)
	if err := holder.Commit(); err != nil {
		t.Fatalf("освободить очередь resize и DELETE: %v", err)
	}

	assertStatus(t, receiveHTTPResult(t, resizeResponses, resizeErrors), http.StatusAccepted)
	assertStatus(t, receiveHTTPResult(t, deleteResponses, deleteErrors), http.StatusAccepted)
	deleted := loadValkey(t, first, instance.ID)
	if deleted.DeletionRequestedAt == nil || deleted.VCPU != 2 || deleted.RAMGB != 8 {
		t.Fatalf("DELETE не сохранил принятый resize: %+v", deleted)
	}

	var periods []store.BillingPeriod
	if err := first.database.DB().
		Where("resource_id = ?", instance.ID).
		Order("started_at, id").
		Find(&periods).
		Error; err != nil {
		t.Fatalf("прочитать периоды после resize и DELETE: %v", err)
	}
	if len(periods) != 2 || periods[0].EndedReason == nil || *periods[0].EndedReason != "resized" ||
		periods[1].StartedReason != "resized" || periods[1].EndedReason == nil ||
		*periods[1].EndedReason != "deleted" || periods[1].EndedAt == nil ||
		!periods[1].EndedAt.Equal(*deleted.DeletionRequestedAt) {
		t.Fatalf("DELETE закрыл не новый период: %+v", periods)
	}
	me := first.requestJSON(t, http.MethodGet, "/v1/me", nil, bearer(owner.Token))
	usage := decodeResponse[currentUserResponse](t, me).Usage
	if usage.UsedVCPU != 2 || usage.UsedRAMGB != 8 {
		t.Fatalf("DELETE преждевременно освободил квоту: %+v", usage)
	}
}

func Test_CreateValkey_WithConcurrentRequestsAtInstanceLimit_AcceptsOnlyRemainingSlot(t *testing.T) {
	first := newHTTPTestAPI(t, testAPIConfig{})
	second := newHTTPTestAPI(t, testAPIConfig{})
	seedOwner := first.registerAccount(t, "")
	firstCandidate := first.registerAccount(t, "")
	secondCandidate := first.registerAccount(t, "")
	setUserQuota(t, first, seedOwner.ID, 128, 512)

	for index := range 31 {
		createValkey(t, first, seedOwner, map[string]any{"name": fmt.Sprintf("seed-%02d", index)})
	}
	responses := runConcurrentRequests(t,
		createRequest(first, firstCandidate, "candidate-one", uuid.NewString()),
		createRequest(second, secondCandidate, "candidate-two", uuid.NewString()),
	)
	assertStatuses(t, responses, http.StatusAccepted, http.StatusUnprocessableEntity)
	assertOneErrorCode(t, responses, apierr.CodeNotEnoughResources)

	var count int64
	if err := first.database.DB().Model(&store.ValkeyInstance{}).Where(
		"user_id IN ? AND deleted_at IS NULL",
		[]uuid.UUID{seedOwner.ID, firstCandidate.ID, secondCandidate.ID},
	).Count(&count).Error; err != nil {
		t.Fatalf("посчитать инстансы трёх владельцев: %v", err)
	}
	if count != 32 {
		t.Fatalf("активных инстансов %d, ожидалось 32", count)
	}
}

func createRequest(app *testAPI, owner testAccount, name, key string) concurrentRequest {
	return createSizedRequest(app, owner, name, 1, 1, key)
}

func createSizedRequest(
	app *testAPI,
	owner testAccount,
	name string,
	vcpu int,
	ramGB int,
	key string,
) concurrentRequest {
	return concurrentRequest{
		app: app, method: http.MethodPost, path: "/v1/managed/valkey/instances",
		body: fmt.Sprintf(
			`{"name":%q,"prefix":"race","mode":"single","vcpu":%d,"ram_gb":%d,"password":%q}`,
			name,
			vcpu,
			ramGB,
			testValkeyPassword,
		),
		headers: mergeHeaders(bearer(owner.Token), map[string]string{
			"Content-Type": "application/json", "Idempotency-Key": key,
		}),
	}
}

func assertOnePlacementCapacityError(t *testing.T, responses []testResponse) {
	t.Helper()

	for _, response := range responses {
		if response.StatusCode < 400 {
			continue
		}
		errorBody := assertError(t, response, http.StatusUnprocessableEntity, string(apierr.CodeNotEnoughResources))
		if errorBody.Error.Details["reason"] != "placement_capacity" {
			t.Fatalf("неверная причина отказа: %+v", errorBody)
		}

		return
	}
	t.Fatal("ответ с ошибкой размещения отсутствует")
}

func resizeRequest(vcpu, ramGB int) func(*testAPI, testAccount, uuid.UUID) concurrentRequest {
	return func(app *testAPI, owner testAccount, instanceID uuid.UUID) concurrentRequest {
		return concurrentRequest{
			app: app, method: http.MethodPost,
			path: "/v1/managed/valkey/instances/" + instanceID.String() + "/resize",
			body: fmt.Sprintf(`{"vcpu":%d,"ram_gb":%d}`, vcpu, ramGB),
			headers: mergeHeaders(bearer(owner.Token), map[string]string{
				"Content-Type": "application/json", "Idempotency-Key": uuid.NewString(),
			}),
		}
	}
}

func whitelistRequest(cidr string) func(*testAPI, testAccount, uuid.UUID) concurrentRequest {
	return func(app *testAPI, owner testAccount, instanceID uuid.UUID) concurrentRequest {
		return concurrentRequest{
			app: app, method: http.MethodPut,
			path:    "/v1/managed/valkey/instances/" + instanceID.String() + "/whitelist",
			body:    fmt.Sprintf(`{"is_whitelist_enabled":true,"whitelist_cidrs":[%q]}`, cidr),
			headers: mergeHeaders(bearer(owner.Token), map[string]string{"Content-Type": "application/json"}),
		}
	}
}

func rotateRequest(password string) func(*testAPI, testAccount, uuid.UUID) concurrentRequest {
	return func(app *testAPI, owner testAccount, instanceID uuid.UUID) concurrentRequest {
		return concurrentRequest{
			app: app, method: http.MethodPost,
			path: "/v1/managed/valkey/instances/" + instanceID.String() + "/credentials/rotate",
			body: fmt.Sprintf(`{"password":%q,"expected_password_version":1}`, password),
			headers: mergeHeaders(bearer(owner.Token), map[string]string{
				"Content-Type": "application/json", "Idempotency-Key": uuid.NewString(),
			}),
		}
	}
}

func patchNameRequest(app *testAPI, owner testAccount, instanceID uuid.UUID, name string) concurrentRequest {
	return concurrentRequest{
		app: app, method: http.MethodPatch,
		path:    "/v1/managed/valkey/instances/" + instanceID.String(),
		body:    fmt.Sprintf(`{"name":%q}`, name),
		headers: mergeHeaders(bearer(owner.Token), map[string]string{"Content-Type": "application/json"}),
	}
}

func runConcurrentRequests(t *testing.T, requests ...concurrentRequest) []testResponse {
	t.Helper()

	start := make(chan struct{})
	responses := make(chan testResponse, len(requests))
	errorsReceived := make(chan error, len(requests))
	for _, request := range requests {
		go func() {
			<-start
			response, err := request.app.do(
				request.method,
				request.path,
				[]byte(request.body),
				request.headers,
			)
			if err != nil {
				errorsReceived <- err

				return
			}
			responses <- response
		}()
	}
	close(start)

	result := make([]testResponse, 0, len(requests))
	for range requests {
		select {
		case err := <-errorsReceived:
			t.Fatalf("выполнить конкурентный запрос: %v", err)
		case response := <-responses:
			result = append(result, response)
		}
	}

	return result
}

func startConcurrentRequest(request concurrentRequest) (<-chan testResponse, <-chan error) {
	responses := make(chan testResponse, 1)
	errorsReceived := make(chan error, 1)
	go func() {
		response, err := request.app.do(request.method, request.path, []byte(request.body), request.headers)
		if err != nil {
			errorsReceived <- err

			return
		}
		responses <- response
	}()

	return responses, errorsReceived
}

func assertStatuses(t *testing.T, responses []testResponse, want ...int) {
	t.Helper()

	got := make([]int, 0, len(responses))
	for _, response := range responses {
		got = append(got, response.StatusCode)
	}
	sort.Ints(got)
	sort.Ints(want)
	if len(got) != len(want) {
		t.Fatalf("статусы %v, ожидались %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("статусы %v, ожидались %v", got, want)
		}
	}
}

func assertOneErrorCode(t *testing.T, responses []testResponse, code apierr.Code) {
	t.Helper()

	for _, response := range responses {
		if response.StatusCode < 400 {
			continue
		}
		if got := decodeResponse[errorResponse](t, response).Error.Code; got != string(code) {
			t.Fatalf("код ошибки %q, ожидался %q", got, code)
		}

		return
	}

	t.Fatalf("в ответах нет ошибки %s", code)
}
