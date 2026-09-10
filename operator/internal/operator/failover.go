package operator

import (
	"context"
	"fmt"
	"net"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

const conditionTypeFailover = "Failover"

const emptyReplicaInitialOffset int64 = 1

func (r *ValkeyInstanceReconciler) reconcileFailover(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	if fullStopRolloutOwnsAvailability(instance) {
		if instance.Status.Failover == nil {
			return ctrl.Result{}, nil
		}
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			status.Failover = nil
			status.Phase = valkeyv1alpha1.InstancePhaseUpdating
			status.Reason = "ROLLOUT_" + string(status.Rollout.Stage)
			apimeta.RemoveStatusCondition(&status.Conditions, conditionTypeFailover)
		})
		return requeueIf(changed), err
	}
	accepted := instance.Status.AcceptedConfiguration
	if accepted == nil || accepted.Mode != valkeyv1alpha1.ValkeyModeHA || !instance.Status.Initialized {
		return ctrl.Result{}, nil
	}
	if instance.Status.Failover == nil {
		return r.maybeStartFailureFailover(ctx, instance)
	}

	switch instance.Status.Failover.Stage {
	case valkeyv1alpha1.FailoverStageFencing:
		return r.reconcileFailoverFencing(ctx, instance)
	case valkeyv1alpha1.FailoverStageChoosing:
		return r.reconcileFailoverChoosing(ctx, instance)
	case valkeyv1alpha1.FailoverStagePromoting:
		return r.reconcileFailoverPromoting(ctx, instance)
	case valkeyv1alpha1.FailoverStageReconfiguring:
		return r.reconcileFailoverReconfiguring(ctx, instance)
	default:
		return ctrl.Result{}, fmt.Errorf("неизвестная стадия failover %q", instance.Status.Failover.Stage)
	}
}

func fullStopRolloutOwnsAvailability(instance *valkeyv1alpha1.ValkeyInstance) bool {
	if !rolloutRequiresFullStop(instance) || instance.Status.Rollout == nil {
		return false
	}

	return instance.Status.Rollout.Stage == valkeyv1alpha1.RolloutStageStopping ||
		instance.Status.Rollout.Stage == valkeyv1alpha1.RolloutStageUpdatingTemplate
}

