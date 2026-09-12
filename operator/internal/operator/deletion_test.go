package operator

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	envoyv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func Test_ReconcileDeletion_WithRunningInstance_ClosesRoutesAndKeepsSecretUntilProcessStops(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	instance, pod, node, secret := processObservationObjects()
	instance.Finalizers = []string{instanceFinalizer}
	instance.DeletionTimestamp = ptrTime(now)
	instance.Status.Initialized = true
	instance.Status.Phase = valkeyv1alpha1.InstancePhaseRunning
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}
	pod.Finalizers = []string{processFinalizer}
	pod.Labels = workloadLabels(instance.Name)
	pod.Labels[applicationRoleLabel] = string(valkeyv1alpha1.NodeRolePrimary)
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayName, Namespace: "valkey-system"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "envoy",
			Listeners: []gatewayv1.Listener{
				{Name: gatewayv1.SectionName(instance.Name), Port: 41379, Protocol: gatewayv1.TCPProtocolType},
				{Name: gatewayv1.SectionName(instance.Name + "-ro"), Port: 41379, Protocol: gatewayv1.TCPProtocolType},
				{Name: "neighbor", Port: 41379, Protocol: gatewayv1.TCPProtocolType},
			},
		},
	}
	route := &gatewayv1alpha2.TCPRoute{ObjectMeta: metav1.ObjectMeta{
		Name: instance.Name, Namespace: instance.Namespace,
	}}
	policy := &envoyv1alpha1.SecurityPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: instance.Name, Namespace: instance.Namespace,
	}}
	readRoute := &gatewayv1alpha2.TCPRoute{ObjectMeta: metav1.ObjectMeta{
		Name: instance.Name + "-ro", Namespace: instance.Namespace,
	}}
	readPolicy := &envoyv1alpha1.SecurityPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: instance.Name + "-ro", Namespace: instance.Namespace,
	}}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}, &corev1.Pod{}).
		WithObjects(instance, pod, node, secret, gateway, route, policy, readRoute, readPolicy).
		Build()
	disableCalls := 0
	newReconciler := func() *ValkeyInstanceReconciler {
		return &ValkeyInstanceReconciler{
			Client: k8s, APIReader: k8s, SystemNamespace: "valkey-system",
			Clock: clocktesting.NewFakeClock(now),
			UpdateAppAccess: func(
				_ context.Context,
				_ string,
				_ string,
				hash string,
				enabled bool,
			) (operatorvalkey.ProcessState, error) {
				disableCalls++
				if enabled {
					t.Fatal("удаление включило app")
				}
				return operatorvalkey.ProcessState{
					Role: "primary", RunID: "run-1", AppPasswordHashes: []string{hash},
				}, nil
			},
		}
	}
	reconcile := func() {
		t.Helper()
		if _, err := newReconciler().reconcileDeletion(ctx, instance); err != nil {
			t.Fatalf("стадия удаления %v: %v", instance.Status.Deletion, err)
		}
	}

	reconcile()
	if instance.Status.Deletion == nil ||
		instance.Status.Deletion.Stage != valkeyv1alpha1.DeletionStageRemovingNetwork {
		t.Fatalf("не сохранена начальная стадия: %+v", instance.Status.Deletion)
	}
	reconcile()
	if instance.Status.Deletion.Stage != valkeyv1alpha1.DeletionStageDisablingApp {
		t.Fatalf("сеть не закрыта: %+v", instance.Status.Deletion)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(gateway), gateway); err != nil {
		t.Fatalf("прочитать Gateway после удаления listener: %v", err)
	}
	if len(gateway.Spec.Listeners) != 1 || gateway.Spec.Listeners[0].Name != "neighbor" {
		t.Fatalf("удаление затронуло listener соседа: %+v", gateway.Spec.Listeners)
	}
	for _, object := range []client.Object{route, policy, readRoute, readPolicy} {
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
			t.Fatalf("сетевой ресурс %T остался: %v", object, err)
		}
	}

	reconcile()
	if instance.Status.Deletion.Stage != valkeyv1alpha1.DeletionStageDisablingApp || disableCalls != 0 {
		t.Fatal("app изменён до снятия primary label")
	}
	reconcile()
	if instance.Status.Deletion.Stage != valkeyv1alpha1.DeletionStageStopping || disableCalls != 1 {
		t.Fatalf("app не выключен: stage=%s calls=%d", instance.Status.Deletion.Stage, disableCalls)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{}); err != nil {
		t.Fatalf("Secret удалён до остановки процесса: %v", err)
	}

	reconcile()
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil || pod.DeletionTimestamp.IsZero() {
		t.Fatalf("удаление Pod не запрошено: deletionTimestamp=%v error=%v", pod.DeletionTimestamp, err)
	}
	reconcile()
	if instance.Status.Deletion.Stage != valkeyv1alpha1.DeletionStageVerifying {
		t.Fatalf("не начата проверка остановки: %+v", instance.Status.Deletion)
	}

	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatalf("прочитать Pod перед завершением: %v", err)
	}
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: 0, Reason: "Completed", FinishedAt: metav1.NewTime(now),
	}}
	if err := k8s.Status().Update(ctx, pod); err != nil {
		t.Fatalf("сохранить завершение Pod: %v", err)
	}
	reconcile()
	if instance.Status.Nodes[0].Termination == nil {
		t.Fatal("доказательство остановки не сохранено")
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{}); err != nil {
		t.Fatalf("Secret удалён до сохранения доказательства: %v", err)
	}
	reconcile()
	reconcile()
	reconcile()

	observed := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observed); !apierrors.IsNotFound(err) {
		t.Fatalf("ValkeyInstance остался после доказанной остановки: %v", err)
	}
}

