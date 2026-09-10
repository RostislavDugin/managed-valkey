package operator

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func TestFP12ChooseMostAdvancedCandidateFromSameHistory(t *testing.T) {
	syncedAt := metav1.NewTime(time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC))
	source := failoverNode(0, valkeyv1alpha1.NodeRolePrimary, "history-a", 100, &syncedAt)
	older := failoverNode(1, valkeyv1alpha1.NodeRoleReplica, "history-a", 90, &syncedAt)
	newer := failoverNode(2, valkeyv1alpha1.NodeRoleReplica, "history-a", 95, &syncedAt)
	newer.Readiness = false
	newer.Replication.LinkUp = false
	foreign := failoverNode(3, valkeyv1alpha1.NodeRoleReplica, "history-b", 1000, &syncedAt)

	candidate, found := chooseFailoverCandidate(source, []valkeyv1alpha1.NodeStatus{
		foreign, older, newer,
	})
	if !found || candidate.Ordinal != newer.Ordinal {
		t.Fatalf("выбран неверный кандидат: found=%t candidate=%+v", found, candidate)
	}
	source.Replication.ReplicationID = ""
	if _, found := chooseFailoverCandidate(source, []valkeyv1alpha1.NodeStatus{newer}); found {
		t.Fatal("кандидат без известной общей истории принят")
	}
}

func TestOP04PromotionLostResponseUsesPersistedCandidate(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.PasswordVersion = 1
	instance.Status.Initialized = true
	source := failoverNode(0, valkeyv1alpha1.NodeRolePrimary, "history-a", 100, nil)
	candidate := failoverNode(2, valkeyv1alpha1.NodeRolePrimary, "history-a", 100, nil)
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{source, candidate}
	identity := processIdentity(candidate)
	instance.Status.Failover = &valkeyv1alpha1.FailoverStatus{
		Reason:                 valkeyv1alpha1.FailoverReasonFailure,
		Stage:                  valkeyv1alpha1.FailoverStagePromoting,
		StartedAt:              metav1.Now(),
		Source:                 processIdentity(source),
		Candidate:              &identity,
		CandidateAppDisabled:   true,
		CandidateClientsKilled: true,
		CandidateMayBePrimary:  true,
	}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	promoteCalls := 0
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		PromoteProcess: func(context.Context, string, string, string) (operatorvalkey.ProcessState, error) {
			promoteCalls++
			return operatorvalkey.ProcessState{}, nil
		},
	}

	result, err := reconciler.reconcileFailoverPromoting(ctx, instance)
	if err != nil || result.IsZero() || promoteCalls != 0 {
		t.Fatalf("фактическая роль после потерянного ответа: result=%+v calls=%d error=%v", result, promoteCalls, err)
	}
	if instance.Status.Failover.Stage != valkeyv1alpha1.FailoverStageReconfiguring ||
		instance.Status.PrimaryOrdinal == nil || *instance.Status.PrimaryOrdinal != candidate.Ordinal ||
		instance.Status.PrimaryRunID != candidate.RunID {
		t.Fatalf("сохранённый кандидат не принят как primary: %+v", instance.Status)
	}
}

func TestOP03ReconfiguringRestartsFailoverWhenNewPrimaryFails(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.Initialized = true
	primary := failoverNode(1, valkeyv1alpha1.NodeRolePrimary, "history-a", 100, nil)
	primary.Termination = &valkeyv1alpha1.ProcessTermination{Evidence: "container_status"}
	replica := failoverNode(2, valkeyv1alpha1.NodeRoleReplica, "history-a", 100, nil)
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{primary, replica}
	setPrimaryIdentity(&instance.Status, primary)
	oldSource := failoverNode(0, valkeyv1alpha1.NodeRoleReplica, "history-a", 95, nil)
	candidate := processIdentity(primary)
	instance.Status.Failover = &valkeyv1alpha1.FailoverStatus{
		Reason: valkeyv1alpha1.FailoverReasonFailure, Stage: valkeyv1alpha1.FailoverStageReconfiguring,
		StartedAt: metav1.Now(), Source: processIdentity(oldSource), Candidate: &candidate,
		SourceAppDisabled: true, SourceClientsKilled: true,
		CandidateAppDisabled: true, CandidateClientsKilled: true, CandidateMayBePrimary: true,
	}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()

	result, err := (&ValkeyInstanceReconciler{Client: k8s}).reconcileFailoverReconfiguring(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("повторно начать failover: result=%+v error=%v", result, err)
	}
	operation := instance.Status.Failover
	if operation == nil || operation.Stage != valkeyv1alpha1.FailoverStageFencing ||
		!sameIdentity(operation.Source, processIdentity(primary)) || operation.Candidate != nil ||
		operation.CandidateMayBePrimary || instance.Status.Reason != "FENCING_REQUIRED" {
		t.Fatalf("отказ нового primary не сохранил новый fencing: %+v", instance.Status)
	}
}

