package operator

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

const conditionTypeReplicasReady = "ReplicasReady"

func applyOperationalPhase(
	instance *valkeyv1alpha1.ValkeyInstance,
	status *valkeyv1alpha1.ValkeyInstanceStatus,
) {
	if !status.Initialized || status.AcceptedConfiguration == nil {
		return
	}
	if !primaryPubliclyReady(status) || hasUnterminatedPreviousProcess(status.PreviousProcesses) {
		status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
		status.Reason = "PRIMARY_NOT_READY"
		return
	}

	activeOperation := status.Failover != nil || status.Rollout != nil || status.CredentialRotation != nil
	if status.AcceptedConfiguration.Mode == valkeyv1alpha1.ValkeyModeHA {
		readyReplicas := readyReplicaCount(status)
		if readyReplicas < 2 {
			setCondition(
				instance,
				status,
				conditionTypeReplicasReady,
				metav1.ConditionFalse,
				"ReplicaSetIncomplete",
				"готовы не все реплики HA",
			)
		} else {
			setCondition(
				instance,
				status,
				conditionTypeReplicasReady,
				metav1.ConditionTrue,
				"ReplicasReady",
				"обе реплики HA готовы",
			)
		}
		if activeOperation {
			status.Phase = valkeyv1alpha1.InstancePhaseUpdating
			status.Reason = "CONFIGURATION_APPLYING"
			return
		}
		if readyReplicas < 2 {
			status.Phase = valkeyv1alpha1.InstancePhaseDegraded
			status.Reason = "REPLICAS_NOT_READY"
			confirmAppliedGeneration(status)
			return
		}
	}
	if activeOperation {
		status.Phase = valkeyv1alpha1.InstancePhaseUpdating
		status.Reason = "CONFIGURATION_APPLYING"
		return
	}

	status.Phase = valkeyv1alpha1.InstancePhaseRunning
	status.Reason = ""
	confirmAppliedGeneration(status)
}

func primaryPubliclyReady(status *valkeyv1alpha1.ValkeyInstanceStatus) bool {
	condition := findCondition(status.Conditions, conditionTypePublicReady)
	if condition == nil || condition.Status != metav1.ConditionTrue || status.PrimaryOrdinal == nil {
		return false
	}
	primary, found := nodeStatusAtOrdinal(status.Nodes, *status.PrimaryOrdinal)
	return found && primary.Role == valkeyv1alpha1.NodeRolePrimary && primary.Termination == nil &&
		primary.Observation == nil && primary.Readiness && primary.AppEnabled &&
		primary.AppPasswordVersion == status.AcceptedConfiguration.PasswordVersion
}

func readyReplicaCount(status *valkeyv1alpha1.ValkeyInstanceStatus) int {
	count := 0
	for _, node := range status.Nodes {
		if node.Role == valkeyv1alpha1.NodeRoleReplica && node.Termination == nil &&
			node.Observation == nil && node.Readiness && node.AppEnabled &&
			node.AppPasswordVersion == status.AcceptedConfiguration.PasswordVersion &&
			node.Replication != nil && node.Replication.LinkUp &&
			!node.Replication.SyncInProgress && node.Replication.SyncedAt != nil {
			count++
		}
	}
	return count
}

func confirmAppliedGeneration(status *valkeyv1alpha1.ValkeyInstanceStatus) {
	accepted := status.AcceptedConfiguration
	if status.Applied != nil && status.Applied.Mode == accepted.Mode &&
		status.Applied.VCPU == accepted.VCPU && status.Applied.RAMGB == accepted.RAMGB &&
		status.AppliedPasswordVersion == accepted.PasswordVersion {
		status.ObservedGeneration = accepted.DesiredGeneration
	}
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for index := range conditions {
		if conditions[index].Type == conditionType {
			return &conditions[index]
		}
	}
	return nil
}