func Test_ReconcileDeletion_WhenPodDisappearsWithoutProof_WaitsUntilNodeDeletionProvesTermination(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Finalizers = []string{instanceFinalizer}
	instance.DeletionTimestamp = ptrTime(time.Now())
	instance.Status.Deletion = &valkeyv1alpha1.DeletionStatus{
		Stage: valkeyv1alpha1.DeletionStageVerifying, StartedAt: metav1.Now(),
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{{
		PodUID: "pod-1", ContainerID: "containerd://1", RunID: "run-1",
		NodeName: "worker-1", NodeUID: "node-1",
	}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "worker-1", UID: types.UID("node-1"),
	}}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, node).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s}

	result, err := reconciler.reconcileDeletion(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("ожидание доказательства: result=%+v error=%v", result, err)
	}
	if !slices.Contains(instance.Finalizers, instanceFinalizer) ||
		instance.Status.Nodes[0].Termination != nil {
		t.Fatal("исчезновение Pod принято как остановка")
	}

	if err := k8s.Delete(ctx, node); err != nil {
		t.Fatalf("удалить прежний Node: %v", err)
	}
	if _, err := reconciler.reconcileDeletion(ctx, instance); err != nil {
		t.Fatalf("сохранить удаление Node: %v", err)
	}
	if instance.Status.Nodes[0].Termination == nil ||
		instance.Status.Nodes[0].Termination.Evidence != "node_deleted" {
		t.Fatalf("удаление Node не сохранено: %+v", instance.Status.Nodes[0])
	}
	if _, err := reconciler.reconcileDeletion(ctx, instance); err != nil {
		t.Fatalf("завершить удаление после Node: %v", err)
	}
	if err := k8s.Get(
		ctx,
		client.ObjectKeyFromObject(instance),
		&valkeyv1alpha1.ValkeyInstance{},
	); !apierrors.IsNotFound(
		err,
	) {
		t.Fatalf("CR остался после удаления Node: %v", err)
	}
}

func Test_ReconcileDeletion_WithDuplicateProcessHistory_CopiesKnownTerminationEvidence(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Finalizers = []string{instanceFinalizer}
	instance.DeletionTimestamp = ptrTime(time.Now())
	instance.Status.Deletion = &valkeyv1alpha1.DeletionStatus{
		Stage: valkeyv1alpha1.DeletionStageVerifying, StartedAt: metav1.Now(),
	}
	current := failoverNode(0, valkeyv1alpha1.NodeRolePrimary, "history", 10, nil)
	current.Termination = &valkeyv1alpha1.ProcessTermination{
		Reason: "Completed", FinishedAt: metav1.Now(), Evidence: "container_status",
	}
	previous := current
	previous.Termination = nil
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{current}
	instance.Status.PreviousProcesses = []valkeyv1alpha1.NodeStatus{previous}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s}

	result, err := reconciler.reconcileDeletion(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("перенести известное завершение в историю: result=%+v error=%v", result, err)
	}
	if instance.Status.PreviousProcesses[0].Termination == nil ||
		instance.Status.PreviousProcesses[0].Termination.Evidence != "container_status" {
		t.Fatalf("известное завершение не перенесено: %+v", instance.Status.PreviousProcesses)
	}
}

