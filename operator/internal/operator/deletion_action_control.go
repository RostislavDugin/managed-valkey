//go:build !integration

package operator

import (
	"context"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func runDeletionActionControl(context.Context, string, *valkeyv1alpha1.ValkeyInstance) error {
	return nil
}
