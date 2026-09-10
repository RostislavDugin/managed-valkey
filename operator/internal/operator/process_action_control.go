//go:build !integration

package operator

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func runProcessActionControl(
	context.Context,
	string,
	*corev1.Pod,
	valkeyv1alpha1.NodeStatus,
) error {
	return nil
}
