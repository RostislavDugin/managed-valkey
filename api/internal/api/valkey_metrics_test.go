package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

func TestValkeyMetricsSchemaAcceptsSnapshotsAndRejectsInvalidRows(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	instance := createValkey(t, app, account, map[string]any{"name": "metrics-schema"})
	now := time.Now().UTC().Truncate(time.Microsecond)

	withoutCPU := metricRecord(instance.ID, 0, now, "run-a")
	withoutCPU.CPUMillicores = nil
	if err := app.database.DB().Create(&withoutCPU).Error; err != nil {
		t.Fatalf("сохранить метрику без CPU: %v", err)
	}

	withCPU := metricRecord(instance.ID, 1, now, "run-b")
	cpu := int64(125)
	withCPU.CPUMillicores = &cpu
	if err := app.database.DB().Create(&withCPU).Error; err != nil {
		t.Fatalf("сохранить метрику с CPU: %v", err)
	}

	duplicate := withoutCPU
	if err := app.database.DB().
		Create(&duplicate).
		Error; postgresConstraint(
		err,
	) != "valkey_node_metrics_instance_ordinal_ts_key" {
		t.Fatalf("повторная метрика вернула %v", err)
	}

	invalid := metricRecord(instance.ID, -1, now.Add(time.Second), "run-c")
	if err := app.database.DB().
		Create(&invalid).
		Error; postgresConstraint(
		err,
	) != "valkey_node_metrics_ordinal_nonnegative" {
		t.Fatalf("отрицательный ordinal вернул %v", err)
	}

	var indexCount int64
	if err := app.database.DB().Raw(`
		SELECT COUNT(*)
		FROM pg_indexes
		WHERE schemaname = current_schema()
		  AND tablename = 'valkey_node_metrics'
		  AND indexname IN (
			  'valkey_node_metrics_instance_ordinal_ts_key',
			  'valkey_node_metrics_instance_ts_idx'
		  )`).Scan(&indexCount).Error; err != nil {
		t.Fatalf("проверить индексы метрик: %v", err)
	}
	if indexCount != 2 {
		t.Fatalf("индексов %d, ожидалось 2", indexCount)
	}

	missingInstance := metricRecord(uuid.Must(uuid.NewV7()), 0, now, "run-d")
	if err := app.database.DB().
		Create(&missingInstance).
		Error; postgresConstraint(
		err,
	) != "valkey_node_metrics_instance_id_fkey" {
		t.Fatalf("чужой instance_id вернул %v", err)
	}
}

func TestImportValkeyNodeMetricsStoresRecentBatchOnce(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	instance := createValkey(t, app, account, map[string]any{"name": "metrics-import"})
	ctx := context.Background()
	now, err := app.database.DatabaseTime(ctx, app.database.DB())
	if err != nil {
		t.Fatalf("прочитать время PostgreSQL: %v", err)
	}
	collectedAt := now.Add(-time.Minute).Truncate(time.Second).Add(123456 * time.Microsecond)
	rows := []store.ValkeyNodeMetric{
		metricValues(
			uuid.Nil, 0, collectedAt, domain.ValkeyNodeRolePrimary, "run-a",
			100, nil, 1, 2, 3, 4, 5,
		),
		metricValues(
			uuid.Nil, 1, collectedAt, domain.ValkeyNodeRoleReplica, "run-b",
			200, int64Pointer(125), 6, 7, 8, 9, 10,
		),
		metricValues(
			uuid.Nil, 2, collectedAt, domain.ValkeyNodeRoleReplica, "run-c",
			300, int64Pointer(250), 11, 12, 13, 14, 15,
		),
	}

	for range 2 {
		if err := app.database.ImportValkeyNodeMetrics(ctx, instance.ID, rows, 24*time.Hour); err != nil {
			t.Fatalf("импортировать метрики: %v", err)
		}
	}

	var stored []store.ValkeyNodeMetric
	if err := app.database.DB().Where("instance_id = ?", instance.ID).Order("ordinal").Find(&stored).Error; err != nil {
		t.Fatalf("прочитать импортированные метрики: %v", err)
	}
	if len(stored) != 3 {
		t.Fatalf("строк %d, ожидалось 3", len(stored))
	}
	if !stored[0].TS.Equal(collectedAt) || stored[0].CPUMillicores != nil || stored[0].RunID != "run-a" {
		t.Fatalf("первая строка изменена: %+v", stored[0])
	}
	if stored[1].CPUMillicores == nil || *stored[1].CPUMillicores != 125 ||
		stored[1].UsedMemoryBytes != 200 || stored[1].KeyspaceMisses != 9 {
		t.Fatalf("вторая строка изменена: %+v", stored[1])
	}
}

