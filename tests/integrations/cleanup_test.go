//go:build integration

package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func Test_CleanupInstance_AfterScenarioStopsEarly_DeletesOnlyOwnedResources(t *testing.T) {
	const firstToken = "first-token"
	const secondToken = "second-token"
	const firstID = "11111111-1111-7111-8111-111111111111"
	const secondID = "22222222-2222-7222-8222-222222222222"
	deleted := map[string]bool{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		token := request.Header.Get("Authorization")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.Method == http.MethodDelete && request.URL.Path == "/v1/managed/valkey/instances/"+firstID:
			if token != "Bearer "+firstToken {
				t.Errorf("очистка использовала чужой token: %s", token)
			}
			deleted[firstID] = true
			writer.WriteHeader(http.StatusAccepted)
		case request.Method == http.MethodGet && request.URL.Path == "/v1/managed/valkey/instances/"+firstID:
			if deleted[firstID] {
				writer.WriteHeader(http.StatusNotFound)
				_, _ = writer.Write([]byte(`{"error":{"code":"not_found"}}`))
			}
		case request.Method == http.MethodGet && request.URL.Path == "/v1/managed/valkey/instances":
			items := []instance{}
			if token == "Bearer "+secondToken {
				items = append(items, instance{ID: secondID})
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"items": items})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/me":
			used := 0
			if token == "Bearer "+secondToken {
				used = 1
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"user":  map[string]string{"id": "user", "email": "user@example.com"},
				"quota": map[string]int{"max_vcpu": 4, "max_ram_gb": 16},
				"usage": map[string]int{"used_vcpu": used, "used_ram_gb": used},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	harness := &scenarioHarness{
		t:          t,
		env:        environment{diagnosticsDir: t.TempDir()},
		api:        newAPIClient(server.URL),
		kubernetes: deletedNamespaceReader{deleted: "valkey-first"},
		processes:  checkFunc(func() error { return nil }),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stoppedErr := errors.New("сценарий остановлен намеренно")
	var cleanupErr error
	runStoppedScenario := func() error {
		defer func() {
			cleanupErr = harness.deleteOwnedInstance(
				ctx,
				account{ID: "first-user", Token: firstToken},
				firstID,
				"valkey-first",
			)
		}()

		return stoppedErr
	}
	if err := runStoppedScenario(); !errors.Is(err, stoppedErr) {
		t.Fatalf("сценарий завершился неверно: %v", err)
	}
	if cleanupErr != nil {
		t.Fatalf("очистить остановленный сценарий: %v", cleanupErr)
	}
	items, err := harness.api.List(ctx, secondToken)
	if err != nil || len(items) != 1 || items[0].ID != secondID {
		t.Fatalf("ресурс второго аккаунта изменён: items=%+v error=%v", items, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !deleted[firstID] || deleted[secondID] {
		t.Fatalf("очистка затронула неверные ресурсы: %+v", deleted)
	}
}
