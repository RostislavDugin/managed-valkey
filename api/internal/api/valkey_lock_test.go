package api_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

func Test_AccessValkey_WhenAdvisoryLockIsHeld_AllowsReadAndTimesOutMutation(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	created := createValkey(t, app, account, map[string]any{"name": "locked-cache"})
	holder := holdValkeyLock(t, app)
	base := "/v1/managed/valkey/instances/" + created.ID.String()

	getContext, cancelGet := context.WithTimeout(t.Context(), time.Second)
	defer cancelGet()
	getResponse, err := app.doWithContext(getContext, http.MethodGet, base, nil, bearer(account.Token))
	if err != nil {
		t.Fatalf("GET под общей блокировкой: %v", err)
	}
	assertStatus(t, getResponse, http.StatusOK)

	started := time.Now()
	mutation := app.requestJSON(
		t,
		http.MethodPatch,
		base,
		map[string]any{"name": "still-locked"},
		bearer(account.Token),
	)
	if time.Since(started) < 2900*time.Millisecond {
		t.Fatalf("мутация не дождалась lock_timeout: %s", time.Since(started))
	}
	errorBody := assertError(t, mutation, http.StatusServiceUnavailable, string(apierr.CodeUnavailable))
	if errorBody.Error.Details["reason"] != "lock_timeout" {
		t.Fatalf("неверная ошибка lock_timeout: %+v", errorBody)
	}
	if record := loadValkey(t, app, created.ID); record.Name != "locked-cache" {
		t.Fatalf("таймаут изменил инстанс: %+v", record)
	}

	if err := holder.Rollback(); err != nil {
		t.Fatalf("освободить advisory lock: %v", err)
	}
	assertStatus(
		t,
		app.requestJSON(t, http.MethodPatch, base, map[string]any{"name": "after-lock"}, bearer(account.Token)),
		http.StatusOK,
	)
}