func TestFP11AllStoppedProcessesPermitEmptyRecovery(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.PasswordVersion = 1
	instance.Status.Initialized = true
	previous := make([]valkeyv1alpha1.NodeStatus, 0, 3)
	current := make([]valkeyv1alpha1.NodeStatus, 0, 3)
	for ordinal := int32(0); ordinal < 3; ordinal++ {
		old := failoverNode(ordinal, valkeyv1alpha1.NodeRoleReplica, "history-a", 100, nil)
		if ordinal == 0 {
			old.Role = valkeyv1alpha1.NodeRolePrimary
		}
		old.Termination = &valkeyv1alpha1.ProcessTermination{
			Reason: "Completed", FinishedAt: metav1.Now(), Evidence: "container_status",
		}
		previous = append(previous, old)
		fresh := failoverNode(ordinal, valkeyv1alpha1.NodeRoleReplica, "", emptyReplicaInitialOffset, nil)
		fresh.PodUID = fmt.Sprintf("fresh-pod-%d", ordinal)
		fresh.ContainerID = fmt.Sprintf("fresh-container-%d", ordinal)
		fresh.RunID = fmt.Sprintf("fresh-run-%d", ordinal)
		fresh.AppEnabled = false
		fresh.Replication.LinkUp = false
		current = append(current, fresh)
	}
	instance.Status.Nodes = current
	instance.Status.PreviousProcesses = previous
	setPrimaryIdentity(&instance.Status, previous[0])
	instance.Status.Failover = &valkeyv1alpha1.FailoverStatus{
		Reason: valkeyv1alpha1.FailoverReasonFailure, Stage: valkeyv1alpha1.FailoverStageChoosing,
		StartedAt: metav1.Now(), Source: processIdentity(previous[0]),
	}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s}

	result, err := reconciler.reconcileFailoverChoosing(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("начать пустое восстановление: result=%+v error=%v", result, err)
	}
	if !instance.Status.Initialized || instance.Status.Failover != nil || instance.Status.PrimaryOrdinal != nil ||
		instance.Status.Phase != valkeyv1alpha1.InstancePhaseUnavailable ||
		instance.Status.Reason != "EMPTY_RECOVERY" {
		t.Fatalf("пустое восстановление не сохранено: %+v", instance.Status)
	}
	condition := findCondition(instance.Status.Conditions, conditionTypeDataLoss)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "AllProcessesStopped" {
		t.Fatalf("потеря кэша не отражена: %+v", condition)
	}
}

func TestCT07EmptyRecoveryRejectsUnknownOrPreviouslySyncedProcess(t *testing.T) {
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.PasswordVersion = 1
	instance.Status.Initialized = true
	for ordinal := int32(0); ordinal < 3; ordinal++ {
		old := failoverNode(ordinal, valkeyv1alpha1.NodeRoleReplica, "history-a", 10, nil)
		old.Termination = &valkeyv1alpha1.ProcessTermination{Evidence: "container_status"}
		instance.Status.PreviousProcesses = append(instance.Status.PreviousProcesses, old)
		fresh := failoverNode(ordinal, valkeyv1alpha1.NodeRoleReplica, "", 0, nil)
		fresh.PodUID = fmt.Sprintf("fresh-pod-%d", ordinal)
		fresh.AppEnabled = false
		fresh.Replication.LinkUp = false
		instance.Status.Nodes = append(instance.Status.Nodes, fresh)
	}
	setPrimaryIdentity(&instance.Status, instance.Status.PreviousProcesses[0])
	instance.Status.PreviousProcesses[1].Termination = nil
	if emptyRecoverySafe(instance) {
		t.Fatal("неизвестный прежний процесс разрешил пустое восстановление")
	}
	instance.Status.PreviousProcesses[1].Termination = &valkeyv1alpha1.ProcessTermination{Evidence: "node_deleted"}
	syncedAt := metav1.Now()
	instance.Status.Nodes[2].Replication.SyncedAt = &syncedAt
	if emptyRecoverySafe(instance) {
		t.Fatal("ранее синхронизированный текущий процесс принят как пустой")
	}
	instance.Status.Nodes[2].Replication.SyncedAt = nil
	instance.Status.Nodes[2].Replication.Offset = emptyReplicaInitialOffset + 1
	if emptyRecoverySafe(instance) {
		t.Fatal("реплика с продвинувшимся offset принята как пустая")
	}
}

