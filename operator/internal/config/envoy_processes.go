//go:build !integration

package config

func configuredEnvoyProcesses() (int, error) {
	return DefaultEnvoyProcesses, nil
}
