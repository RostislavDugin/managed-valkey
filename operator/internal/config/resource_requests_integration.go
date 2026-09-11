//go:build integration

package config

import (
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func configuredResourceRequests() (corev1.ResourceList, error) {
	cpuValue := strings.TrimSpace(os.Getenv(EnvIntegrationRequestCPU))
	memoryValue := strings.TrimSpace(os.Getenv(EnvIntegrationRequestMemory))
	if cpuValue == "" && memoryValue == "" {
		return nil, nil
	}
	if cpuValue == "" || memoryValue == "" {
		return nil, fmt.Errorf(
			"%s и %s должны быть заданы вместе",
			EnvIntegrationRequestCPU,
			EnvIntegrationRequestMemory,
		)
	}

	cpu, err := resource.ParseQuantity(cpuValue)
	if err != nil || cpu.Sign() <= 0 {
		return nil, fmt.Errorf("%s должно быть положительным количеством ресурса Kubernetes", EnvIntegrationRequestCPU)
	}
	memory, err := resource.ParseQuantity(memoryValue)
	if err != nil || memory.Sign() <= 0 {
		return nil, fmt.Errorf(
			"%s должно быть положительным количеством ресурса Kubernetes",
			EnvIntegrationRequestMemory,
		)
	}

	return corev1.ResourceList{
		corev1.ResourceCPU:    cpu,
		corev1.ResourceMemory: memory,
	}, nil
}
