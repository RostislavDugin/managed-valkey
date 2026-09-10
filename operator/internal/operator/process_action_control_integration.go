//go:build integration

package operator

import (
	"context"
	"sync"

	corev1 "k8s.io/api/core/v1"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

type IntegrationProcessActionEvent struct {
	Name      string
	Namespace string
	PodName   string
	PodUID    string
	Evidence  string
}

type IntegrationProcessActionControl func(context.Context, IntegrationProcessActionEvent) error

var integrationProcessActionControl struct {
	sync.Mutex
	nextID  uint64
	id      uint64
	control IntegrationProcessActionControl
}

func InstallIntegrationProcessActionControl(control IntegrationProcessActionControl) func() {
	integrationProcessActionControl.Lock()
	integrationProcessActionControl.nextID++
	id := integrationProcessActionControl.nextID
	integrationProcessActionControl.id = id
	integrationProcessActionControl.control = control
	integrationProcessActionControl.Unlock()

	return func() {
		integrationProcessActionControl.Lock()
		defer integrationProcessActionControl.Unlock()
		if integrationProcessActionControl.id == id {
			integrationProcessActionControl.control = nil
		}
	}
}

func runProcessActionControl(
	ctx context.Context,
	name string,
	pod *corev1.Pod,
	process valkeyv1alpha1.NodeStatus,
) error {
	integrationProcessActionControl.Lock()
	control := integrationProcessActionControl.control
	integrationProcessActionControl.Unlock()
	if control == nil {
		return nil
	}
	evidence := ""
	if process.Termination != nil {
		evidence = process.Termination.Evidence
	}
	return control(ctx, IntegrationProcessActionEvent{
		Name: name, Namespace: pod.Namespace, PodName: pod.Name,
		PodUID: string(pod.UID), Evidence: evidence,
	})
}
