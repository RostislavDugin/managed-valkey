package operator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func TestTerminatedContainerIsSavedBeforePodFinalizerIsRemoved(t *testing.T) {
	ctx := context.Background()
	instance, pod, node, _ := processObservationObjects()
	pod.Finalizers = []string{processFinalizer}
	finishedAt := metav1.NewTime(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC))
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: 0, Reason: "Completed", FinishedAt: finishedAt,
	}}
	instance.Status.Initialized = true
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}, &corev1.Pod{}).
		WithObjects(instance, pod, node).
		Build()
	newReconciler := func() *ValkeyInstanceReconciler {
		return &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s}
	}

	result, err := newReconciler().reconcileProcess(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("сохранить завершение: result=%+v error=%v", result, err)
	}
	if instance.Status.Nodes[0].Termination == nil ||
		instance.Status.Nodes[0].Termination.Evidence != "container_status" ||
		instance.Status.Nodes[0].Termination.ExitCode != 0 {
		t.Fatalf("неверное доказательство завершения: %+v", instance.Status.Nodes[0])
	}
	observedPod := &corev1.Pod{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), observedPod); err != nil {
		t.Fatalf("прочитать удержанный Pod: %v", err)
	}
	if len(observedPod.Finalizers) != 1 || observedPod.Finalizers[0] != processFinalizer {
		t.Fatal("finalizer снят до сохранения доказательства")
	}

	lostDeleteResponse := &lostDeleteResponseClient{Client: k8s}
	result, err = (&ValkeyInstanceReconciler{
		Client: lostDeleteResponse, APIReader: k8s,
	}).reconcileProcess(ctx, instance)
	if err == nil || !result.IsZero() {
		t.Fatalf("потерянный ответ удаления Pod: result=%+v error=%v", result, err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), observedPod); err != nil {
		t.Fatalf("прочитать удаляемый Pod: %v", err)
	}
	if observedPod.DeletionTimestamp.IsZero() {
		t.Fatal("Pod не получил deletionTimestamp")
	}

	result, err = newReconciler().reconcileProcess(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("снять finalizer Pod: result=%+v error=%v", result, err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), observedPod); err == nil {
		t.Fatal("Pod остался после снятия finalizer")
	}
}

type lostDeleteResponseClient struct {
	client.Client
	once sync.Once
}

func (c *lostDeleteResponseClient) Delete(
	ctx context.Context,
	object client.Object,
	options ...client.DeleteOption,
) error {
	lost := false
	c.once.Do(func() { lost = true })
	if err := c.Client.Delete(ctx, object, options...); err != nil {
		return err
	}
	if lost {
		return errors.New("ответ Delete потерян")
	}

	return nil
}

func TestLastStateOfDifferentContainerDoesNotProveOldProcessStopped(t *testing.T) {
	ctx := context.Background()
	instance, pod, node, secret := processObservationObjects()
	pod.Finalizers = []string{processFinalizer}
	instance.Status.Initialized = true
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}
	instance.Status.Nodes[0].ContainerID = "containerd://process-a"
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://process-c"
	pod.Status.ContainerStatuses[0].RestartCount = 2
	pod.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{
		Reason: "ProcessBStopped", FinishedAt: metav1.Now(),
	}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}, &corev1.Pod{}).
		WithObjects(instance, pod, node, secret).
		Build()
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		InspectProcess: func(context.Context, string, string, string) (operatorvalkey.ProcessState, error) {
			return operatorvalkey.ProcessState{
				Role: "primary", RunID: "run-c", AppEnabled: true,
				AppPasswordHashes: []string{strings.Repeat("ab", 32)},
			}, nil
		},
	}
	result, err := reconciler.reconcileProcess(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("обнаружить потерю истории: result=%+v error=%v", result, err)
	}
	condition := apimeta.FindStatusCondition(instance.Status.Conditions, conditionTypeRecoveryRequired)
	if condition == nil || condition.Reason != "TerminationProofLost" ||
		instance.Status.Nodes[0].Termination != nil || instance.Status.Reason != "FENCING_REQUIRED" {
		t.Fatalf("lastState чужого процесса принят как доказательство: %+v", instance.Status)
	}
}

func TestNodeDeletionRequiresSavedNameAndUID(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	process := valkeyv1alpha1.NodeStatus{
		PodUID: "pod-a", ContainerID: "containerd://a", RunID: "run-a",
		NodeName: "worker-1", NodeUID: "node-old",
	}

	tests := []struct {
		name     string
		objects  []client.Object
		process  valkeyv1alpha1.NodeStatus
		expected bool
	}{
		{name: "missing node", expected: true},
		{name: "reused name", objects: []client.Object{&corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: "worker-1", UID: types.UID("node-new"),
		}}}, expected: true},
		{name: "same not ready node", objects: []client.Object{&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1", UID: types.UID("node-old")},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeReady, Status: corev1.ConditionFalse,
			}}},
		}}, expected: false},
		{name: "missing identity", process: valkeyv1alpha1.NodeStatus{}, expected: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observed := process
			if test.process.NodeName == "" && test.name == "missing identity" {
				observed = test.process
			}
			k8s := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(test.objects...).Build()
			reconciler := &ValkeyInstanceReconciler{
				Client: k8s, APIReader: k8s, Clock: clocktesting.NewFakeClock(now),
			}
			deleted, err := reconciler.previousNodeDeleted(ctx, observed)
			if err != nil || deleted != test.expected {
				t.Fatalf("проверка Node: deleted=%t error=%v", deleted, err)
			}
		})
	}
}

type errorReader struct {
	client.Reader
}

func (errorReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("kubernetes недоступен")
}

func TestNodeReadErrorIsNotProof(t *testing.T) {
	reconciler := &ValkeyInstanceReconciler{APIReader: errorReader{}}
	deleted, err := reconciler.previousNodeDeleted(context.Background(), valkeyv1alpha1.NodeStatus{
		NodeName: "worker-1", NodeUID: "node-1",
	})
	if err == nil || deleted {
		t.Fatalf("ошибка Kubernetes принята как доказательство: deleted=%t error=%v", deleted, err)
	}
}
