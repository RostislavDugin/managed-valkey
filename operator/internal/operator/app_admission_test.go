package operator

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func TestAppAdmissionWaitsForCurrentPrimaryEndpoint(t *testing.T) {
	ctx := context.Background()
	instance, pod, _, secret := processObservationObjects()
	pod.Labels = workloadLabels(instance.Name)
	pod.Labels[applicationRoleLabel] = string(valkeyv1alpha1.NodeRolePrimary)
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, secret).
		Build()
	enableCalls := 0
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		UpdateAppAccess: func(
			_ context.Context,
			_ string,
			_ string,
			hash string,
			enabled bool,
		) (operatorvalkey.ProcessState, error) {
			enableCalls++
			if !enabled {
				t.Fatal("допуск попытался выключить app")
			}
			return operatorvalkey.ProcessState{
				Role: "primary", RunID: "run-1", AppEnabled: true,
				AppPasswordHashes: []string{hash},
			}, nil
		},
	}

	result, err := reconciler.reconcileAppAdmission(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("ожидание EndpointSlice: result=%+v error=%v", result, err)
	}
	if enableCalls != 0 || instance.Status.Initialized {
		t.Fatal("app включён до появления EndpointSlice")
	}

	endpointSlice := testPrimaryEndpointSlice(instance, pod)
	endpointSlice.Endpoints[0].TargetRef.UID = types.UID("old-pod")
	if err := k8s.Create(ctx, endpointSlice); err != nil {
		t.Fatalf("создать EndpointSlice прежнего Pod: %v", err)
	}
	result, err = reconciler.reconcileAppAdmission(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("ожидание актуального EndpointSlice: result=%+v error=%v", result, err)
	}
	if enableCalls != 0 {
		t.Fatal("app включён для EndpointSlice прежнего Pod")
	}

	endpointSlice.Endpoints[0].TargetRef.UID = pod.UID
	if err := k8s.Update(ctx, endpointSlice); err != nil {
		t.Fatalf("обновить EndpointSlice: %v", err)
	}
	result, err = reconciler.reconcileAppAdmission(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("допустить app: result=%+v error=%v", result, err)
	}
	if enableCalls != 1 {
		t.Fatalf("app включён %d раз", enableCalls)
	}
	if !instance.Status.Initialized || instance.Status.Phase != valkeyv1alpha1.InstancePhaseRunning ||
		instance.Status.PrimaryOrdinal == nil || *instance.Status.PrimaryOrdinal != 0 ||
		instance.Status.PrimaryPodUID != string(pod.UID) ||
		instance.Status.PrimaryContainerID != pod.Status.ContainerStatuses[0].ContainerID ||
		instance.Status.ObservedGeneration != instance.Status.AcceptedConfiguration.DesiredGeneration ||
		instance.Status.AppliedPasswordVersion != instance.Status.AcceptedConfiguration.PasswordVersion ||
		instance.Status.Applied == nil || instance.Status.ObservedAt == nil {
		t.Fatalf("неполный status после допуска: %+v", instance.Status)
	}
}

func TestAppAdmissionRechecksStateAfterLostEnableResponse(t *testing.T) {
	ctx := context.Background()
	instance, pod, _, secret := processObservationObjects()
	pod.Labels = workloadLabels(instance.Name)
	pod.Labels[applicationRoleLabel] = string(valkeyv1alpha1.NodeRolePrimary)
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}
	endpointSlice := testPrimaryEndpointSlice(instance, pod)
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, secret, endpointSlice).
		Build()
	calls := 0
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		UpdateAppAccess: func(
			_ context.Context,
			_ string,
			_ string,
			hash string,
			enabled bool,
		) (operatorvalkey.ProcessState, error) {
			calls++
			if !enabled {
				t.Fatal("повтор допуска выключил app")
			}
			if calls == 1 {
				return operatorvalkey.ProcessState{}, errors.New("ответ ACL SETUSER потерян")
			}
			return operatorvalkey.ProcessState{
				Role: "primary", RunID: "run-1", AppEnabled: true,
				AppPasswordHashes: []string{hash},
			}, nil
		},
	}

	if _, err := reconciler.reconcileAppAdmission(ctx, instance); err == nil {
		t.Fatal("потерянный ответ ACL SETUSER принят как успех")
	}
	if instance.Status.Initialized {
		t.Fatal("готовность записана без подтверждения состояния ACL")
	}
	if _, err := reconciler.reconcileAppAdmission(ctx, instance); err != nil {
		t.Fatalf("повторно проверить состояние ACL: %v", err)
	}
	if calls != 2 || !instance.Status.Initialized {
		t.Fatalf("состояние ACL не подтверждено повторно: calls=%d status=%+v", calls, instance.Status)
	}
}