func Test_ReconcileDeletion_BeforeWorkloadExists_RemovesInstanceWithoutCreatingResources(t *testing.T) {
	tests := []struct {
		name        string
		phase       valkeyv1alpha1.InstancePhase
		unsupported bool
	}{
		{
			name:  "при подготовке инстанса удаляет CR без создания Pod",
			phase: valkeyv1alpha1.InstancePhaseProvisioning,
		},
		{name: "при ошибке инстанса удаляет CR без создания Pod", phase: valkeyv1alpha1.InstancePhaseError},
		{
			name:        "при неподдерживаемом изменении удаляет CR без создания Pod",
			phase:       valkeyv1alpha1.InstancePhaseRunning,
			unsupported: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			instance := completeAcceptedInstance()
			instance.Finalizers = []string{instanceFinalizer}
			instance.DeletionTimestamp = ptrTime(time.Now())
			instance.Status.Phase = test.phase
			if test.unsupported {
				instance.Status.Conditions = append(instance.Status.Conditions, metav1.Condition{
					Type: conditionTypeUnsupportedChange, Status: metav1.ConditionTrue, Reason: "SpecChanged",
				})
			}
			k8s := fake.NewClientBuilder().
				WithScheme(NewScheme()).
				WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
				WithObjects(instance).
				Build()
			reconciler := &ValkeyInstanceReconciler{
				Client: k8s, APIReader: k8s, SystemNamespace: "valkey-system",
			}
			for range 5 {
				if _, err := reconciler.reconcileDeletion(ctx, instance); err != nil {
					t.Fatalf("удалить незавершённый CR: %v", err)
				}
			}
			if err := k8s.Get(
				ctx,
				client.ObjectKeyFromObject(instance),
				&valkeyv1alpha1.ValkeyInstance{},
			); !apierrors.IsNotFound(
				err,
			) {
				t.Fatalf("незавершённый CR остался: %v", err)
			}
			pods := &corev1.PodList{}
			if err := k8s.List(ctx, pods, client.InNamespace(instance.Namespace)); err != nil ||
				len(pods.Items) != 0 {
				t.Fatalf("удаление создало Pod: items=%d error=%v", len(pods.Items), err)
			}
		})
	}
}

func Test_ReconcileDeletion_WhenAppProcessIsUnavailable_ContinuesToStoppingStage(t *testing.T) {
	ctx := context.Background()
	instance, pod, _, secret := processObservationObjects()
	instance.Finalizers = []string{instanceFinalizer}
	instance.DeletionTimestamp = ptrTime(time.Now())
	instance.Status.Deletion = &valkeyv1alpha1.DeletionStatus{
		Stage: valkeyv1alpha1.DeletionStageDisablingApp, StartedAt: metav1.Now(),
	}
	pod.Finalizers = []string{processFinalizer}
	pod.Labels = workloadLabels(instance.Name)
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}, &corev1.Pod{}).
		WithObjects(instance, pod, secret).
		Build()
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		UpdateAppAccess: func(context.Context, string, string, string, bool) (operatorvalkey.ProcessState, error) {
			return operatorvalkey.ProcessState{}, errors.New("процесс недоступен")
		},
	}

	if _, err := reconciler.reconcileDeletion(ctx, instance); err != nil {
		t.Fatalf("недоступный процесс заблокировал остановку: %v", err)
	}
	if instance.Status.Deletion.Stage != valkeyv1alpha1.DeletionStageStopping {
		t.Fatalf("удаление не перешло к остановке: %+v", instance.Status.Deletion)
	}
}

func Test_ReconcileDeletion_WhenSecretCannotBeRead_DoesNotStopWorkload(t *testing.T) {
	ctx := context.Background()
	instance, pod, _, _ := processObservationObjects()
	instance.Finalizers = []string{instanceFinalizer}
	instance.DeletionTimestamp = ptrTime(time.Now())
	instance.Status.Deletion = &valkeyv1alpha1.DeletionStatus{
		Stage: valkeyv1alpha1.DeletionStageDisablingApp, StartedAt: metav1.Now(),
	}
	pod.Labels = workloadLabels(instance.Name)
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod).
		Build()

	result, err := (&ValkeyInstanceReconciler{Client: k8s}).reconcileDeletion(ctx, instance)
	if err == nil || !result.IsZero() ||
		instance.Status.Deletion.Stage != valkeyv1alpha1.DeletionStageDisablingApp {
		t.Fatalf("удаление продолжилось без Secret: result=%+v stage=%s error=%v",
			result, instance.Status.Deletion.Stage, err)
	}
}

func ptrTime(value time.Time) *metav1.Time {
	result := metav1.NewTime(value)
	return &result
}
