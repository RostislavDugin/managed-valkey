package operator

import (
	"context"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func Test_HA02_ReconcileInitialPrimary_AfterRestartAndLostResponse_PreservesPersistedTarget(t *testing.T) {
	ctx := context.Background()
	instance, pod, node, secret := processObservationObjects()
	instance.Spec.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	target := testObservedNode(pod)
	target.Role = valkeyv1alpha1.NodeRoleReplica
	target.AppEnabled = false
	target.AppPasswordVersion = instance.Status.AcceptedConfiguration.PasswordVersion
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{target}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, node, secret).
		Build()
	calls := 0
	lostResponse := true
	newReconciler := func() *ValkeyInstanceReconciler {
		return &ValkeyInstanceReconciler{
			Client: k8s, APIReader: k8s,
			PromoteProcess: func(_ context.Context, _, _, _ string) (operatorvalkey.ProcessState, error) {
				calls++
				if lostResponse {
					return operatorvalkey.ProcessState{}, errors.New("ответ REPLICAOF потерян")
				}
				return operatorvalkey.ProcessState{
					Role: "replica", RunID: target.RunID, AppEnabled: false,
				}, nil
			},
		}
	}

	result, err := newReconciler().reconcileInitialPrimary(ctx, instance)
	if err != nil || result.IsZero() || calls != 0 {
		t.Fatalf("сохранить цель primary: result=%+v calls=%d error=%v", result, calls, err)
	}
	if !primaryIdentityMatches(instance, target) {
		t.Fatalf("сохранена неверная цель primary: %+v", instance.Status)
	}

	restarted := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), restarted); err != nil {
		t.Fatalf("прочитать цель после рестарта: %v", err)
	}
	result, err = newReconciler().reconcileInitialPrimary(ctx, restarted)
	if err == nil || !result.IsZero() || calls != 1 || !primaryIdentityMatches(restarted, target) {
		t.Fatalf("потерянный ответ изменил цель: result=%+v calls=%d status=%+v error=%v",
			result, calls, restarted.Status, err)
	}

	lostResponse = false
	result, err = newReconciler().reconcileInitialPrimary(ctx, restarted)
	if err != nil || result.IsZero() || calls != 2 {
		t.Fatalf("повтор после проверки цели: result=%+v calls=%d error=%v", result, calls, err)
	}
	restarted.Status.Nodes[0].Role = valkeyv1alpha1.NodeRolePrimary
	result, err = newReconciler().reconcileInitialPrimary(ctx, restarted)
	if err != nil || !result.IsZero() || calls != 2 {
		t.Fatalf("подтверждённый primary назначен повторно: result=%+v calls=%d error=%v", result, calls, err)
	}
}

func Test_HA02_ReconcileInitialPrimary_WhenTargetIsReplaced_DoesNotMoveToUnprovenProcess(t *testing.T) {
	instance, pod, _, _ := processObservationObjects()
	instance.Spec.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	previous := testObservedNode(pod)
	previous.Role = valkeyv1alpha1.NodeRoleReplica
	previous.AppPasswordVersion = 1
	ordinal := int32(0)
	instance.Status.PrimaryOrdinal = &ordinal
	instance.Status.PrimaryPodUID = previous.PodUID
	instance.Status.PrimaryContainerID = previous.ContainerID
	instance.Status.PrimaryRunID = previous.RunID
	instance.Status.PrimaryNodeName = previous.NodeName
	instance.Status.PrimaryNodeUID = previous.NodeUID
	current := previous
	current.PodUID = "replacement-pod"
	current.ContainerID = "containerd://replacement"
	current.RunID = "replacement-run"
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{current}
	instance.Status.PreviousProcesses = []valkeyv1alpha1.NodeStatus{previous}

	called := false
	reconciler := &ValkeyInstanceReconciler{PromoteProcess: func(
		context.Context,
		string,
		string,
		string,
	) (operatorvalkey.ProcessState, error) {
		called = true
		return operatorvalkey.ProcessState{}, nil
	}}
	result, err := reconciler.reconcileInitialPrimary(context.Background(), instance)
	if err != nil || result.RequeueAfter == 0 || called || primaryIdentityMatches(instance, current) {
		t.Fatalf("неподтверждённая замена получила primary: result=%+v called=%t status=%+v error=%v",
			result, called, instance.Status, err)
	}
}

