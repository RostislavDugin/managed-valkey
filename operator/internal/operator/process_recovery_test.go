package operator

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func Test_FP06_ReconcileProcessRecovery_WhenProcessIsUnresponsive_PersistsDeletionBeforeDeletingPod(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	instance, pod, node, secret := processObservationObjects()
	pod.Finalizers = []string{processFinalizer}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	process := testObservedNode(pod)
	since := metav1.NewTime(now.Add(-config.ProcessUnresponsiveTimeout))
	process.Observation = &valkeyv1alpha1.ProcessObservationStatus{
		Kind:                       valkeyv1alpha1.ProcessObservationTransportError,
		ConsecutiveTransportErrors: config.PrimaryFailureThreshold,
		TransportErrorSince:        &since,
		ObservedAt:                 since,
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{process}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}, &corev1.Pod{}).
		WithObjects(instance, pod, node, secret).
		Build()
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s, APIReader: k8s, Clock: clocktesting.NewFakeClock(now),
	}

	if result, err := reconciler.reconcileProcessRecovery(ctx, instance); err != nil || result.IsZero() {
		t.Fatalf("сохранить решение об остановке: result=%+v error=%v", result, err)
	}
	if instance.Status.Nodes[0].Recovery == nil ||
		instance.Status.Nodes[0].Recovery.Stage != valkeyv1alpha1.ProcessRecoveryStageDeleting {
		t.Fatalf("решение об остановке не сохранено: %+v", instance.Status.Nodes[0])
	}
	if result, err := reconciler.reconcileProcessRecovery(ctx, instance); err != nil || result.IsZero() {
		t.Fatalf("запросить удаление: result=%+v error=%v", result, err)
	}
	if instance.Status.Nodes[0].Recovery.Stage != valkeyv1alpha1.ProcessRecoveryStageWaitingForTermination ||
		instance.Status.Nodes[0].Recovery.DeleteRequestedAt == nil {
		t.Fatalf("принятый DELETE не сохранён: %+v", instance.Status.Nodes[0].Recovery)
	}
}

func Test_FP06_ReconcileProcessRecovery_WhenProcessIdentityChanges_DoesNotApplyRecoveryToReplacement(t *testing.T) {
	requestedAt := metav1.Now()
	current := valkeyv1alpha1.NodeStatus{
		Ordinal: 0, PodUID: "pod-old", ContainerID: "containerd://old", RunID: "run-old",
		NodeName: "worker-1", NodeUID: "node-1",
		Recovery: &valkeyv1alpha1.ProcessRecoveryStatus{
			Reason:            valkeyv1alpha1.ProcessRecoveryUnresponsive,
			Stage:             valkeyv1alpha1.ProcessRecoveryStageWaitingForTermination,
			DeleteRequestedAt: &requestedAt,
		},
	}
	same := *current.DeepCopy()
	same.Recovery = nil
	if recovery := retainedProcessRecovery(current, same, nil); recovery == nil ||
		recovery.DeleteRequestedAt == current.Recovery.DeleteRequestedAt {
		t.Fatalf("восстановление точного процесса не сохранено отдельной копией: %+v", recovery)
	}
	replacement := same
	replacement.PodUID = "pod-new"
	replacement.ContainerID = "containerd://new"
	replacement.RunID = "run-new"
	if recovery := retainedProcessRecovery(current, replacement, nil); recovery != nil {
		t.Fatalf("новый процесс унаследовал восстановление старого: %+v", recovery)
	}
}

func Test_FP09_ReconcileProcessRecovery_WhenBusyProcessIsUnkillable_AdvancesToDeletion(t *testing.T) {
	ctx := context.Background()
	instance, pod, node, secret := processObservationObjects()
	process := testObservedNode(pod)
	process.Recovery = &valkeyv1alpha1.ProcessRecoveryStatus{
		Reason:    valkeyv1alpha1.ProcessRecoveryBusy,
		Stage:     valkeyv1alpha1.ProcessRecoveryStageKillingBusy,
		StartedAt: metav1.Now(),
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{process}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, node, secret).
		Build()
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s, APIReader: k8s,
		StopBusyProcess: func(context.Context, string, string) error {
			return operatorvalkey.ErrUnkillable
		},
	}

	if result, err := reconciler.reconcileProcessRecovery(ctx, instance); err != nil || result.IsZero() {
		t.Fatalf("обработать UNKILLABLE: result=%+v error=%v", result, err)
	}
	if instance.Status.Nodes[0].Recovery.Stage != valkeyv1alpha1.ProcessRecoveryStageDeleting {
		t.Fatalf("UNKILLABLE не привёл к остановке: %+v", instance.Status.Nodes[0].Recovery)
	}
}

func Test_FP08_ReconcileProcessRecovery_WhenNoBusyProcessRemains_ClearsRecovery(t *testing.T) {
	ctx := context.Background()
	instance, pod, node, secret := processObservationObjects()
	process := testObservedNode(pod)
	process.Recovery = &valkeyv1alpha1.ProcessRecoveryStatus{
		Reason:    valkeyv1alpha1.ProcessRecoveryBusy,
		Stage:     valkeyv1alpha1.ProcessRecoveryStageKillingBusy,
		StartedAt: metav1.Now(),
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{process}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, node, secret).
		Build()
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s, APIReader: k8s,
		StopBusyProcess: func(context.Context, string, string) error {
			return operatorvalkey.ErrNoBusyProcess
		},
	}

	if _, err := reconciler.reconcileProcessRecovery(ctx, instance); err != nil &&
		!errors.Is(err, operatorvalkey.ErrNoBusyProcess) {
		t.Fatalf("завершившийся BUSY: %v", err)
	}
	if instance.Status.Nodes[0].Recovery != nil {
		t.Fatalf("ожидание BUSY не очищено: %+v", instance.Status.Nodes[0].Recovery)
	}
}