func (r *ValkeyInstanceReconciler) maybeStartFailureFailover(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	if instance.Status.PrimaryOrdinal == nil {
		if emptyPrimaryAssignmentAllowed(instance) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}
	primary, found := nodeStatusAtOrdinal(instance.Status.Nodes, *instance.Status.PrimaryOrdinal)
	if !found || !primaryIdentityMatches(instance, primary) {
		if emptyRecoverySafe(instance) {
			return r.beginEmptyRecovery(ctx, instance, false)
		}
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}
	failed := primary.Termination != nil
	if !failed {
		failed = processThresholds(primary.Observation, nil, r.now()).TransportFailure
	}
	if !failed {
		return ctrl.Result{}, nil
	}

	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		status.Failover = &valkeyv1alpha1.FailoverStatus{
			Reason:    valkeyv1alpha1.FailoverReasonFailure,
			Stage:     valkeyv1alpha1.FailoverStageFencing,
			StartedAt: metav1.NewTime(r.now()),
			Source:    processIdentity(primary),
		}
		status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
		status.Reason = "FENCING_REQUIRED"
		setCondition(
			instance,
			status,
			conditionTypeFailover,
			metav1.ConditionFalse,
			"Fencing",
			"ожидается подтверждение fencing прежнего primary",
		)
	})

	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) reconcileFailoverFencing(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	operation := instance.Status.Failover
	source, found := knownProcess(instance.Status, operation.Source)
	if !found {
		return r.failoverWait(ctx, instance, "SourceIdentityUnknown", "идентичность прежнего primary потеряна")
	}
	if source.Termination == nil {
		changed, err := r.removeProcessRoleLabel(ctx, instance, source)
		if err != nil || changed {
			return requeueIf(changed), err
		}
		if !operation.SourceAppDisabled || !operation.SourceClientsKilled {
			fenced, err := r.fenceProcess(ctx, instance, source)
			if err != nil {
				if operatorvalkey.ClassifyError(err) == operatorvalkey.ErrorKindTransport {
					return r.failoverWait(
						ctx,
						instance,
						"FencingSource",
						"прежний primary недоступен без доказательства остановки",
					)
				}
				return ctrl.Result{}, err
			}
			if !fenced {
				return r.failoverWait(ctx, instance, "FencingSource", "роль или ACL прежнего primary не подтверждены")
			}
			changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
				if status.Failover != nil && sameIdentity(status.Failover.Source, operation.Source) {
					status.Failover.SourceAppDisabled = true
					status.Failover.SourceClientsKilled = true
				}
			})
			return requeueIf(changed), err
		}
	}

	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		if status.Failover != nil && sameIdentity(status.Failover.Source, operation.Source) {
			status.Failover.Stage = valkeyv1alpha1.FailoverStageChoosing
			status.Reason = "FAILOVER"
		}
	})

	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) reconcileFailoverChoosing(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	operation := instance.Status.Failover
	if operation.Candidate == nil {
		source, found := knownProcess(instance.Status, operation.Source)
		if !found {
			return r.failoverWait(ctx, instance, "SourceIdentityUnknown", "идентичность прежнего primary потеряна")
		}
		candidate, found := chooseFailoverCandidate(source, instance.Status.Nodes)
		if !found {
			if emptyRecoverySafe(instance) {
				return r.beginEmptyRecovery(ctx, instance, false)
			}
			return r.reconcileEmptyRecoveryWait(ctx, instance)
		}
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			if status.Failover == nil || status.Failover.Candidate != nil {
				return
			}
			identity := processIdentity(candidate)
			status.Failover.Candidate = &identity
			status.Failover.EmptySince = nil
			if source.Replication != nil {
				status.Failover.ReplicationID = source.Replication.ReplicationID
				status.Failover.ControlOffset = source.Replication.Offset
			}
			deadline := metav1.NewTime(r.now().Add(config.FailoverTimeout))
			status.Failover.OffsetDeadline = &deadline
		})
		return requeueIf(changed), err
	}
	if operation.OffsetDeadline == nil {
		source, found := knownProcess(instance.Status, operation.Source)
		if !found {
			return r.failoverWait(ctx, instance, "SourceIdentityUnknown", "идентичность прежнего primary потеряна")
		}
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			if status.Failover == nil || status.Failover.Candidate == nil {
				return
			}
			if source.Replication != nil {
				status.Failover.ReplicationID = source.Replication.ReplicationID
				status.Failover.ControlOffset = source.Replication.Offset
			}
			deadline := metav1.NewTime(r.now().Add(config.FailoverTimeout))
			status.Failover.OffsetDeadline = &deadline
		})
		return requeueIf(changed), err
	}

	candidate, found := knownProcess(instance.Status, *operation.Candidate)
	if !found {
		return r.failoverWait(ctx, instance, "CandidateIdentityUnknown", "идентичность кандидата потеряна")
	}
	if candidate.Termination != nil {
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			if status.Failover != nil && status.Failover.Candidate != nil &&
				sameIdentity(*status.Failover.Candidate, *operation.Candidate) {
				status.Failover.Candidate = nil
				status.Failover.CandidateAppDisabled = false
				status.Failover.CandidateClientsKilled = false
				status.Failover.CandidateMayBePrimary = false
				status.Failover.OffsetDeadline = nil
			}
		})
		return requeueIf(changed), err
	}
	if !operation.CandidateAppDisabled || !operation.CandidateClientsKilled {
		fenced, err := r.fenceProcess(ctx, instance, candidate)
		if err != nil {
			if operatorvalkey.ClassifyError(err) == operatorvalkey.ErrorKindTransport {
				return r.failoverWait(ctx, instance, "FencingCandidate", "кандидат недоступен до продвижения")
			}
			return ctrl.Result{}, err
		}
		if !fenced {
			return r.failoverWait(ctx, instance, "FencingCandidate", "роль или ACL кандидата не подтверждены")
		}
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			if status.Failover != nil && status.Failover.Candidate != nil &&
				sameIdentity(*status.Failover.Candidate, *operation.Candidate) {
				status.Failover.CandidateAppDisabled = true
				status.Failover.CandidateClientsKilled = true
			}
		})
		return requeueIf(changed), err
	}

	offsetReady := candidate.Replication != nil && operation.ReplicationID != "" &&
		candidate.Replication.ReplicationID == operation.ReplicationID &&
		candidate.Replication.Offset >= operation.ControlOffset
	deadlineReached := operation.OffsetDeadline != nil && !r.now().Before(operation.OffsetDeadline.Time)
	if !offsetReady && !deadlineReached {
		return r.failoverWait(ctx, instance, "WaitingForOffset", "кандидат догоняет контрольный offset")
	}
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		if status.Failover != nil {
			status.Failover.Stage = valkeyv1alpha1.FailoverStagePromoting
			status.Reason = "FAILOVER"
		}
	})

	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) reconcileEmptyRecoveryWait(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	operation := instance.Status.Failover
	if operation == nil || !emptyRecoveryWaitingSafe(instance, operation) {
		if operation != nil && operation.EmptySince != nil {
			changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
				if status.Failover != nil {
					status.Failover.EmptySince = nil
				}
			})
			return requeueIf(changed), err
		}
		return r.failoverWait(ctx, instance, "CandidateNotReady", "нет синхронизированной реплики для failover")
	}
	if operation.EmptySince == nil {
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			if status.Failover != nil && status.Failover.Candidate == nil &&
				emptyRecoveryWaitingSafe(instance, status.Failover) {
				now := metav1.NewTime(r.now())
				status.Failover.EmptySince = &now
			}
		})
		return requeueIf(changed), err
	}
	if processThresholds(nil, operation.EmptySince, r.now()).EmptyPrimary {
		return r.beginEmptyRecovery(ctx, instance, true)
	}
	return r.failoverWait(ctx, instance, "EmptyPrimaryWaiting", "проверенные пустые процессы ожидают безопасный срок")
}