func Test_ReconcileInitialPrimary_WhenAssignmentStartsFromEmptyProcesses_PreservesInitializedHistory(t *testing.T) {
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.Initialized = true
	instance.Status.PrimaryOrdinal = nil
	instance.Status.Reason = "EMPTY_RECOVERY"
	instance.Status.Conditions = append(instance.Status.Conditions, metav1.Condition{
		Type: conditionTypeDataLoss, Status: metav1.ConditionTrue, Reason: "AllProcessesStopped",
	})
	if !emptyPrimaryAssignmentAllowed(instance) {
		t.Fatal("подтверждённая пустая замена не получила право назначить primary")
	}
	ordinal := int32(0)
	instance.Status.PrimaryOrdinal = &ordinal
	if !emptyPrimaryAssignmentAllowed(instance) {
		t.Fatal("сохранённая цель пустой замены не получила право завершить назначение primary")
	}
	primary := failoverNode(ordinal, valkeyv1alpha1.NodeRolePrimary, "", emptyReplicaInitialOffset, nil)
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{primary}
	setPrimaryIdentity(&instance.Status, primary)
	if emptyPrimaryAssignmentAllowed(instance) {
		t.Fatal("подтверждённый primary остался в режиме пустого назначения")
	}
	instance.Status.PrimaryOrdinal = nil
	instance.Status.Nodes = nil
	instance.Status.Reason = "PRIMARY_NOT_READY"
	if emptyPrimaryAssignmentAllowed(instance) {
		t.Fatal("старая отметка потери данных разрешила новое пустое назначение")
	}
	instance.Status.Rollout = &valkeyv1alpha1.RolloutStatus{
		VCPU: 1, RAMGB: 1, Stage: valkeyv1alpha1.RolloutStageStarting,
	}
	instance.Status.Applied = &valkeyv1alpha1.AppliedConfiguration{
		Mode: valkeyv1alpha1.ValkeyModeHA, VCPU: 2, RAMGB: 2,
	}
	if !emptyPrimaryAssignmentAllowed(instance) {
		t.Fatal("подтверждённый пустой запуск rollout не получил право назначить primary")
	}
	failed := failoverNode(0, valkeyv1alpha1.NodeRoleReplica, "", emptyReplicaInitialOffset, nil)
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{failed}
	setPrimaryIdentity(&instance.Status, failed)
	if !emptyPrimaryAssignmentAllowed(instance) {
		t.Fatal("сохранённая цель full-stop rollout не получила право завершить назначение primary")
	}
	failed.PodUID = "replacement-pod"
	failed.ContainerID = "containerd://replacement"
	failed.RunID = "replacement-run"
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{failed}
	if emptyPrimaryAssignmentAllowed(instance) {
		t.Fatal("замена отказавшего primary получила пустое назначение поверх реплик")
	}
}

func Test_HA01_ReconcileReplicas_WhenPrimaryIsKnown_ConfiguresReplicasToFollowPrimaryPodIP(t *testing.T) {
	ctx := context.Background()
	instance, basePod, _, secret := processObservationObjects()
	instance.Spec.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.Initialized = true
	primaryOrdinal := int32(0)
	instance.Status.PrimaryOrdinal = &primaryOrdinal
	objects := []client.Object{instance, secret}
	for ordinal := range int32(3) {
		pod := basePod.DeepCopy()
		pod.Name = fmt.Sprintf("%s-%d", instance.Name, ordinal)
		pod.UID = types.UID(fmt.Sprintf("pod-%d", ordinal))
		pod.Spec.NodeName = fmt.Sprintf("worker-%d", ordinal)
		pod.Status.PodIP = fmt.Sprintf("10.42.0.%d", ordinal+1)
		pod.Status.ContainerStatuses[0].ContainerID = fmt.Sprintf("containerd://%d", ordinal)
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: pod.Spec.NodeName, UID: types.UID(fmt.Sprintf("node-%d", ordinal)),
		}}
		role := valkeyv1alpha1.NodeRoleReplica
		if ordinal == primaryOrdinal {
			role = valkeyv1alpha1.NodeRolePrimary
		}
		process := valkeyv1alpha1.NodeStatus{
			Ordinal: ordinal, PodUID: string(pod.UID),
			ContainerID: pod.Status.ContainerStatuses[0].ContainerID,
			RunID:       fmt.Sprintf("run-%d", ordinal),
			NodeName:    node.Name, NodeUID: string(node.UID), Role: role,
			AppPasswordVersion: 1,
		}
		if ordinal == 1 {
			process.Replication = &valkeyv1alpha1.ReplicationStatus{
				UpstreamHost: "10.42.0.1", UpstreamPort: valkeyPort,
			}
		}
		instance.Status.Nodes = append(instance.Status.Nodes, process)
		objects = append(objects, pod, node)
	}
	primary := instance.Status.Nodes[0]
	instance.Status.PrimaryPodUID = primary.PodUID
	instance.Status.PrimaryContainerID = primary.ContainerID
	instance.Status.PrimaryRunID = primary.RunID
	instance.Status.PrimaryNodeName = primary.NodeName
	instance.Status.PrimaryNodeUID = primary.NodeUID
	k8s := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(objects...).Build()
	calls := 0
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		FollowProcess: func(
			_ context.Context,
			address string,
			_ string,
			_ string,
			upstream string,
			port int32,
		) (operatorvalkey.ProcessState, error) {
			calls++
			if address != "10.42.0.3:6379" || upstream != "10.42.0.1" || port != 6379 {
				t.Fatalf("неверное прямое подключение реплики: %s -> %s:%d", address, upstream, port)
			}
			return operatorvalkey.ProcessState{Role: "replica", RunID: "run-2"}, nil
		},
	}

	result, err := reconciler.reconcileReplicas(ctx, instance)
	if err != nil || result.IsZero() || calls != 1 {
		t.Fatalf("подключить реплику: result=%+v calls=%d error=%v", result, calls, err)
	}
	instance.Status.Nodes[2].Replication = &valkeyv1alpha1.ReplicationStatus{
		UpstreamHost: "10.42.0.1", UpstreamPort: valkeyPort,
	}
	result, err = reconciler.reconcileReplicas(ctx, instance)
	if err != nil || !result.IsZero() || calls != 1 {
		t.Fatalf("готовая топология изменена повторно: result=%+v calls=%d error=%v", result, calls, err)
	}
}
