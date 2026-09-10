//go:build envtest

package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func TestEnvtestCT01CT02CT06CT07ReviewBoundaries(t *testing.T) {
	k8s := startReviewEnvtest(t)
	t.Run("CT-01 serializes status updates", func(t *testing.T) {
		testEnvtestCT01StatusSerialization(t, k8s)
	})
	t.Run("CT-02 resets transport series on replies", func(t *testing.T) {
		testEnvtestCT02ObservationKinds(t, k8s)
	})
	t.Run("CT-06 rejects stale and live-node fencing", func(t *testing.T) {
		testEnvtestCT06ManualFencing(t, k8s)
	})
	t.Run("CT-07 requires fencing before empty timeout", func(t *testing.T) {
		testEnvtestCT07EmptyRecovery(t, k8s)
	})
}

func startReviewEnvtest(t *testing.T) client.Client {
	t.Helper()
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	restConfig, err := environment.Start()
	if err != nil {
		t.Fatalf("запустить envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("остановить envtest: %v", err)
		}
	})
	k8s, err := client.New(restConfig, client.Options{Scheme: NewScheme()})
	if err != nil {
		t.Fatalf("создать клиент envtest: %v", err)
	}
	return k8s
}

func testEnvtestCT01StatusSerialization(t *testing.T, k8s client.Client) {
	ctx := context.Background()
	instance := reviewEnvtestInstance("review-ct01")
	createReviewEnvtestInstance(t, ctx, k8s, instance)
	concurrent := &valkeyv1alpha1.ValkeyInstance{}
	stale := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), concurrent); err != nil {
		t.Fatalf("прочитать первую копию CR: %v", err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), stale); err != nil {
		t.Fatalf("прочитать вторую копию CR: %v", err)
	}
	concurrent.Status.Reason = "CONCURRENT_OBSERVATION"
	if err := k8s.Status().Update(ctx, concurrent); err != nil {
		t.Fatalf("сохранить конкурентное наблюдение: %v", err)
	}
	stale.Status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
	if err := k8s.Status().Update(ctx, stale); err == nil {
		t.Fatal("устаревшая запись status не получила conflict")
	}
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s}
	changed, err := reconciler.updateStatus(ctx, stale, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		status.Phase = valkeyv1alpha1.InstancePhaseDegraded
	})
	if err != nil || !changed || stale.Status.Phase != valkeyv1alpha1.InstancePhaseDegraded ||
		stale.Status.Reason != "CONCURRENT_OBSERVATION" {
		t.Fatalf("повтор status потерял конкурентное поле: changed=%t status=%+v error=%v",
			changed, stale.Status, err)
	}
}

func testEnvtestCT02ObservationKinds(t *testing.T, k8s client.Client) {
	ctx := context.Background()
	instance := reviewEnvtestInstance("review-ct02")
	instance.Status.Initialized = true
	instance.Status.Phase = valkeyv1alpha1.InstancePhaseRunning
	primary := reviewEnvtestProcess(0, valkeyv1alpha1.NodeRolePrimary)
	startedAt := metav1.NewTime(time.Now().Add(-config.PrimaryFailureMinDuration))
	primary.Observation = &valkeyv1alpha1.ProcessObservationStatus{
		Kind: valkeyv1alpha1.ProcessObservationTransportError, ObservedAt: startedAt,
		ConsecutiveTransportErrors: config.PrimaryFailureThreshold, TransportErrorSince: &startedAt,
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{primary}
	setPrimaryIdentity(&instance.Status, primary)
	createReviewEnvtestInstance(t, ctx, k8s, instance)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: "pod-0"},
		Spec:       corev1.PodSpec{NodeName: "worker-0"},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}},
	}
	container := &corev1.ContainerStatus{
		Name: "valkey", ContainerID: "containerd://0",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", UID: "node-0"}}
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s}
	for _, test := range []struct {
		cause error
		kind  valkeyv1alpha1.ProcessObservationKind
	}{
		{cause: operatorvalkey.ErrBusy, kind: valkeyv1alpha1.ProcessObservationBusy},
		{cause: operatorvalkey.ErrLoading, kind: valkeyv1alpha1.ProcessObservationLoading},
		{cause: operatorvalkey.ErrAuthentication, kind: valkeyv1alpha1.ProcessObservationAuthError},
	} {
		if _, err := reconciler.recordProcessObservationError(
			ctx, instance, pod, node, container, 0, test.cause,
		); err != nil {
			t.Fatalf("сохранить ответ %s: %v", test.kind, err)
		}
		observation := instance.Status.Nodes[0].Observation
		if observation == nil || observation.Kind != test.kind ||
			observation.ConsecutiveTransportErrors != 0 || observation.TransportErrorSince != nil {
			t.Fatalf("ответ %s не сбросил транспортную серию: %+v", test.kind, observation)
		}
	}
}

