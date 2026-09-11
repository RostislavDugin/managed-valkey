//go:build integration

package operator

import (
	"context"
	"sync"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

type IntegrationRolloutActionEvent struct {
	Name              string
	Namespace         string
	InstanceName      string
	Stage             valkeyv1alpha1.RolloutStage
	DesiredProcesses  int32
	DesiredConfigName string
}

type IntegrationRolloutActionControl func(context.Context, IntegrationRolloutActionEvent) error

var integrationRolloutActionControl struct {
	sync.Mutex
	nextID  uint64
	id      uint64
	control IntegrationRolloutActionControl
}

func InstallIntegrationRolloutActionControl(control IntegrationRolloutActionControl) func() {
	integrationRolloutActionControl.Lock()
	integrationRolloutActionControl.nextID++
	id := integrationRolloutActionControl.nextID
	integrationRolloutActionControl.id = id
	integrationRolloutActionControl.control = control
	integrationRolloutActionControl.Unlock()

	return func() {
		integrationRolloutActionControl.Lock()
		defer integrationRolloutActionControl.Unlock()
		if integrationRolloutActionControl.id == id {
			integrationRolloutActionControl.control = nil
		}
	}
}

func runRolloutActionControl(
	ctx context.Context,
	name string,
	instance *valkeyv1alpha1.ValkeyInstance,
	desiredProcesses int32,
	desiredConfigName string,
) error {
	integrationRolloutActionControl.Lock()
	control := integrationRolloutActionControl.control
	integrationRolloutActionControl.Unlock()
	if control == nil {
		return nil
	}
	stage := valkeyv1alpha1.RolloutStage("")
	if instance.Status.Rollout != nil {
		stage = instance.Status.Rollout.Stage
	}
	return control(ctx, IntegrationRolloutActionEvent{
		Name: name, Namespace: instance.Namespace, InstanceName: instance.Name,
		Stage: stage, DesiredProcesses: desiredProcesses, DesiredConfigName: desiredConfigName,
	})
}

func runRolloutStatusActionControl(
	ctx context.Context,
	name string,
	instance *valkeyv1alpha1.ValkeyInstance,
) error {
	integrationRolloutActionControl.Lock()
	control := integrationRolloutActionControl.control
	integrationRolloutActionControl.Unlock()
	if control == nil {
		return nil
	}
	stage := valkeyv1alpha1.RolloutStage("")
	if instance.Status.Rollout != nil {
		stage = instance.Status.Rollout.Stage
	}
	return control(ctx, IntegrationRolloutActionEvent{
		Name: name, Namespace: instance.Namespace, InstanceName: instance.Name, Stage: stage,
	})
}