func emptyRecoverySafe(instance *valkeyv1alpha1.ValkeyInstance) bool {
	status := &instance.Status
	if !status.Initialized || status.AcceptedConfiguration == nil ||
		status.AcceptedConfiguration.Mode != valkeyv1alpha1.ValkeyModeHA ||
		status.PrimaryOrdinal == nil || len(status.Nodes) != int(expectedProcessCount(instance)) ||
		len(status.PreviousProcesses) < int(expectedProcessCount(instance)) ||
		hasUnterminatedPreviousProcess(status.PreviousProcesses) {
		return false
	}
	primary := valkeyv1alpha1.ProcessIdentity{
		PodUID: status.PrimaryPodUID, ContainerID: status.PrimaryContainerID,
		RunID: status.PrimaryRunID, NodeName: status.PrimaryNodeName, NodeUID: status.PrimaryNodeUID,
	}
	knownPrimary, found := knownProcess(*status, primary)
	if !found || knownPrimary.Termination == nil {
		return false
	}
	for _, current := range status.Nodes {
		if current.Termination != nil || current.Observation != nil ||
			current.Role != valkeyv1alpha1.NodeRoleReplica || current.AppEnabled ||
			current.AppPasswordVersion != status.AcceptedConfiguration.PasswordVersion ||
			current.Replication == nil || current.Replication.SyncedAt != nil ||
			current.Replication.Offset > emptyReplicaInitialOffset {
			return false
		}
		replacedStoppedProcess := slices.ContainsFunc(
			status.PreviousProcesses,
			func(previous valkeyv1alpha1.NodeStatus) bool {
				return previous.Ordinal == current.Ordinal && previous.Termination != nil &&
					!sameProcess(previous, current)
			},
		)
		if !replacedStoppedProcess {
			return false
		}
	}
	return true
}

