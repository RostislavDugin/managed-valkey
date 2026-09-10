//go:build integration

package operator

import (
	"context"
	"sync"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

type IntegrationDeletionActionEvent struct {
	Name      string
	Namespace string
	Stage     valkeyv1alpha1.DeletionStage
}

type IntegrationDeletionActionControl func(context.Context, IntegrationDeletionActionEvent) error

var integrationDeletionActionControl struct {
	sync.Mutex
	nextID  uint64
	id      uint64
	control IntegrationDeletionActionControl
}

func InstallIntegrationDeletionActionControl(control IntegrationDeletionActionControl) func() {
	integrationDeletionActionControl.Lock()
	integrationDeletionActionControl.nextID++
	id := integrationDeletionActionControl.nextID
	integrationDeletionActionControl.id = id
	integrationDeletionActionControl.control = control
	integrationDeletionActionControl.Unlock()

	return func() {
		integrationDeletionActionControl.Lock()
		defer integrationDeletionActionControl.Unlock()
		if integrationDeletionActionControl.id == id {
			integrationDeletionActionControl.control = nil
		}
	}
}

func runDeletionActionControl(
	ctx context.Context,
	name string,
	instance *valkeyv1alpha1.ValkeyInstance,
) error {
	integrationDeletionActionControl.Lock()
	control := integrationDeletionActionControl.control
	integrationDeletionActionControl.Unlock()
	if control == nil {
		return nil
	}
	stage := valkeyv1alpha1.DeletionStage("")
	if instance.Status.Deletion != nil {
		stage = instance.Status.Deletion.Stage
	}
	return control(ctx, IntegrationDeletionActionEvent{
		Name: name, Namespace: instance.Namespace, Stage: stage,
	})
}
