//go:build integration

package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const EnvEnvoyProcesses = "VALKEY_ENVOY_PROCESSES"

func configuredEnvoyProcesses() (int, error) {
	value := strings.TrimSpace(os.Getenv(EnvEnvoyProcesses))
	if value == "" {
		return DefaultEnvoyProcesses, nil
	}
	result, err := strconv.Atoi(value)
	if err != nil || result < 1 || result > DefaultEnvoyProcesses {
		return 0, fmt.Errorf(
			"%s должно быть целым числом от 1 до %d",
			EnvEnvoyProcesses,
			DefaultEnvoyProcesses,
		)
	}

	return result, nil
}
