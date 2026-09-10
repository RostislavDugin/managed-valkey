//go:build !integration

package operator

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func runRolloutActionControl(
	context.Context,
	string,
	*valkeyv1alpha1.ValkeyInstance,
	*appsv1.StatefulSet,
) error {
	return nil
}

func runRolloutStatusActionControl(
	context.Context,
	string,
	*valkeyv1alpha1.ValkeyInstance,
) error {
	return nil
}