func TestImportValkeyNodeMetricsRejectsLargeBatchAndExpiredRows(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	instance := createValkey(t, app, account, map[string]any{"name": "metrics-import-limits"})
	ctx := context.Background()
	now, err := app.database.DatabaseTime(ctx, app.database.DB())
	if err != nil {
		t.Fatalf("прочитать время PostgreSQL: %v", err)
	}

	tooMany := make([]store.ValkeyNodeMetric, 4)
	if err := app.database.ImportValkeyNodeMetrics(ctx, instance.ID, tooMany, time.Hour); !errors.Is(
		err,
		store.ErrValkeyMetricBatchTooLarge,
	) {
		t.Fatalf("ошибка %v, ожидалась %v", err, store.ErrValkeyMetricBatchTooLarge)
	}

	rows := []store.ValkeyNodeMetric{
		metricRecord(instance.ID, 0, now.Add(-2*time.Hour), "expired"),
		metricRecord(instance.ID, 1, now.Add(-time.Minute), "recent"),
	}
	if err := app.database.ImportValkeyNodeMetrics(ctx, instance.ID, rows, time.Hour); err != nil {
		t.Fatalf("импортировать метрики: %v", err)
	}

	var stored []store.ValkeyNodeMetric
	if err := app.database.DB().Where("instance_id = ?", instance.ID).Find(&stored).Error; err != nil {
		t.Fatalf("прочитать импортированные метрики: %v", err)
	}
	if len(stored) != 1 || stored[0].RunID != "recent" {
		t.Fatalf("сохранены неожиданные строки: %+v", stored)
	}
}

func TestImportValkeyNodeMetricsDoesNotRaceDeletion(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	instance := createValkey(t, app, account, map[string]any{"name": "metrics-import-deletion"})
	ctx := context.Background()

	tx := app.database.DB().Begin()
	if tx.Error != nil {
		t.Fatalf("начать транзакцию удаления: %v", tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })
	var locked store.ValkeyInstance
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", instance.ID).
		First(&locked).
		Error; err != nil {
		t.Fatalf("заблокировать инстанс: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		result <- app.database.ImportValkeyNodeMetrics(ctx, instance.ID, []store.ValkeyNodeMetric{
			metricRecord(instance.ID, 0, time.Now().UTC(), "concurrent"),
		}, time.Hour)
	}()

	requestedAt := time.Now().UTC()
	if err := tx.Model(&store.ValkeyInstance{}).Where("id = ?", instance.ID).
		UpdateColumn("deletion_requested_at", requestedAt).Error; err != nil {
		t.Fatalf("начать удаление: %v", err)
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatalf("завершить транзакцию удаления: %v", err)
	}

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("импорт при удалении: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("импорт не завершился после удаления")
	}

	var count int64
	if err := app.database.DB().Model(&store.ValkeyNodeMetric{}).
		Where("instance_id = ?", instance.ID).Count(&count).Error; err != nil {
		t.Fatalf("посчитать метрики: %v", err)
	}
	if count != 0 {
		t.Fatalf("при удалении сохранено строк: %d", count)
	}
}