func emptyRecoveryWaitingSafe(
	instance *valkeyv1alpha1.ValkeyInstance,
	operation *valkeyv1alpha1.FailoverStatus,
) bool {
	status := &instance.Status
	if operation == nil || operation.Reason != valkeyv1alpha1.FailoverReasonFailure ||
		!status.Initialized || status.AcceptedConfiguration == nil ||
		status.AcceptedConfiguration.Mode != valkeyv1alpha1.ValkeyModeHA || len(status.Nodes) == 0 {
		return false
	}
	for _, process := range status.Nodes {
		if process.Termination != nil {
			continue
		}
		if process.Observation != nil || process.Role != valkeyv1alpha1.NodeRoleReplica || process.AppEnabled ||
			process.AppPasswordVersion != status.AcceptedConfiguration.PasswordVersion ||
			process.Replication == nil || process.Replication.SyncedAt != nil ||
			process.Replication.Offset > emptyReplicaInitialOffset {
			return false
		}
	}
	for _, previous := range status.PreviousProcesses {
		if previous.Termination != nil {
			continue
		}
		identity := processIdentity(previous)
		if sameIdentity(identity, operation.Source) &&
			operation.SourceAppDisabled && operation.SourceClientsKilled {
			continue
		}
		if operation.Candidate != nil && sameIdentity(identity, *operation.Candidate) &&
			operation.CandidateAppDisabled && operation.CandidateClientsKilled {
			continue
		}
		return false
	}
	return true
}

func (r *ValkeyInstanceReconciler) beginEmptyRecovery(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	timedOut bool,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		operation := status.Failover
		if timedOut {
			if operation == nil || !emptyRecoveryWaitingSafe(instance, operation) ||
				!processThresholds(nil, operation.EmptySince, r.now()).EmptyPrimary {
				return
			}
		} else if !emptyRecoverySafe(instance) {
			return
		}
		status.Failover = nil
		status.PrimaryOrdinal = nil
		status.PrimaryPodUID = ""
		status.PrimaryContainerID = ""
		status.PrimaryRunID = ""
		status.PrimaryNodeName = ""
		status.PrimaryNodeUID = ""
		status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
		status.Reason = "EMPTY_RECOVERY"
		reason := "AllProcessesStopped"
		message := "все прежние процессы остановлены, запускается пустой кэш"
		if timedOut {
			reason = "EmptyProcessesTimedOut"
			message = "прежние primary изолированы, проверенные пустые процессы запускаются после ожидания"
		}
		setCondition(
			instance,
			status,
			conditionTypeDataLoss,
			metav1.ConditionTrue,
			reason,
			message,
		)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		reason := "all_processes_stopped"
		message := "все прежние процессы остановлены, запускается пустой кэш"
		if timedOut {
			reason = "empty_processes_timed_out"
			message = "прежние primary изолированы, проверенные пустые процессы запускаются после ожидания"
		}
		logf.FromContext(ctx).Info("запускается пустое восстановление", "reason", reason)
		if r.Recorder != nil {
			r.Recorder.Eventf(
				instance,
				nil,
				corev1.EventTypeWarning,
				"CacheEmptied",
				"Failover",
				message,
			)
		}
	}
	return requeueIf(changed), nil
}