func Test_CreateValkey_WhenLockWaitIsCanceled_ReleasesTransactionForRetry(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	holder := holdValkeyLock(t, app)
	key := uuid.NewString()
	body := []byte(
		`{"name":"cancelled-cache","prefix":"cancel","mode":"single","vcpu":1,"ram_gb":1,"password":"` + testValkeyPassword + `"}`,
	)
	headers := mergeHeaders(bearer(account.Token), map[string]string{
		"Content-Type": "application/json", "Idempotency-Key": key,
	})
	requestContext, cancel := context.WithCancel(t.Context())
	result := make(chan testResponse, 1)
	requestError := make(chan error, 1)
	go func() {
		response, err := app.doWithContext(
			requestContext,
			http.MethodPost,
			"/v1/managed/valkey/instances",
			body,
			headers,
		)
		if err != nil {
			requestError <- err

			return
		}
		result <- response
	}()
	waitForValkeyLockWaiter(t, app)
	cancel()

	select {
	case err := <-requestError:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ошибка отменённого запроса: %v", err)
		}
	case response := <-result:
		if response.StatusCode < 400 {
			t.Fatalf("отменённый запрос принят: %+v", response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("отменённый запрос не завершился")
	}
	assertDatabaseCount(t, app.database.DB().Model(&store.ValkeyInstance{}).Where(
		"user_id = ? AND name = ?", account.ID, "cancelled-cache",
	), 0)

	if err := holder.Rollback(); err != nil {
		t.Fatalf("освободить advisory lock: %v", err)
	}
	retry, err := app.do(http.MethodPost, "/v1/managed/valkey/instances", body, headers)
	if err != nil {
		t.Fatalf("повторить запрос после отмены: %v", err)
	}
	assertStatus(t, retry, http.StatusAccepted)
}

func Test_MutateValkey_AfterWaitingForLock_RereadsStateAndUsesPostLockTime(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	created := createValkey(t, app, account, map[string]any{"name": "reread-cache"})
	makeValkeyReady(t, app, created.ID)
	holder := holdValkeyLock(t, app)
	responseChannel := make(chan testResponse, 1)
	errorChannel := make(chan error, 1)
	go func() {
		response, err := app.do(
			http.MethodPost,
			"/v1/managed/valkey/instances/"+created.ID.String()+"/resize",
			[]byte(`{"vcpu":1,"ram_gb":2}`),
			mergeHeaders(bearer(account.Token), map[string]string{
				"Content-Type": "application/json", "Idempotency-Key": uuid.NewString(),
			}),
		)
		if err != nil {
			errorChannel <- err

			return
		}
		responseChannel <- response
	}()
	waitForValkeyLockWaiter(t, app)
	deletionTime := time.Now().UTC()
	if _, err := holder.ExecContext(t.Context(), `
		UPDATE valkey_instances
		SET deletion_requested_at = $1, updated_at = $1
		WHERE id = $2`, deletionTime, created.ID); err != nil {
		t.Fatalf("изменить состояние под lock: %v", err)
	}
	if err := holder.Commit(); err != nil {
		t.Fatalf("зафиксировать состояние под lock: %v", err)
	}

	response := receiveHTTPResult(t, responseChannel, errorChannel)
	errorBody := assertError(t, response, http.StatusConflict, string(apierr.CodeInstanceNotReady))
	if errorBody.Error.Details["reason"] != "deleting" {
		t.Fatalf("ожидавшая мутация не перечитала строку: %+v", errorBody)
	}

	second := newHTTPTestAPI(t, testAPIConfig{})
	holder = holdValkeyLock(t, app)
	createResponses := make(chan testResponse, 1)
	createErrors := make(chan error, 1)
	go func() {
		response, err := second.do(
			http.MethodPost,
			"/v1/managed/valkey/instances",
			[]byte(
				`{"name":"after-wait","prefix":"wait","mode":"single","vcpu":1,"ram_gb":1,"password":"`+testValkeyPassword+`"}`,
			),
			mergeHeaders(bearer(account.Token), map[string]string{
				"Content-Type": "application/json", "Idempotency-Key": uuid.NewString(),
			}),
		)
		if err != nil {
			createErrors <- err

			return
		}
		createResponses <- response
	}()
	waitForValkeyLockWaiter(t, app)
	releasedAt := time.Now().UTC()
	if err := holder.Commit(); err != nil {
		t.Fatalf("освободить lock перед созданием: %v", err)
	}
	createResponse := receiveHTTPResult(t, createResponses, createErrors)
	assertStatus(t, createResponse, http.StatusAccepted)
	createdAfterWait := decodeResponse[valkeydomain.Instance](t, createResponse)
	if createdAfterWait.CreatedAt.Before(releasedAt) {
		t.Fatalf("время %s получено до освобождения lock %s", createdAfterWait.CreatedAt, releasedAt)
	}
}

func holdValkeyLock(t *testing.T, app *testAPI) *sql.Tx {
	t.Helper()

	pool, err := app.database.DB().DB()
	if err != nil {
		t.Fatalf("получить SQL-пул: %v", err)
	}
	tx, err := pool.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("начать транзакцию блокировки: %v", err)
	}
	if _, err := tx.ExecContext(t.Context(), "SELECT pg_advisory_xact_lock(1)"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("получить advisory lock: %v", err)
	}

	return tx
}

func waitForValkeyLockWaiter(t *testing.T, app *testAPI) {
	waitForValkeyLockWaiters(t, app, 1)
}

func waitForValkeyLockWaiters(t *testing.T, app *testAPI, minimum int64) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var count int64
		if err := app.database.DB().Raw(`
			SELECT COUNT(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND wait_event = 'advisory'
			  AND query LIKE '%pg_advisory_xact_lock(1)%'`).Scan(&count).Error; err != nil {
			t.Fatalf("проверить ожидание advisory lock: %v", err)
		}
		if count >= minimum {
			return
		}

		runtime.Gosched()
	}

	t.Fatalf("ожидающих advisory lock меньше %d", minimum)
}

func receiveHTTPResult(
	t *testing.T,
	responses <-chan testResponse,
	errorsReceived <-chan error,
) testResponse {
	t.Helper()

	select {
	case err := <-errorsReceived:
		t.Fatalf("выполнить конкурентный HTTP-запрос: %v", err)
	case response := <-responses:
		return response
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP-запрос не завершился")
	}

	return testResponse{}
}