func TestCT07EmptyRecoveryWaitRequiresFencingAndFullTimeout(t *testing.T) {
	ctx := context.Background()
	startedAt := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	emptySince := metav1.NewTime(startedAt)
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.PasswordVersion = 1
	instance.Status.Initialized = true
	for ordinal := int32(0); ordinal < 3; ordinal++ {
		old := failoverNode(ordinal, valkeyv1alpha1.NodeRoleReplica, "history-a", 10, nil)
		old.Termination = &valkeyv1alpha1.ProcessTermination{Evidence: "container_status"}
		instance.Status.PreviousProcesses = append(instance.Status.PreviousProcesses, old)
		fresh := failoverNode(ordinal, valkeyv1alpha1.NodeRoleReplica, "", emptyReplicaInitialOffset, nil)
		fresh.PodUID = fmt.Sprintf("fresh-pod-%d", ordinal)
		fresh.ContainerID = fmt.Sprintf("fresh-container-%d", ordinal)
		fresh.RunID = fmt.Sprintf("fresh-run-%d", ordinal)
		fresh.AppEnabled = false
		fresh.Replication.LinkUp = false
		instance.Status.Nodes = append(instance.Status.Nodes, fresh)
	}
	source := &instance.Status.PreviousProcesses[0]
	source.Termination = nil
	setPrimaryIdentity(&instance.Status, *source)
	instance.Status.Failover = &valkeyv1alpha1.FailoverStatus{
		Reason: valkeyv1alpha1.FailoverReasonFailure, Stage: valkeyv1alpha1.FailoverStageChoosing,
		StartedAt: metav1.NewTime(startedAt), Source: processIdentity(*source), EmptySince: &emptySince,
	}
	if emptyRecoveryWaitingSafe(instance, instance.Status.Failover) {
		t.Fatal("пустое восстановление разрешено без fencing прежнего primary")
	}
	instance.Status.Failover.SourceAppDisabled = true
	instance.Status.Failover.SourceClientsKilled = true
	if !emptyRecoveryWaitingSafe(instance, instance.Status.Failover) {
		t.Fatal("изолированный прежний primary не разрешил начать безопасное ожидание")
	}

	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		Clock:  clocktesting.NewFakeClock(startedAt.Add(config.EmptyPrimaryTimeout - time.Nanosecond)),
	}
	result, err := reconciler.reconcileFailoverChoosing(ctx, instance)
	if err != nil || result.RequeueAfter == 0 || instance.Status.Failover == nil {
		t.Fatalf("пустое восстановление началось до срока: result=%+v status=%+v error=%v",
			result, instance.Status, err)
	}
	reconciler.Clock = clocktesting.NewFakeClock(startedAt.Add(config.EmptyPrimaryTimeout))
	result, err = reconciler.reconcileFailoverChoosing(ctx, instance)
	condition := findCondition(instance.Status.Conditions, conditionTypeDataLoss)
	if err != nil || result.IsZero() || instance.Status.Failover != nil ||
		instance.Status.PrimaryOrdinal != nil || condition == nil ||
		condition.Reason != "EmptyProcessesTimedOut" {
		t.Fatalf("пустое восстановление не началось после срока: result=%+v status=%+v error=%v",
			result, instance.Status, err)
	}
}

func failoverNode(
	ordinal int32,
	role valkeyv1alpha1.NodeRole,
	history string,
	offset int64,
	syncedAt *metav1.Time,
) valkeyv1alpha1.NodeStatus {
	return valkeyv1alpha1.NodeStatus{
		Ordinal: ordinal, PodUID: fmt.Sprintf("pod-%d", ordinal),
		ContainerID: "container", RunID: fmt.Sprintf("run-%d", ordinal),
		NodeName: fmt.Sprintf("worker-%d", ordinal), NodeUID: fmt.Sprintf("node-%d", ordinal),
		Role: role, Readiness: true, AppPasswordVersion: 1,
		Replication: &valkeyv1alpha1.ReplicationStatus{
			ReplicationID: history, Offset: offset, LinkUp: true, SyncedAt: syncedAt,
		},
	}
}