func testEnvtestCT06ManualFencing(t *testing.T, k8s client.Client) {
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "review-worker-ct06"}}
	if err := k8s.Create(ctx, node); err != nil {
		t.Fatalf("создать Node: %v", err)
	}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if err := k8s.Status().Update(ctx, node); err != nil {
		t.Fatalf("сохранить готовность Node: %v", err)
	}
	instance := reviewEnvtestInstance("review-ct06")
	process := reviewEnvtestProcess(0, valkeyv1alpha1.NodeRolePrimary)
	process.NodeName = node.Name
	process.NodeUID = string(node.UID)
	process.Recovery = &valkeyv1alpha1.ProcessRecoveryStatus{
		Reason:    valkeyv1alpha1.ProcessRecoveryUnresponsive,
		Stage:     valkeyv1alpha1.ProcessRecoveryStageWaitingForTermination,
		StartedAt: metav1.Now(),
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{process}
	setPrimaryIdentity(&instance.Status, process)
	stale := processIdentity(process)
	stale.RunID = "stale-run"
	encoded, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	instance.Annotations = map[string]string{manualFencingAnnotation: string(encoded)}
	createReviewEnvtestInstance(t, ctx, k8s, instance)
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s}
	if result, err := reconciler.reconcileManualFencing(ctx, instance); err != nil || !result.IsZero() ||
		instance.Status.Nodes[0].Termination != nil {
		t.Fatalf("устаревшее fencing принято: result=%+v status=%+v error=%v", result, instance.Status, err)
	}
	exact, err := json.Marshal(processIdentity(process))
	if err != nil {
		t.Fatal(err)
	}
	before := instance.DeepCopy()
	instance.Annotations[manualFencingAnnotation] = string(exact)
	if err := k8s.Patch(ctx, instance, client.MergeFrom(before)); err != nil {
		t.Fatalf("записать точное fencing: %v", err)
	}
	if result, err := reconciler.reconcileManualFencing(ctx, instance); err != nil || result.RequeueAfter == 0 {
		t.Fatalf("дождаться остановки Node: result=%+v error=%v", result, err)
	}
	condition := apimeta.FindStatusCondition(instance.Status.Conditions, conditionTypeManualFencing)
	if condition == nil || condition.Reason != "NodeStillReady" || instance.Status.Nodes[0].Termination != nil {
		t.Fatalf("готовая Node разрешила fencing: condition=%+v status=%+v", condition, instance.Status)
	}
	currentNode := &corev1.Node{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(node), currentNode); err != nil {
		t.Fatalf("перечитать Node: %v", err)
	}
	currentNode.Status.Conditions = []corev1.NodeCondition{{
		Type: corev1.NodeReady, Status: corev1.ConditionFalse,
	}}
	if err := k8s.Status().Update(ctx, currentNode); err != nil {
		t.Fatalf("остановить Node: %v", err)
	}
	if result, err := reconciler.reconcileManualFencing(ctx, instance); err != nil || result.IsZero() ||
		instance.Status.Nodes[0].Termination == nil ||
		instance.Status.Nodes[0].Termination.Evidence != "manual_fencing" {
		t.Fatalf("точное fencing остановленной Node не принято: result=%+v status=%+v error=%v",
			result, instance.Status, err)
	}
}

