//go:build integration

package operator

import (
	"context"
	"sync"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

type IntegrationCredentialRotationActionEvent struct {
	Name          string
	Namespace     string
	Stage         valkeyv1alpha1.CredentialRotationStage
	TargetVersion int64
	Ordinal       int32
	PodUID        string
}

type IntegrationCredentialRotationActionControl func(
	context.Context,
	IntegrationCredentialRotationActionEvent,
) error

var integrationCredentialRotationActionControl struct {
	sync.Mutex
	nextID  uint64
	id      uint64
	control IntegrationCredentialRotationActionControl
}

func InstallIntegrationCredentialRotationActionControl(
	control IntegrationCredentialRotationActionControl,
) func() {
	integrationCredentialRotationActionControl.Lock()
	integrationCredentialRotationActionControl.nextID++
	id := integrationCredentialRotationActionControl.nextID
	integrationCredentialRotationActionControl.id = id
	integrationCredentialRotationActionControl.control = control
	integrationCredentialRotationActionControl.Unlock()

	return func() {
		integrationCredentialRotationActionControl.Lock()
		defer integrationCredentialRotationActionControl.Unlock()
		if integrationCredentialRotationActionControl.id == id {
			integrationCredentialRotationActionControl.control = nil
		}
	}
}

func runCredentialRotationActionControl(
	ctx context.Context,
	name string,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) error {
	integrationCredentialRotationActionControl.Lock()
	control := integrationCredentialRotationActionControl.control
	integrationCredentialRotationActionControl.Unlock()
	if control == nil || instance.Status.CredentialRotation == nil {
		return nil
	}
	rotation := instance.Status.CredentialRotation
	return control(ctx, IntegrationCredentialRotationActionEvent{
		Name: name, Namespace: instance.Namespace, Stage: rotation.Stage,
		TargetVersion: rotation.TargetVersion, Ordinal: process.Ordinal, PodUID: process.PodUID,
	})
}
