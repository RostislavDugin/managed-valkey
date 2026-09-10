package valkey_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func Test_ReadMetrics_WithCompleteInfo_ReturnsAllFieldsAndVerifiesRunID(t *testing.T) {
	server := newMetricsRESPServer(t, func(int) string { return "process-1" }, nil)
	client, session := dialSession(t, server.address())
	defer client.Close()
	defer session.Close()

	metrics, err := session.ReadMetrics(context.Background(), "operator-secret")
	if err != nil {
		t.Fatalf("прочитать метрики: %v", err)
	}
	want := operatorvalkey.Metrics{
		Role: "primary", RunID: "process-1", UsedMemoryBytes: 1024, MaxmemoryBytes: 2048,
		ConnectedClients: 7, OpsPerSec: 8, KeyspaceHits: 9, KeyspaceMisses: 10, EvictedKeys: 11,
	}
	if metrics != want {
		t.Fatalf("получены неверные метрики: %+v", metrics)
	}
	if err := session.VerifyRunID(context.Background(), metrics.RunID); err != nil {
		t.Fatalf("повторно проверить run_id: %v", err)
	}
	if got := server.applicationCommands(); !slices.Equal(got, []string{"AUTH", "INFO", "INFO"}) {
		t.Fatalf("порядок команд %v", got)
	}
}

func Test_ReadMetrics_WithMissingOrNegativeRequiredField_ReturnsUnexpectedReply(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
	}{
		{name: "обязательное поле отсутствует", overrides: map[string]string{"keyspace_hits": ""}},
		{name: "обязательное поле отрицательное", overrides: map[string]string{"evicted_keys": "-1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newMetricsRESPServer(t, func(int) string { return "process-1" }, test.overrides)
			client, session := dialSession(t, server.address())
			defer client.Close()
			defer session.Close()

			_, err := session.ReadMetrics(context.Background(), "operator-secret")
			if !errors.Is(err, operatorvalkey.ErrUnexpectedReply) {
				t.Fatalf("ожидался недопустимый ответ INFO, получено %v", err)
			}
		})
	}
}

func Test_VerifyRunID_WhenProcessChanges_ReturnsProcessChanged(t *testing.T) {
	server := newMetricsRESPServer(t, func(call int) string {
		if call == 1 {
			return "process-1"
		}

		return "process-2"
	}, nil)
	client, session := dialSession(t, server.address())
	defer client.Close()
	defer session.Close()

	metrics, err := session.ReadMetrics(context.Background(), "operator-secret")
	if err != nil {
		t.Fatalf("прочитать метрики: %v", err)
	}
	if err := session.VerifyRunID(
		context.Background(),
		metrics.RunID,
	); !errors.Is(
		err,
		operatorvalkey.ErrProcessChanged,
	) {
		t.Fatalf("смена процесса вернула %v", err)
	}
}

func newMetricsRESPServer(
	t *testing.T,
	runID func(int) string,
	overrides map[string]string,
) *respServer {
	t.Helper()

	infoCalls := 0
	return newRESPServer(t, func(command []string) (string, bool) {
		switch command[0] {
		case "HELLO", "AUTH":
			return "+OK\r\n", false
		case "INFO":
			infoCalls++
			if len(command) > 1 {
				body := "# Server\r\nrun_id:" + runID(infoCalls) + "\r\n"
				return fmt.Sprintf("$%d\r\n%s\r\n", len(body), body), false
			}

			values := map[string]string{
				"run_id": "process-1", "role": "master", "used_memory": "1024", "maxmemory": "2048",
				"connected_clients": "7", "instantaneous_ops_per_sec": "8", "keyspace_hits": "9",
				"keyspace_misses": "10", "evicted_keys": "11",
			}
			for name, value := range overrides {
				values[name] = value
			}
			lines := make([]string, 0, len(values))
			for _, name := range []string{
				"run_id", "role", "used_memory", "maxmemory", "connected_clients",
				"instantaneous_ops_per_sec", "keyspace_hits", "keyspace_misses", "evicted_keys",
			} {
				if values[name] != "" {
					lines = append(lines, name+":"+values[name])
				}
			}
			body := "# All\r\n" + strings.Join(lines, "\r\n") + "\r\n"

			return fmt.Sprintf("$%d\r\n%s\r\n", len(body), body), false
		default:
			return "-ERR unexpected command\r\n", false
		}
	})
}