func (r *ValkeyInstanceReconciler) reconcileFailoverPromoting(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	operation := instance.Status.Failover
	if operation.Candidate == nil {
		return ctrl.Result{}, fmt.Errorf("кандидат failover не сохранён")
	}
	candidate, found := knownProcess(instance.Status, *operation.Candidate)
	if !found {
		return r.failoverWait(ctx, instance, "CandidateIdentityUnknown", "идентичность кандидата потеряна")
	}
	if candidate.Termination != nil {
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			if status.Failover != nil {
				status.Failover.Stage = valkeyv1alpha1.FailoverStageChoosing
				status.Failover.Candidate = nil
				status.Failover.CandidateAppDisabled = false
				status.Failover.CandidateClientsKilled = false
				status.Failover.CandidateMayBePrimary = false
				status.Failover.OffsetDeadline = nil
			}
		})
		return requeueIf(changed), err
	}
	if candidate.Role == valkeyv1alpha1.NodeRolePrimary {
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			if status.Failover == nil || status.Failover.Candidate == nil ||
				!sameIdentity(*status.Failover.Candidate, *operation.Candidate) {
				return
			}
			setPrimaryIdentity(status, candidate)
			status.Failover.Stage = valkeyv1alpha1.FailoverStageReconfiguring
			status.Reason = "FAILOVER"
		})
		return requeueIf(changed), err
	}
	if candidate.Role != valkeyv1alpha1.NodeRoleReplica || candidate.AppEnabled ||
		candidate.AppPasswordVersion != instance.Status.AcceptedConfiguration.PasswordVersion {
		return r.failoverWait(ctx, instance, "CandidateNotFenced", "кандидат не готов к продвижению")
	}
	if !operation.CandidateMayBePrimary {
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			if status.Failover != nil {
				status.Failover.CandidateMayBePrimary = true
			}
		})
		return requeueIf(changed), err
	}

	pod, err := r.currentProcessPod(ctx, instance, candidate)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.failoverWait(
				ctx,
				instance,
				"CandidateUnavailable",
				"кандидат после возможного продвижения недоступен",
			)
		}
		return ctrl.Result{}, err
	}
	credentials, _, err := r.processCredentials(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	promote := r.PromoteProcess
	if promote == nil {
		promote = r.promoteProcess
	}
	state, err := promote(
		ctx,
		net.JoinHostPort(pod.Status.PodIP, "6379"),
		string(credentials.OperatorPassword),
		candidate.RunID,
	)
	if err != nil {
		if operatorvalkey.ClassifyError(err) == operatorvalkey.ErrorKindTransport {
			return r.failoverWait(ctx, instance, "PromotionResultUnknown", "результат продвижения кандидата неизвестен")
		}
		return ctrl.Result{}, err
	}
	if state.RunID != candidate.RunID || state.AppEnabled {
		return r.failoverWait(ctx, instance, "CandidateIdentityChanged", "кандидат изменился перед продвижением")
	}

	return requeueIf(true), nil
}

func (r *ValkeyInstanceReconciler) reconcileFailoverReconfiguring(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	primary, found := failoverPrimaryProcess(instance)
	if !found {
		return r.failoverWait(
			ctx,
			instance,
			"PrimaryIdentityUnknown",
			"идентичность нового primary потеряна во время переподключения реплик",
		)
	}
	failed := primary.Termination != nil ||
		processThresholds(primary.Observation, nil, r.now()).TransportFailure
	if failed {
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			if status.Failover == nil || status.Failover.Stage != valkeyv1alpha1.FailoverStageReconfiguring {
				return
			}
			status.Failover.Reason = valkeyv1alpha1.FailoverReasonFailure
			status.Failover.Stage = valkeyv1alpha1.FailoverStageFencing
			status.Failover.Source = processIdentity(primary)
			status.Failover.Candidate = nil
			status.Failover.SourceAppDisabled = false
			status.Failover.SourceClientsKilled = false
			status.Failover.CandidateAppDisabled = false
			status.Failover.CandidateClientsKilled = false
			status.Failover.CandidateMayBePrimary = false
			status.Failover.ReplicationID = ""
			status.Failover.ControlOffset = 0
			status.Failover.OffsetDeadline = nil
			status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
			status.Reason = "FENCING_REQUIRED"
			setCondition(
				instance,
				status,
				conditionTypeFailover,
				metav1.ConditionFalse,
				"Fencing",
				"новый primary отказал до завершения переподключения реплик",
			)
		})
		return requeueIf(changed), err
	}

	ready, err := r.failoverReconfigurationReady(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return ctrl.Result{}, nil
	}
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		status.Failover = nil
		status.Reason = ""
		setCondition(
			instance,
			status,
			conditionTypeFailover,
			metav1.ConditionTrue,
			"Completed",
			"переключение primary завершено",
		)
		applyOperationalPhase(instance, status)
	})

	return requeueIf(changed), err
}

