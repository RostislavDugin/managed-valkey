package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
)

// Отдельная таблица версий не смешивает записи теста с рабочими миграциями в
// той же базе.
const versionTable = "goose_db_version_rollback_test"

func TestFailedMigrationLeavesNoVersion(t *testing.T) {
	db := openTestDatabase(t)

	dir := t.TempDir()
	writeMigration(t, dir, "20260101000001_ok.sql", `-- +goose Up
CREATE TABLE rollback_probe_ok (id INTEGER NOT NULL);

-- +goose Down
DROP TABLE rollback_probe_ok;
`)
	writeMigration(t, dir, "20260101000002_broken.sql", `-- +goose Up
CREATE TABLE rollback_probe_broken (id INTEGER NOT NULL);
SELECT совершенно_несуществующая_функция();

-- +goose Down
DROP TABLE rollback_probe_broken;
`)

	provider, err := goose.NewProvider(goose.DialectCustom, db, os.DirFS(dir),
		goose.WithStore(mustStore(t)),
	)
	if err != nil {
		t.Fatalf("создать провайдер goose: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()

		for _, statement := range []string{
			"DROP TABLE IF EXISTS rollback_probe_ok",
			"DROP TABLE IF EXISTS rollback_probe_broken",
			"DROP TABLE IF EXISTS " + versionTable,
		} {
			if _, err := db.ExecContext(cleanupCtx, statement); err != nil {
				t.Errorf("очистка %q: %v", statement, err)
			}
		}
	})

	if _, err := provider.Up(ctx); err == nil {
		t.Fatal("ошибочная миграция применена, ожидалась ошибка")
	}

	if version := currentVersion(ctx, t, db); version != 20260101000001 {
		t.Errorf("версия схемы %d, ожидалась 20260101000001", version)
	}

	if tableExists(ctx, t, db, "rollback_probe_broken") {
		t.Error("таблица ошибочной миграции осталась в базе")
	}

	if !tableExists(ctx, t, db, "rollback_probe_ok") {
		t.Error("успешная миграция откачена вместе с ошибочной")
	}
}

func mustStore(t *testing.T) database.Store {
	t.Helper()

	store, err := database.NewStore(database.DialectPostgres, versionTable)
	if err != nil {
		t.Fatalf("создать хранилище версий: %v", err)
	}

	return store
}

func currentVersion(ctx context.Context, t *testing.T, db *sql.DB) int64 {
	t.Helper()

	var version int64

	query := "SELECT COALESCE(MAX(version_id), -1) FROM " + versionTable + " WHERE is_applied"
	if err := db.QueryRowContext(ctx, query).Scan(&version); err != nil {
		t.Fatalf("прочитать версию схемы: %v", err)
	}

	return version
}

func tableExists(ctx context.Context, t *testing.T, db *sql.DB, name string) bool {
	t.Helper()

	var exists bool

	query := "SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = current_schema() AND tablename = $1)"
	if err := db.QueryRowContext(ctx, query, name).Scan(&exists); err != nil {
		t.Fatalf("проверить таблицу %s: %v", name, err)
	}

	return exists
}

func writeMigration(t *testing.T, dir, name, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("записать миграцию %s: %v", name, err)
	}
}

func openTestDatabase(t *testing.T) *sql.DB {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL не задан")
	}

	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("открыть тестовую базу: %v", err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("закрыть тестовую базу: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("тестовая база недоступна: %v", err)
	}

	return db
}
