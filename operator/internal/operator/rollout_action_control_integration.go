//go:build integration

package operator

import (
	"context"
	"sync"

	appsv1 "k8s.io/api/apps/v1"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

type IntegrationRolloutActionEvent struct {
	Name              string
	Namespace         string
	StatefulSetName   string
	Stage             valkeyv1alpha1.RolloutStage
	DesiredReplicas   int32
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
	statefulSet *appsv1.StatefulSet,
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
	desiredReplicas := int32(0)
	if statefulSet.Spec.Replicas != nil {
		desiredReplicas = *statefulSet.Spec.Replicas
	}
	configName := ""
	for _, volume := range statefulSet.Spec.Template.Spec.Volumes {
		if volume.Name == "config" && volume.ConfigMap != nil {
			configName = volume.ConfigMap.Name
			break
		}
	}
	return control(ctx, IntegrationRolloutActionEvent{
		Name: name, Namespace: statefulSet.Namespace, StatefulSetName: statefulSet.Name,
		Stage: stage, DesiredReplicas: desiredReplicas, DesiredConfigName: configName,
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
		Name: name, Namespace: instance.Namespace, StatefulSetName: instance.Name, Stage: stage,
	})
}