func TestDeleteValkeyNodeMetricsBeforeKeepsBoundary(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	instance := createValkey(t, app, account, map[string]any{"name": "metrics-cleanup"})
	cutoff := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	rows := []store.ValkeyNodeMetric{
		metricRecord(instance.ID, 0, cutoff.Add(-time.Microsecond), "old"),
		metricRecord(instance.ID, 0, cutoff, "boundary"),
		metricRecord(instance.ID, 0, cutoff.Add(time.Microsecond), "new"),
	}
	if err := app.database.DB().Create(&rows).Error; err != nil {
		t.Fatalf("подготовить метрики: %v", err)
	}

	var deleted int64
	err := app.database.WithinTransaction(context.Background(), func(tx *gorm.DB) error {
		var deleteErr error
		deleted, deleteErr = app.database.DeleteValkeyNodeMetricsBefore(context.Background(), tx, cutoff)
		return deleteErr
	})
	if err != nil {
		t.Fatalf("очистить метрики: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("удалено строк %d, ожидалась 1", deleted)
	}

	var stored []store.ValkeyNodeMetric
	if err := app.database.DB().Where("instance_id = ?", instance.ID).Order("ts").Find(&stored).Error; err != nil {
		t.Fatalf("прочитать метрики после очистки: %v", err)
	}
	if len(stored) != 2 || !stored[0].TS.Equal(cutoff) || stored[0].RunID != "boundary" {
		t.Fatalf("после очистки остались неожиданные строки: %+v", stored)
	}
}

func TestValkeyMetricsAggregateStoredRows(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	instance := createValkey(t, app, account, map[string]any{"name": "metrics-series", "prefix": "series"})
	from := time.Now().UTC().Add(-time.Minute).Truncate(10 * time.Second)
	to := from.Add(40 * time.Second)

	rows := []store.ValkeyNodeMetric{
		metricValues(
			instance.ID,
			0,
			from.Add(-time.Second),
			domain.ValkeyNodeRolePrimary,
			"run-a",
			50,
			nil,
			5,
			50,
			100,
			20,
			2,
		),
		metricValues(
			instance.ID,
			0,
			from.Add(time.Second),
			domain.ValkeyNodeRolePrimary,
			"run-a",
			100,
			nil,
			10,
			100,
			110,
			23,
			2,
		),
		metricValues(
			instance.ID,
			0,
			from.Add(5*time.Second),
			domain.ValkeyNodeRolePrimary,
			"run-a",
			300,
			int64Pointer(200),
			20,
			300,
			120,
			25,
			3,
		),
		metricValues(
			instance.ID,
			0,
			from.Add(21*time.Second),
			domain.ValkeyNodeRoleReplica,
			"run-b",
			500,
			int64Pointer(400),
			30,
			500,
			5,
			2,
			0,
		),
		metricValues(
			instance.ID,
			1,
			from.Add(2*time.Second),
			domain.ValkeyNodeRoleUnknown,
			"run-x",
			700,
			int64Pointer(600),
			40,
			700,
			7,
			3,
			1,
		),
	}
	if err := app.database.DB().Create(&rows).Error; err != nil {
		t.Fatalf("подготовить метрики: %v", err)
	}

	path := metricsPath(instance.ID, from, to)
	response := app.requestJSON(t, http.MethodGet, path, nil, bearer(account.Token))
	assertStatus(t, response, http.StatusOK)
	metrics := decodeResponse[valkeydomain.Metrics](t, response)
	if metrics.StepSeconds != 10 || len(metrics.Nodes) != 2 {
		t.Fatalf("неожиданный ответ: %+v", metrics)
	}
	if metrics.Nodes[0].Ordinal != 0 || metrics.Nodes[0].Name != instance.Slug+"-0" ||
		metrics.Nodes[0].Role != domain.ValkeyNodeRoleReplica || len(metrics.Nodes[0].Points) != 4 {
		t.Fatalf("неожиданный первый ряд: %+v", metrics.Nodes[0])
	}
	if metrics.Nodes[1].Ordinal != 1 || metrics.Nodes[1].Role != domain.ValkeyNodeRoleUnknown {
		t.Fatalf("неожиданный второй ряд: %+v", metrics.Nodes[1])
	}

	first := metrics.Nodes[0].Points[0]
	assertFloatPointer(t, "память", first.UsedMemoryBytes, 200)
	assertFloatPointer(t, "CPU", first.CPUMillicores, 200)
	assertFloatPointer(t, "подключения", first.ConnectedClients, 15)
	assertFloatPointer(t, "операции", first.OpsPerSec, 200)
	assertIntPointer(t, "попадания", first.KeyspaceHits, 20)
	assertIntPointer(t, "промахи", first.KeyspaceMisses, 5)
	assertIntPointer(t, "вытеснения", first.EvictedKeys, 1)

	gap := metrics.Nodes[0].Points[1]
	if gap.UsedMemoryBytes != nil || gap.CPUMillicores != nil || gap.KeyspaceHits != nil {
		t.Fatalf("пропуск заполнен значениями: %+v", gap)
	}

	afterRestart := metrics.Nodes[0].Points[2]
	assertIntPointer(t, "попадания нового процесса", afterRestart.KeyspaceHits, 5)
	assertIntPointer(t, "промахи нового процесса", afterRestart.KeyspaceMisses, 2)
	assertIntPointer(t, "вытеснения нового процесса", afterRestart.EvictedKeys, 0)
	if !metrics.Nodes[0].Points[0].CollectedAt.Before(metrics.Nodes[0].Points[3].CollectedAt) {
		t.Fatalf("точки не отсортированы: %+v", metrics.Nodes[0].Points)
	}
	if strings.Contains(string(response.Body), "run-a") || strings.Contains(string(response.Body), "maxmemory_bytes") ||
		strings.Contains(string(response.Body), "demo") {
		t.Fatalf("ответ содержит внутренние поля: %s", response.Body)
	}
}

func TestValkeyMetricsValidateWindowAndProtectOwner(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	owner := app.registerAccount(t, "")
	stranger := app.registerAccount(t, "")
	instance := createValkey(t, app, owner, map[string]any{"name": "metrics-private"})
	base := "/v1/managed/valkey/instances/" + instance.ID.String() + "/metrics"

	defaultResponse := app.requestJSON(t, http.MethodGet, base, nil, bearer(owner.Token))
	assertStatus(t, defaultResponse, http.StatusOK)
	defaultMetrics := decodeResponse[valkeydomain.Metrics](t, defaultResponse)
	if defaultMetrics.StepSeconds != 60 || len(defaultMetrics.Nodes) != 0 {
		t.Fatalf("неожиданное окно по умолчанию: %+v", defaultMetrics)
	}

	for _, path := range []string{
		base + "?range=bad",
		base + "?range=5m&from=2026-09-09T00%3A00%3A00Z&to=2026-09-09T00%3A05%3A00Z",
		base + "?from=2026-09-09T00%3A00%3A00Z",
		base + "?from=bad&to=2026-09-09T00%3A05%3A00Z",
		base + "?from=2026-09-09T00%3A05%3A00Z&to=2026-09-09T00%3A00%3A00Z",
		base + "?from=2026-09-01T00%3A00%3A00Z&to=2026-09-09T00%3A00%3A00Z",
		base + "?unknown=1",
		base + "?range=5m&range=1h",
	} {
		assertError(
			t,
			app.requestJSON(t, http.MethodGet, path, nil, bearer(owner.Token)),
			http.StatusBadRequest,
			string(apierr.CodeValidationFailed),
		)
	}

	assertError(
		t,
		app.requestJSON(t, http.MethodGet, base, nil, nil),
		http.StatusUnauthorized,
		string(apierr.CodeUnauthorized),
	)
	assertError(
		t,
		app.requestJSON(t, http.MethodGet, base, nil, bearer(stranger.Token)),
		http.StatusNotFound,
		string(apierr.CodeNotFound),
	)
	missing := "/v1/managed/valkey/instances/" + uuid.Must(uuid.NewV7()).String() + "/metrics"
	assertError(
		t,
		app.requestJSON(t, http.MethodGet, missing, nil, bearer(owner.Token)),
		http.StatusNotFound,
		string(apierr.CodeNotFound),
	)

	deletedAt := time.Now().UTC()
	if err := app.database.DB().Model(&store.ValkeyInstance{}).Where("id = ?", instance.ID).
		Updates(map[string]any{"deletion_requested_at": deletedAt, "deleted_at": deletedAt}).Error; err != nil {
		t.Fatalf("удалить базу: %v", err)
	}
	assertError(
		t,
		app.requestJSON(t, http.MethodGet, base, nil, bearer(owner.Token)),
		http.StatusNotFound,
		string(apierr.CodeNotFound),
	)
}

func TestValkeyMetricsHideRowsOlderThanSevenDays(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	instance := createValkey(t, app, account, map[string]any{"name": "metrics-retention"})
	old := time.Now().UTC().Add(-8 * 24 * time.Hour)
	if err := app.database.DB().Create(&store.ValkeyNodeMetric{
		InstanceID: instance.ID, Ordinal: 0, TS: old, Role: domain.ValkeyNodeRolePrimary,
		RunID: "old-run", UsedMemoryBytes: 100, MaxmemoryBytes: 200,
		ConnectedClients: 1, OpsPerSec: 1, KeyspaceHits: 1, KeyspaceMisses: 1, EvictedKeys: 1,
	}).Error; err != nil {
		t.Fatalf("сохранить старую метрику: %v", err)
	}

	from := old.Add(-time.Hour)
	to := old.Add(time.Hour)
	response := app.requestJSON(t, http.MethodGet, metricsPath(instance.ID, from, to), nil, bearer(account.Token))
	assertStatus(t, response, http.StatusOK)
	if metrics := decodeResponse[valkeydomain.Metrics](t, response); len(metrics.Nodes) != 0 {
		t.Fatalf("API вернул старые метрики: %+v", metrics)
	}
}

func metricRecord(instanceID uuid.UUID, ordinal int, ts time.Time, runID string) store.ValkeyNodeMetric {
	return metricValues(
		instanceID,
		ordinal,
		ts,
		domain.ValkeyNodeRolePrimary,
		runID,
		100,
		nil,
		1,
		2,
		3,
		4,
		5,
	)
}

func metricValues(
	instanceID uuid.UUID,
	ordinal int,
	ts time.Time,
	role domain.ValkeyNodeRole,
	runID string,
	usedMemory int64,
	cpu *int64,
	clients int64,
	ops int64,
	hits int64,
	misses int64,
	evicted int64,
) store.ValkeyNodeMetric {
	return store.ValkeyNodeMetric{
		InstanceID: instanceID, Ordinal: ordinal, TS: ts, Role: role, RunID: runID,
		UsedMemoryBytes: usedMemory, MaxmemoryBytes: 1024, ConnectedClients: clients,
		OpsPerSec: ops, KeyspaceHits: hits, KeyspaceMisses: misses, EvictedKeys: evicted,
		CPUMillicores: cpu,
	}
}

func metricsPath(instanceID uuid.UUID, from, to time.Time) string {
	query := url.Values{"from": {from.Format(time.RFC3339Nano)}, "to": {to.Format(time.RFC3339Nano)}}

	return fmt.Sprintf("/v1/managed/valkey/instances/%s/metrics?%s", instanceID, query.Encode())
}

func postgresConstraint(err error) string {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		return postgresError.ConstraintName
	}

	return ""
}

func int64Pointer(value int64) *int64 {
	return &value
}

func assertFloatPointer(t *testing.T, name string, value *float64, expected float64) {
	t.Helper()
	if value == nil || *value != expected {
		t.Fatalf("%s равно %v, ожидалось %v", name, value, expected)
	}
}

func assertIntPointer(t *testing.T, name string, value *int64, expected int64) {
	t.Helper()
	if value == nil || *value != expected {
		t.Fatalf("%s равно %v, ожидалось %v", name, value, expected)
	}
}