func testEnvtestCT07EmptyRecovery(t *testing.T, k8s client.Client) {
	ctx := context.Background()
	now := time.Now().UTC()
	emptySince := metav1.NewTime(now.Add(-config.EmptyPrimaryTimeout))
	instance := reviewEnvtestInstance("review-ct07")
	instance.Status.Initialized = true
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.Nodes = nil
	instance.Status.PreviousProcesses = nil
	for ordinal := int32(0); ordinal < 3; ordinal++ {
		previous := reviewEnvtestProcess(ordinal, valkeyv1alpha1.NodeRoleReplica)
		previous.Replication.ReplicationID = "history-a"
		previous.Replication.Offset = 10
		if ordinal == 0 {
			previous.Role = valkeyv1alpha1.NodeRolePrimary
		} else {
			previous.Termination = &valkeyv1alpha1.ProcessTermination{
				Reason: "Completed", FinishedAt: metav1.Now(), Evidence: "container_status",
			}
		}
		instance.Status.PreviousProcesses = append(instance.Status.PreviousProcesses, previous)
		fresh := reviewEnvtestProcess(ordinal, valkeyv1alpha1.NodeRoleReplica)
		fresh.PodUID = fmt.Sprintf("fresh-pod-%d", ordinal)
		fresh.ContainerID = fmt.Sprintf("containerd://fresh-%d", ordinal)
		fresh.RunID = fmt.Sprintf("fresh-run-%d", ordinal)
		fresh.AppEnabled = false
		fresh.Replication.ReplicationID = ""
		fresh.Replication.Offset = emptyReplicaInitialOffset
		fresh.Replication.LinkUp = false
		fresh.Replication.SyncedAt = nil
		instance.Status.Nodes = append(instance.Status.Nodes, fresh)
	}
	source := instance.Status.PreviousProcesses[0]
	setPrimaryIdentity(&instance.Status, source)
	instance.Status.Failover = &valkeyv1alpha1.FailoverStatus{
		Reason: valkeyv1alpha1.FailoverReasonFailure, Stage: valkeyv1alpha1.FailoverStageChoosing,
		StartedAt: metav1.NewTime(now.Add(-config.EmptyPrimaryTimeout)),
		Source:    processIdentity(source), EmptySince: &emptySince,
	}
	createReviewEnvtestInstance(t, ctx, k8s, instance)
	clock := clocktesting.NewFakeClock(now)
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s, APIReader: k8s,
		Clock: clock,
	}
	if _, err := reconciler.reconcileFailoverChoosing(ctx, instance); err != nil {
		t.Fatalf("проверить пустое восстановление без fencing: %v", err)
	}
	if instance.Status.Failover == nil || instance.Status.PrimaryOrdinal == nil ||
		instance.Status.Reason == "EMPTY_RECOVERY" {
		t.Fatalf("таймаут без fencing разрешил пустое восстановление: %+v", instance.Status)
	}
	changed, err := reconciler.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		status.Failover.SourceAppDisabled = true
		status.Failover.SourceClientsKilled = true
	})
	if err != nil || !changed {
		t.Fatalf("сохранить fencing источника: changed=%t error=%v", changed, err)
	}
	if result, err := reconciler.reconcileFailoverChoosing(ctx, instance); err != nil || result.IsZero() ||
		instance.Status.Failover == nil || instance.Status.Failover.EmptySince == nil {
		t.Fatalf("fencing не начал новый безопасный отсчёт: result=%+v status=%+v error=%v",
			result, instance.Status, err)
	}
	clock.Step(config.EmptyPrimaryTimeout)
	if result, err := reconciler.reconcileFailoverChoosing(ctx, instance); err != nil || result.IsZero() ||
		instance.Status.Failover != nil || instance.Status.PrimaryOrdinal != nil ||
		instance.Status.Reason != "EMPTY_RECOVERY" {
		t.Fatalf("fencing не разрешил пустое восстановление после срока: result=%+v status=%+v error=%v",
			result, instance.Status, err)
	}
}

func reviewEnvtestInstance(name string) *valkeyv1alpha1.ValkeyInstance {
	return &valkeyv1alpha1.ValkeyInstance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: valkeyv1alpha1.ValkeyInstanceSpec{
			InstanceID: "01991ad0-1234-7000-8000-000000000001", Slug: name,
			Mode: valkeyv1alpha1.ValkeyModeSingle, VCPU: 1, RAMGB: 4, PublicPort: 41379,
			Whitelist: &valkeyv1alpha1.WhitelistSpec{}, PasswordVersion: 1, DesiredGeneration: 1,
		},
		Status: valkeyv1alpha1.ValkeyInstanceStatus{
			Phase: valkeyv1alpha1.InstancePhaseRunning,
			AcceptedConfiguration: &valkeyv1alpha1.AcceptedConfiguration{
				InstanceID: "01991ad0-1234-7000-8000-000000000001", Slug: name,
				Mode: valkeyv1alpha1.ValkeyModeSingle, VCPU: 1, RAMGB: 4, PublicPort: 41379,
				Whitelist: valkeyv1alpha1.WhitelistSpec{}, PasswordVersion: 1, DesiredGeneration: 1,
			},
		},
	}
}

func reviewEnvtestProcess(ordinal int32, role valkeyv1alpha1.NodeRole) valkeyv1alpha1.NodeStatus {
	return valkeyv1alpha1.NodeStatus{
		Ordinal: ordinal, PodUID: fmt.Sprintf("pod-%d", ordinal),
		ContainerID: fmt.Sprintf("containerd://%d", ordinal), RunID: fmt.Sprintf("run-%d", ordinal),
		NodeName: fmt.Sprintf("worker-%d", ordinal), NodeUID: fmt.Sprintf("node-%d", ordinal),
		Role: role, Readiness: true, AppPasswordVersion: 1,
		Replication: &valkeyv1alpha1.ReplicationStatus{
			ReplicationID: "history", LinkUp: true, ObservedAt: metav1.Now(),
		},
	}
}

func createReviewEnvtestInstance(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	instance *valkeyv1alpha1.ValkeyInstance,
) {
	t.Helper()
	status := instance.Status.DeepCopy()
	instance.Status = valkeyv1alpha1.ValkeyInstanceStatus{}
	if err := k8s.Create(ctx, instance); err != nil {
		t.Fatalf("создать ValkeyInstance: %v", err)
	}
	instance.Status = *status
	if err := k8s.Status().Update(ctx, instance); err != nil {
		t.Fatalf("сохранить status ValkeyInstance: %v", err)
	}
}