func failoverPrimaryProcess(instance *valkeyv1alpha1.ValkeyInstance) (valkeyv1alpha1.NodeStatus, bool) {
	status := &instance.Status
	if status.PrimaryOrdinal == nil {
		return valkeyv1alpha1.NodeStatus{}, false
	}
	identity := valkeyv1alpha1.ProcessIdentity{
		PodUID: status.PrimaryPodUID, ContainerID: status.PrimaryContainerID,
		RunID: status.PrimaryRunID, NodeName: status.PrimaryNodeName, NodeUID: status.PrimaryNodeUID,
	}
	return knownProcess(*status, identity)
}

func (r *ValkeyInstanceReconciler) failoverReconfigurationReady(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
	status := &instance.Status
	if status.PrimaryOrdinal == nil || !primaryPubliclyReady(status) {
		return false, nil
	}
	primary, found := nodeStatusAtOrdinal(status.Nodes, *status.PrimaryOrdinal)
	if !found || !primaryIdentityMatches(instance, primary) {
		return false, nil
	}
	primaryPod, err := r.currentProcessPod(ctx, instance, primary)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, node := range status.Nodes {
		if node.Ordinal == primary.Ordinal || node.Termination != nil || node.Observation != nil {
			continue
		}
		if node.Role != valkeyv1alpha1.NodeRoleReplica || node.Replication == nil ||
			!node.Replication.LinkUp || node.Replication.SyncInProgress ||
			node.Replication.UpstreamHost != primaryPod.Status.PodIP ||
			node.Replication.UpstreamPort != valkeyPort ||
			node.AppPasswordVersion != status.AcceptedConfiguration.PasswordVersion {
			return false, nil
		}
	}
	return true, nil
}

func chooseFailoverCandidate(
	source valkeyv1alpha1.NodeStatus,
	nodes []valkeyv1alpha1.NodeStatus,
) (valkeyv1alpha1.NodeStatus, bool) {
	if source.Replication == nil || source.Replication.ReplicationID == "" {
		return valkeyv1alpha1.NodeStatus{}, false
	}
	candidates := slices.DeleteFunc(slices.Clone(nodes), func(node valkeyv1alpha1.NodeStatus) bool {
		return node.Ordinal == source.Ordinal || node.Role != valkeyv1alpha1.NodeRoleReplica ||
			node.Termination != nil || node.Observation != nil ||
			node.Replication == nil || node.Replication.SyncedAt == nil ||
			node.Replication.SyncInProgress ||
			node.Replication.ReplicationID != source.Replication.ReplicationID
	})
	if len(candidates) == 0 {
		return valkeyv1alpha1.NodeStatus{}, false
	}
	slices.SortFunc(candidates, func(left, right valkeyv1alpha1.NodeStatus) int {
		if left.Replication.Offset != right.Replication.Offset {
			if left.Replication.Offset > right.Replication.Offset {
				return -1
			}
			return 1
		}
		return int(left.Ordinal - right.Ordinal)
	})
	return candidates[0], true
}

