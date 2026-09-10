package store_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

func Test_OpenStore_WhenDatabaseIsUnreachable_ReturnsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	opened, err := store.Open(
		ctx,
		"postgres://user:pass@"+closedAddress(t)+"/managed_valkey?sslmode=disable",
		discardLogger(),
	)
	if err == nil {
		_ = opened.Close()

		t.Fatal("недоступная база принята, ожидалась ошибка")
	}
}

func Test_OpenStore_WithoutDatabaseUrl_ReturnsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	opened, err := store.Open(ctx, "", discardLogger())
	if err == nil {
		_ = opened.Close()

		t.Fatal("пустой DATABASE_URL принят, ожидалась ошибка")
	}
}

// Подключение к освобождённому порту гарантированно не проходит, а тест не
// зависит от чужих сервисов.
func closedAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("занять порт: %v", err)
	}

	address := listener.Addr().String()

	if err := listener.Close(); err != nil {
		t.Fatalf("освободить порт: %v", err)
	}

	return address
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
