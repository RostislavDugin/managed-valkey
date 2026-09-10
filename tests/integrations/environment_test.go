//go:build integration

package integrations

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

const (
	envAPIURL          = "MANAGED_VALKEY_INTEGRATION_API_URL"
	envAdminKubeconfig = "MANAGED_VALKEY_INTEGRATION_ADMIN_KUBECONFIG"
	envAPIProcessID    = "MANAGED_VALKEY_INTEGRATION_API_PID"
	envCAFile          = "MANAGED_VALKEY_INTEGRATION_CA_FILE"
	envDiagnosticsDir  = "MANAGED_VALKEY_INTEGRATION_DIAGNOSTICS_DIR"
	envOperatorPID     = "MANAGED_VALKEY_INTEGRATION_OPERATOR_PID"
	envPublicAddress   = "MANAGED_VALKEY_INTEGRATION_PUBLIC_ADDRESS"
	envRunID           = "MANAGED_VALKEY_INTEGRATION_RUN_ID"
	envSecretValues    = "MANAGED_VALKEY_INTEGRATION_SECRET_VALUES_FILE"
)

type environment struct {
	apiURL            string
	adminKubeconfig   string
	apiProcessID      int
	caFile            string
	diagnosticsDir    string
	operatorProcessID int
	publicAddress     string
	runID             string
	secretValuesFile  string
}

func loadEnvironment() (environment, error) {
	values := map[string]string{}
	for _, name := range []string{
		envAPIURL,
		envAdminKubeconfig,
		envAPIProcessID,
		envCAFile,
		envDiagnosticsDir,
		envOperatorPID,
		envPublicAddress,
		envRunID,
		envSecretValues,
	} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return environment{}, fmt.Errorf("переменная %s обязательна", name)
		}
		values[name] = value
	}

	apiProcessID, err := positiveProcessID(envAPIProcessID, values[envAPIProcessID])
	if err != nil {
		return environment{}, err
	}
	operatorProcessID, err := positiveProcessID(envOperatorPID, values[envOperatorPID])
	if err != nil {
		return environment{}, err
	}
	for _, name := range []string{envAdminKubeconfig, envCAFile} {
		if _, err := os.Stat(values[name]); err != nil {
			return environment{}, fmt.Errorf("прочитать %s: %w", name, err)
		}
	}

	return environment{
		apiURL:            strings.TrimRight(values[envAPIURL], "/"),
		adminKubeconfig:   values[envAdminKubeconfig],
		apiProcessID:      apiProcessID,
		caFile:            values[envCAFile],
		diagnosticsDir:    values[envDiagnosticsDir],
		operatorProcessID: operatorProcessID,
		publicAddress:     values[envPublicAddress],
		runID:             values[envRunID],
		secretValuesFile:  values[envSecretValues],
	}, nil
}

func positiveProcessID(name, value string) (int, error) {
	processID, err := strconv.Atoi(value)
	if err != nil || processID <= 0 {
		return 0, fmt.Errorf("переменная %s должна содержать положительный PID", name)
	}

	return processID, nil
}

func mustLoadEnvironment(t *testing.T) environment {
	t.Helper()

	env, err := loadEnvironment()
	if err != nil {
		t.Fatalf("загрузить integration-окружение: %v", err)
	}

	return env
}

func Test_LoadEnvironment_WithCompleteConfiguration_LoadsSharedBackendAddresses(t *testing.T) {
	temporary := t.TempDir()
	kubeconfig := temporary + "/admin.kubeconfig"
	caFile := temporary + "/ca.crt"
	if err := os.WriteFile(kubeconfig, []byte("kubeconfig"), 0o600); err != nil {
		t.Fatalf("создать kubeconfig: %v", err)
	}
	if err := os.WriteFile(caFile, []byte("ca"), 0o600); err != nil {
		t.Fatalf("создать CA: %v", err)
	}
	values := map[string]string{
		envAPIURL:          "http://127.0.0.1:18080/",
		envAdminKubeconfig: kubeconfig,
		envAPIProcessID:    "101",
		envCAFile:          caFile,
		envDiagnosticsDir:  temporary + "/diagnostics",
		envOperatorPID:     "102",
		envPublicAddress:   "127.0.0.1:31379",
		envRunID:           "integration-test",
		envSecretValues:    temporary + "/secret-values",
	}
	for name, value := range values {
		t.Setenv(name, value)
	}

	env, err := loadEnvironment()
	if err != nil {
		t.Fatalf("загрузить окружение: %v", err)
	}
	if env.apiURL != "http://127.0.0.1:18080" || env.adminKubeconfig != kubeconfig ||
		env.caFile != caFile || env.publicAddress != "127.0.0.1:31379" ||
		env.apiProcessID != 101 || env.operatorProcessID != 102 {
		t.Fatalf("окружение загружено неверно: %+v", env)
	}
}

func Test_LoadEnvironment_WithoutRequiredValue_ReturnsNamedError(t *testing.T) {
	for _, name := range []string{
		envAPIURL,
		envAdminKubeconfig,
		envAPIProcessID,
		envCAFile,
		envDiagnosticsDir,
		envOperatorPID,
		envPublicAddress,
		envRunID,
		envSecretValues,
	} {
		t.Setenv(name, "")
	}

	_, err := loadEnvironment()
	if err == nil || !strings.Contains(err.Error(), envAPIURL) {
		t.Fatalf("неверная ошибка обязательного значения: %v", err)
	}
}
