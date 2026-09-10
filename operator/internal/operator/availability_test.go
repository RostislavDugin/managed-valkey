package operator

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func Test_HA03_UpdateOperationalPhase_WhenReplicaAvailabilityChanges_ReportsRunningOrDegraded(t *testing.T) {
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.PasswordVersion = 1
	instance.Status.AcceptedConfiguration.DesiredGeneration = 2
	instance.Status.Applied = &valkeyv1alpha1.AppliedConfiguration{
		Mode: valkeyv1alpha1.ValkeyModeHA, VCPU: 2, RAMGB: 4,
	}
	instance.Status.AppliedPasswordVersion = 1
	instance.Status.Initialized = true
	instance.Status.PrimaryOrdinal = ordinalPointer(0)
	instance.Status.PrimaryPodUID = "pod-0"
	instance.Status.PrimaryContainerID = "containerd://0"
	instance.Status.PrimaryRunID = "run-0"
	instance.Status.PrimaryNodeName = "worker-0"
	instance.Status.PrimaryNodeUID = "node-0"
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{readyPrimaryNode(0)}
	setCondition(
		instance,
		&instance.Status,
		conditionTypePublicReady,
		metav1.ConditionTrue,
		"Admitted",
		"primary готов",
	)

	applyOperationalPhase(instance, &instance.Status)
	if instance.Status.Phase != valkeyv1alpha1.InstancePhaseDegraded ||
		instance.Status.Reason != "REPLICAS_NOT_READY" ||
		instance.Status.ObservedGeneration != instance.Status.AcceptedConfiguration.DesiredGeneration {
		t.Fatalf("HA без реплик не перешёл в degraded: %+v", instance.Status)
	}

	instance.Status.Nodes = append(
		instance.Status.Nodes,
		readyReplicaNode(1),
		readyReplicaNode(2),
	)
	applyOperationalPhase(instance, &instance.Status)
	if instance.Status.Phase != valkeyv1alpha1.InstancePhaseRunning || instance.Status.Reason != "" {
		t.Fatalf("полный HA не перешёл в running: %+v", instance.Status)
	}

	instance.Status.CredentialRotation = &valkeyv1alpha1.CredentialRotationStatus{
		TargetVersion: 2, PreviousVersion: 1,
		Stage: valkeyv1alpha1.CredentialRotationStagePreparing,
	}
	instance.Status.Nodes = instance.Status.Nodes[:2]
	applyOperationalPhase(instance, &instance.Status)
	if instance.Status.Phase != valkeyv1alpha1.InstancePhaseUpdating {
		t.Fatalf("неполный HA во время операции не перешёл в updating: %+v", instance.Status)
	}
	condition := findCondition(instance.Status.Conditions, conditionTypeReplicasReady)
	if condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatalf("неполный состав не отражён condition: %+v", condition)
	}
}

func readyPrimaryNode(ordinal int32) valkeyv1alpha1.NodeStatus {
	return valkeyv1alpha1.NodeStatus{
		Ordinal: ordinal, PodUID: "pod-0", ContainerID: "containerd://0", RunID: "run-0",
		NodeName: "worker-0", NodeUID: "node-0", Role: valkeyv1alpha1.NodeRolePrimary,
		Readiness: true, AppEnabled: true, AppPasswordVersion: 1,
	}
}

func readyReplicaNode(ordinal int32) valkeyv1alpha1.NodeStatus {
	now := metav1.NewTime(time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC))
	return valkeyv1alpha1.NodeStatus{
		Ordinal: ordinal, PodUID: "pod-replica", ContainerID: "containerd://replica",
		RunID: "run-replica", NodeName: "worker-replica", NodeUID: "node-replica",
		Role: valkeyv1alpha1.NodeRoleReplica, Readiness: true, AppEnabled: true,
		AppPasswordVersion: 1,
		Replication: &valkeyv1alpha1.ReplicationStatus{
			LinkUp: true, SyncedAt: &now, ObservedAt: now,
		},
	}
}

func ordinalPointer(value int32) *int32 {
	return &value
}