func (r *ValkeyInstanceReconciler) fenceProcess(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) (bool, error) {
	pod, err := r.currentProcessPod(ctx, instance, process)
	if err != nil {
		return false, err
	}
	credentials, appHash, err := r.processCredentials(ctx, instance)
	if err != nil {
		return false, err
	}
	update := r.UpdateAppAccess
	if update == nil {
		update = r.updateAppAccess
	}
	state, err := update(
		ctx,
		net.JoinHostPort(pod.Status.PodIP, "6379"),
		string(credentials.OperatorPassword),
		appHash,
		false,
	)
	if err != nil {
		return false, err
	}
	return state.RunID == process.RunID && !state.AppEnabled &&
		appACLMatches(state.AppPasswordHashes, appHash), nil
}

func (r *ValkeyInstanceReconciler) currentProcessPod(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) (*corev1.Pod, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	pod := &corev1.Pod{}
	key := client.ObjectKey{
		Namespace: instance.Namespace,
		Name:      fmt.Sprintf("%s-%d", instance.Status.AcceptedConfiguration.Slug, process.Ordinal),
	}
	if err := reader.Get(ctx, key, pod); err != nil {
		return nil, err
	}
	container := valkeyContainerStatus(pod.Status.ContainerStatuses)
	if pod.Status.PodIP == "" || container == nil || container.State.Running == nil ||
		process.PodUID != string(pod.UID) || process.ContainerID != container.ContainerID ||
		process.NodeName != pod.Spec.NodeName {
		return nil, apierrors.NewNotFound(corev1.Resource("pods"), pod.Name)
	}
	node := &corev1.Node{}
	if err := reader.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
		return nil, err
	}
	if process.NodeUID != string(node.UID) {
		return nil, apierrors.NewNotFound(corev1.Resource("nodes"), node.Name)
	}

	return pod, nil
}

func (r *ValkeyInstanceReconciler) removeProcessRoleLabel(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) (bool, error) {
	pod, err := r.currentProcessPod(ctx, instance, process)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if _, found := pod.Labels[applicationRoleLabel]; !found {
		return false, nil
	}
	before := pod.DeepCopy()
	pod.Labels = cloneWithoutKey(pod.Labels, applicationRoleLabel)
	if err := r.Patch(ctx, pod, client.MergeFrom(before)); err != nil {
		return false, fmt.Errorf("закрыть Service процесса перед failover: %w", err)
	}
	return true, nil
}

func (r *ValkeyInstanceReconciler) failoverWait(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	reason string,
	message string,
) (ctrl.Result, error) {
	_, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
		status.Reason = "FAILOVER"
		if status.Failover != nil && status.Failover.Stage == valkeyv1alpha1.FailoverStageFencing {
			status.Reason = "FENCING_REQUIRED"
		}
		setCondition(instance, status, conditionTypeFailover, metav1.ConditionFalse, reason, message)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
}

func processIdentity(process valkeyv1alpha1.NodeStatus) valkeyv1alpha1.ProcessIdentity {
	return valkeyv1alpha1.ProcessIdentity{
		PodUID: process.PodUID, ContainerID: process.ContainerID, RunID: process.RunID,
		NodeName: process.NodeName, NodeUID: process.NodeUID,
	}
}

func sameIdentity(left, right valkeyv1alpha1.ProcessIdentity) bool {
	return left == right
}

func knownProcess(
	status valkeyv1alpha1.ValkeyInstanceStatus,
	identity valkeyv1alpha1.ProcessIdentity,
) (valkeyv1alpha1.NodeStatus, bool) {
	for _, process := range append(slices.Clone(status.Nodes), status.PreviousProcesses...) {
		if sameIdentity(processIdentity(process), identity) {
			return process, true
		}
	}
	return valkeyv1alpha1.NodeStatus{}, false
}

func setPrimaryIdentity(status *valkeyv1alpha1.ValkeyInstanceStatus, process valkeyv1alpha1.NodeStatus) {
	ordinal := process.Ordinal
	status.PrimaryOrdinal = &ordinal
	status.PrimaryPodUID = process.PodUID
	status.PrimaryContainerID = process.ContainerID
	status.PrimaryRunID = process.RunID
	status.PrimaryNodeName = process.NodeName
	status.PrimaryNodeUID = process.NodeUID
}
