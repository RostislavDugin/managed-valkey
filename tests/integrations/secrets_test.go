//go:build integration

package integrations

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func Test_RegisterAccount_WithSuccessfulResponse_RecordsPasswordAndTokenForArtifactScan(t *testing.T) {
	const token = "eyJhbGciOiJIUzI1NiJ9.integration-test.signature"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/auth/register":
			_ = json.NewEncoder(writer).Encode(map[string]string{"token": token})
		case "/v1/me":
			if request.Header.Get("Authorization") != "Bearer "+token {
				http.Error(writer, "unauthorized", http.StatusUnauthorized)

				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"user":  map[string]string{"id": "11111111-1111-7111-8111-111111111111", "email": "user@example.com"},
				"quota": map[string]int{"max_vcpu": 4, "max_ram_gb": 12},
				"usage": map[string]int{"used_vcpu": 0, "used_ram_gb": 0},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	secretValuesFile := filepath.Join(t.TempDir(), "secret-values")
	harness := &scenarioHarness{
		t:   t,
		env: environment{runID: "token-recording", secretValuesFile: secretValuesFile},
		api: newAPIClient(server.URL),
	}
	registered := harness.registerAccount()
	if registered.Token != token {
		t.Fatal("регистрация вернула неожиданный JWT")
	}
	contents, err := os.ReadFile(secretValuesFile)
	if err != nil {
		t.Fatalf("прочитать контрольные секреты: %v", err)
	}
	values := strings.Split(strings.TrimSpace(string(contents)), "\n")
	if len(values) != 2 {
		t.Fatalf("записано контрольных секретов: %d, ожидалось 2", len(values))
	}
	if len(values[0]) != 32 || values[0] == token {
		t.Fatal("пароль аккаунта не записан отдельно от JWT")
	}
	if values[1] != token {
		t.Fatal("JWT аккаунта не записан для проверки артефактов")
	}
}
