//go:build !integration

package config

import corev1 "k8s.io/api/core/v1"

func configuredResourceRequests() (corev1.ResourceList, error) {
	return nil, nil
}