func TestPrimaryLabelRequiresSavedCurrentProcess(t *testing.T) {
	ctx := context.Background()
	instance, pod, _, _ := processObservationObjects()
	pod.Labels = workloadLabels(instance.Name)
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}
	k8s := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(instance, pod).Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s}

	result, err := reconciler.reconcilePrimaryLabel(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("назначить primary: result=%+v error=%v", result, err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatalf("прочитать Pod с ролью: %v", err)
	}
	if pod.Labels[applicationRoleLabel] != string(valkeyv1alpha1.NodeRolePrimary) {
		t.Fatal("текущий процесс не получил роль primary")
	}

	instance.Status.Nodes[0].ContainerID = "containerd://old"
	result, err = reconciler.reconcilePrimaryLabel(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("снять устаревшую роль: result=%+v error=%v", result, err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatalf("прочитать Pod без роли: %v", err)
	}
	if _, exists := pod.Labels[applicationRoleLabel]; exists {
		t.Fatal("роль primary осталась у процесса без сохранённой идентичности")
	}
}

func TestProcessDisablesEarlyAppAccess(t *testing.T) {
	ctx := context.Background()
	instance, pod, node, secret := processObservationObjects()
	pod.Finalizers = []string{processFinalizer}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, node, secret).
		Build()
	hash := strings.Repeat("ab", 32)
	disableCalls := 0
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		InspectProcess: func(context.Context, string, string, string) (operatorvalkey.ProcessState, error) {
			return operatorvalkey.ProcessState{
				Role: "primary", RunID: "run-1", AppEnabled: true,
				AppPasswordHashes: []string{hash},
			}, nil
		},
		UpdateAppAccess: func(
			_ context.Context,
			_ string,
			_ string,
			observedHash string,
			enabled bool,
		) (operatorvalkey.ProcessState, error) {
			disableCalls++
			if enabled || observedHash != hash {
				t.Fatal("ранний доступ app отключён с неверными параметрами")
			}
			return operatorvalkey.ProcessState{
				Role: "primary", RunID: "run-1", AppPasswordHashes: []string{hash},
			}, nil
		},
	}

	result, err := reconciler.reconcileProcess(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("отключить ранний app: result=%+v error=%v", result, err)
	}
	if disableCalls != 1 || len(instance.Status.Nodes) != 0 {
		t.Fatalf("ранний app не отключён отдельным проходом: calls=%d nodes=%v", disableCalls, instance.Status.Nodes)
	}
}

func TestEndpointSliceRequestsPrimaryInstance(t *testing.T) {
	endpointSlice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
		Namespace: "valkey-cache-a1b2c3",
		Labels:    map[string]string{discoveryv1.LabelServiceName: "cache-a1b2c3-primary"},
	}}
	requests := endpointSliceInstanceRequests(context.Background(), endpointSlice)
	if len(requests) != 1 || requests[0].Namespace != endpointSlice.Namespace ||
		requests[0].Name != "cache-a1b2c3" {
		t.Fatalf("неверная очередь EndpointSlice: %+v", requests)
	}
}

func testObservedNode(pod *corev1.Pod) valkeyv1alpha1.NodeStatus {
	return valkeyv1alpha1.NodeStatus{
		Ordinal: 0, PodUID: string(pod.UID), ContainerID: pod.Status.ContainerStatuses[0].ContainerID,
		RunID: "run-1", NodeName: pod.Spec.NodeName, NodeUID: "node-1",
		Role: valkeyv1alpha1.NodeRolePrimary, Readiness: true,
	}
}

func testPrimaryEndpointSlice(
	instance *valkeyv1alpha1.ValkeyInstance,
	pod *corev1.Pod,
) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name + "-primary-1",
			Namespace: instance.Namespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: instance.Name + "-primary",
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports: []discoveryv1.EndpointPort{{
			Name: ptr.To("valkey"), Port: ptr.To[int32](valkeyPort), Protocol: ptr.To(corev1.ProtocolTCP),
		}},
		Endpoints: []discoveryv1.Endpoint{{
			Addresses: []string{pod.Status.PodIP},
			Conditions: discoveryv1.EndpointConditions{
				Ready: ptr.To(true),
			},
			TargetRef: &corev1.ObjectReference{
				APIVersion: "v1", Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID,
			},
		}},
	}
}
