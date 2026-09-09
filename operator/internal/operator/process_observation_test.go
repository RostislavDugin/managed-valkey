package operator

import (
	"bytes"
	"context"
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func TestReconcileProcessSavesFinalizerBeforeIdentity(t *testing.T) {
	ctx := context.Background()
	instance, pod, node, secret := processObservationObjects()
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, node, secret).
		Build()
	inspections := 0
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		Scheme: NewScheme(),
		InspectProcess: func(
			ctx context.Context,
			_ string,
			_ string,
			_ string,
		) (operatorvalkey.ProcessState, error) {
			inspections++
			observedPod := &corev1.Pod{}
			if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), observedPod); err != nil {
				t.Fatalf("прочитать Pod во время наблюдения: %v", err)
			}
			if !slices.Contains(observedPod.Finalizers, processFinalizer) {
				t.Fatal("клиент Valkey вызван до сохранения finalizer Pod")
			}

			return operatorvalkey.ProcessState{
				Role: "primary", RunID: "run-1", AppPasswordHashes: []string{strings.Repeat("ab", 32)},
			}, nil
		},
	}

	result, err := reconciler.reconcileProcess(ctx, instance)
	if err != nil {
		t.Fatalf("первый reconcile процесса: %v", err)
	}
	if result.IsZero() || inspections != 0 {
		t.Fatalf("первый reconcile не ограничился finalizer: result=%+v inspections=%d", result, inspections)
	}

	observedPod := &corev1.Pod{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), observedPod); err != nil {
		t.Fatalf("прочитать Pod после первого reconcile: %v", err)
	}
	if !slices.Contains(observedPod.Finalizers, processFinalizer) {
		t.Fatal("finalizer процесса не сохранён")
	}

	observedInstance := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observedInstance); err != nil {
		t.Fatalf("прочитать ValkeyInstance: %v", err)
	}
	result, err = reconciler.reconcileProcess(ctx, observedInstance)
	if err != nil {
		t.Fatalf("второй reconcile процесса: %v", err)
	}
	if inspections != 1 {
		t.Fatalf("идентичность не была сохранена: result=%+v inspections=%d", result, inspections)
	}

	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observedInstance); err != nil {
		t.Fatalf("прочитать сохранённую идентичность: %v", err)
	}
	if len(observedInstance.Status.Nodes) != 1 {
		t.Fatalf("сохранено идентичностей: %d", len(observedInstance.Status.Nodes))
	}
	nodeStatus := observedInstance.Status.Nodes[0]
	if nodeStatus.PodUID != string(pod.UID) ||
		nodeStatus.ContainerID != pod.Status.ContainerStatuses[0].ContainerID ||
		nodeStatus.RunID != "run-1" ||
		nodeStatus.NodeName != node.Name ||
		nodeStatus.NodeUID != string(node.UID) ||
		nodeStatus.Role != valkeyv1alpha1.NodeRolePrimary ||
		!nodeStatus.Readiness {
		t.Fatalf("сохранена неверная идентичность: %+v", nodeStatus)
	}
}

func TestProcessIdentityChangeRequiresRecovery(t *testing.T) {
	base := valkeyv1alpha1.NodeStatus{
		Ordinal: 0, PodUID: "pod-1", ContainerID: "containerd://1", RunID: "run-1",
		NodeName: "worker-1", NodeUID: "node-1",
	}

	tests := map[string]func(*valkeyv1alpha1.NodeStatus){
		"pod UID":      func(status *valkeyv1alpha1.NodeStatus) { status.PodUID = "pod-2" },
		"container ID": func(status *valkeyv1alpha1.NodeStatus) { status.ContainerID = "containerd://2" },
		"run_id":       func(status *valkeyv1alpha1.NodeStatus) { status.RunID = "run-2" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			observed := base
			mutate(&observed)
			if sameProcess(base, observed) {
				t.Fatalf("смена %s не обнаружена", name)
			}
		})
	}
	if !sameProcess(base, base) {
		t.Fatal("неизменный процесс распознан как замена")
	}
}

func TestPodInstanceRequests(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "cache-a1b2c3-0", Namespace: "valkey-cache-a1b2c3",
		Labels: map[string]string{instanceLabelKey: "cache-a1b2c3"},
	}}
	requests := podInstanceRequests(context.Background(), pod)
	if len(requests) != 1 || requests[0].Name != "cache-a1b2c3" ||
		requests[0].Namespace != "valkey-cache-a1b2c3" {
		t.Fatalf("неверный запрос reconcile для Pod: %+v", requests)
	}

	pod.Labels = nil
	if requests := podInstanceRequests(context.Background(), pod); len(requests) != 0 {
		t.Fatalf("Pod без метки поставлен в очередь: %+v", requests)
	}
}

func processObservationObjects() (
	*valkeyv1alpha1.ValkeyInstance,
	*corev1.Pod,
	*corev1.Node,
	*corev1.Secret,
) {
	instance := completeAcceptedInstance()
	instance.Spec.Slug = instance.Name
	instance.Status.AcceptedConfiguration.PasswordVersion = 1
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: instance.Name + "-0", Namespace: instance.Namespace, UID: types.UID("pod-1"),
		},
		Spec: corev1.PodSpec{NodeName: "worker-1"},
		Status: corev1.PodStatus{
			PodIP: "10.42.0.10",
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "valkey", ContainerID: "containerd://1",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", UID: types.UID("node-1")}}
	password := []byte(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 24)))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: valkeyv1alpha1.AuthSecretName(instance.Name), Namespace: instance.Namespace,
		},
		Data: map[string][]byte{
			valkeyv1alpha1.OperatorPasswordKey:            password,
			valkeyv1alpha1.ReplicaPasswordKey:             password,
			valkeyv1alpha1.HealthPasswordKey:              password,
			valkeyv1alpha1.UsersACLKey:                    []byte("acl"),
			valkeyv1alpha1.AppPasswordHashKeyPrefix + "1": []byte(strings.Repeat("ab", 32)),
		},
	}

	return instance, pod, node, secret
}