func Test_FP07_ReconcileProcessRecovery_WhenNonTransportResponseArrives_CancelsUnacceptedDeletion(t *testing.T) {
	for _, kind := range []valkeyv1alpha1.ProcessObservationKind{
		valkeyv1alpha1.ProcessObservationBusy,
		valkeyv1alpha1.ProcessObservationLoading,
		valkeyv1alpha1.ProcessObservationAuthError,
	} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			instance, _, _, _ := processObservationObjects()
			process := valkeyv1alpha1.NodeStatus{
				Ordinal: 0, PodUID: "pod-1", ContainerID: "containerd://1", RunID: "run-1",
				NodeName: "worker-1", NodeUID: "node-1",
				Observation: &valkeyv1alpha1.ProcessObservationStatus{Kind: kind, ObservedAt: metav1.Now()},
				Recovery: &valkeyv1alpha1.ProcessRecoveryStatus{
					Reason: valkeyv1alpha1.ProcessRecoveryUnresponsive,
					Stage:  valkeyv1alpha1.ProcessRecoveryStageDeleting, StartedAt: metav1.Now(),
				},
			}
			instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{process}
			k8s := fake.NewClientBuilder().
				WithScheme(NewScheme()).
				WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
				WithObjects(instance).
				Build()

			result, err := (&ValkeyInstanceReconciler{Client: k8s}).reconcileProcessRecovery(ctx, instance)
			if err != nil || result.IsZero() || instance.Status.Nodes[0].Recovery != nil {
				t.Fatalf("ответ %s не отменил DELETE: result=%+v status=%+v error=%v",
					kind, result, instance.Status.Nodes[0], err)
			}
		})
	}
}

func Test_FP09_ReconcileProcessRecovery_WhenKillingBusyProcessReachesTransportThreshold_AdvancesToDeletion(
	t *testing.T,
) {
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	instance, _, _, _ := processObservationObjects()
	since := metav1.NewTime(now.Add(-config.ProcessUnresponsiveTimeout))
	process := valkeyv1alpha1.NodeStatus{
		Ordinal: 0, PodUID: "pod-1", ContainerID: "containerd://1", RunID: "run-1",
		NodeName: "worker-1", NodeUID: "node-1",
		Observation: &valkeyv1alpha1.ProcessObservationStatus{
			Kind:                       valkeyv1alpha1.ProcessObservationTransportError,
			ConsecutiveTransportErrors: config.PrimaryFailureThreshold,
			TransportErrorSince:        &since, ObservedAt: since,
		},
		Recovery: &valkeyv1alpha1.ProcessRecoveryStatus{
			Reason: valkeyv1alpha1.ProcessRecoveryBusy,
			Stage:  valkeyv1alpha1.ProcessRecoveryStageKillingBusy, StartedAt: since,
		},
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{process}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()

	result, err := (&ValkeyInstanceReconciler{
		Client: k8s, Clock: clocktesting.NewFakeClock(now),
	}).reconcileProcessRecovery(ctx, instance)
	if err != nil || result.IsZero() ||
		instance.Status.Nodes[0].Recovery.Stage != valkeyv1alpha1.ProcessRecoveryStageDeleting {
		t.Fatalf("остановка BUSY не продолжилась после потери ответов: result=%+v status=%+v error=%v",
			result, instance.Status.Nodes[0], err)
	}
}

func Test_FP01_ReconcileProcessRecovery_WhenReplicaRecoveryIsPending_AllowsPrimaryFailover(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.Initialized = true
	primary := failoverNode(0, valkeyv1alpha1.NodeRolePrimary, "history-a", 100, nil)
	primary.Termination = &valkeyv1alpha1.ProcessTermination{Evidence: "container_status"}
	replica := failoverNode(1, valkeyv1alpha1.NodeRoleReplica, "history-a", 100, nil)
	replica.Recovery = &valkeyv1alpha1.ProcessRecoveryStatus{
		Reason:    valkeyv1alpha1.ProcessRecoveryUnresponsive,
		Stage:     valkeyv1alpha1.ProcessRecoveryStageWaitingForTermination,
		StartedAt: metav1.Now(),
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{replica, primary}
	setPrimaryIdentity(&instance.Status, primary)
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s}

	recoveryResult, err := reconciler.reconcileProcessRecovery(ctx, instance)
	if err != nil || recoveryResult.RequeueAfter == 0 || resultRequestsImmediateRequeue(recoveryResult) {
		t.Fatalf("ожидание реплики должно оставить failover доступным: result=%+v error=%v", recoveryResult, err)
	}
	failoverResult, err := reconciler.reconcileFailover(ctx, instance)
	if err != nil || failoverResult.IsZero() || instance.Status.Failover == nil {
		t.Fatalf("ожидание реплики заблокировало failover primary: result=%+v status=%+v error=%v",
			failoverResult, instance.Status, err)
	}
}
