package operator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

const conditionTypeProcessReady = "ProcessReady"

var ErrServiceCredentialsMissing = errors.New("служебные учётные данные отсутствуют")

type ProcessInspector func(
	context.Context,
	string,
	string,
	string,
	bool,
) (operatorvalkey.ProcessState, error)

func (r *ValkeyInstanceReconciler) reconcileProcess(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	type processResult struct {
		result ctrl.Result
		err    error
	}
	var combined ctrl.Result
	var combinedErr error
	processCount := expectedProcessCount(instance)
	if processCount == 1 {
		combined, combinedErr = r.reconcileProcessOrdinal(ctx, instance, 0)
	} else {
		results := make(chan processResult, processCount)
		for ordinal := range processCount {
			processInstance := instance.DeepCopy()
			go func() {
				result, err := r.reconcileProcessOrdinal(ctx, processInstance, ordinal)
				results <- processResult{result: result, err: err}
			}()
		}
		for range processCount {
			result := <-results
			combined = earlierRequeue(combined, result.result)
			combinedErr = errors.Join(combinedErr, result.err)
		}
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	current := &valkeyv1alpha1.ValkeyInstance{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(instance), current); err != nil {
		return ctrl.Result{}, errors.Join(combinedErr, fmt.Errorf("перечитать status процессов: %w", err))
	}
	*instance = *current
	_, conditionErr := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		ready := int32(0)
		for ordinal := range processCount {
			process, found := nodeStatusAtOrdinal(status.Nodes, ordinal)
			if found && process.PodUID != "" && process.ContainerID != "" && process.RunID != "" &&
				process.Readiness && process.Observation == nil && process.Termination == nil {
				ready++
			}
		}
		conditionStatus := metav1.ConditionFalse
		reason := "ProcessesNotReady"
		message := fmt.Sprintf("готовы процессы Valkey: %d из %d", ready, processCount)
		if ready == processCount {
			conditionStatus = metav1.ConditionTrue
			reason = "Observed"
			message = "все процессы Valkey наблюдаются"
		}
		setCondition(instance, status, conditionTypeProcessReady, conditionStatus, reason, message)
	})
	combinedErr = errors.Join(combinedErr, conditionErr)

	return combined, combinedErr
}

func (r *ValkeyInstanceReconciler) reconcileProcessOrdinal(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	ordinal int32,
) (ctrl.Result, error) {
	pod := &corev1.Pod{}
	podKey := client.ObjectKey{
		Namespace: instance.Namespace,
		Name:      fmt.Sprintf("%s-%d", instance.Status.AcceptedConfiguration.Slug, ordinal),
	}
	if err := r.Get(ctx, podKey, pod); err != nil {
		if apierrors.IsNotFound(err) {
			current, found := nodeStatusAtOrdinal(instance.Status.Nodes, ordinal)
			if found && current.Termination == nil {
				deleted, checkErr := r.previousNodeDeleted(ctx, current)
				if checkErr != nil {
					return ctrl.Result{}, checkErr
				}
				if deleted {
					return r.saveNodeDeletionTermination(ctx, instance, current)
				}
			}
			return r.processUnavailable(ctx, instance, ordinal)
		}

		return ctrl.Result{}, fmt.Errorf("прочитать Pod Valkey: %w", err)
	}
	if !slices.Contains(pod.Finalizers, processFinalizer) && pod.DeletionTimestamp.IsZero() {
		before := pod.DeepCopy()
		pod.Finalizers = append(pod.Finalizers, processFinalizer)
		if err := r.Patch(ctx, pod, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, fmt.Errorf("добавить finalizer Pod: %w", err)
		}

		return requeueIf(true), nil
	}

	containerStatus := valkeyContainerStatus(pod.Status.ContainerStatuses)
	if containerStatus != nil && containerStatus.State.Terminated != nil {
		return r.saveContainerTermination(ctx, instance, pod, containerStatus, ordinal)
	}
	current, currentFound := nodeStatusAtOrdinal(instance.Status.Nodes, ordinal)
	if currentFound && current.Termination != nil && current.PodUID == string(pod.UID) &&
		containerStatus != nil && current.ContainerID == containerStatus.ContainerID {
		return r.releaseTerminatedPod(ctx, instance, pod, current)
	}
	if pod.Status.PodIP == "" || pod.Spec.NodeName == "" || containerStatus == nil ||
		containerStatus.ContainerID == "" || containerStatus.State.Running == nil {
		return r.processUnavailable(ctx, instance, ordinal)
	}

	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
		if apierrors.IsNotFound(err) && currentFound && current.Termination == nil &&
			current.PodUID == string(pod.UID) && current.ContainerID == containerStatus.ContainerID &&
			current.NodeName == pod.Spec.NodeName && current.NodeUID != "" {
			return r.saveNodeDeletionTermination(ctx, instance, current)
		}
		return ctrl.Result{}, fmt.Errorf("прочитать Node процесса Valkey: %w", err)
	}
	if currentFound && current.Termination == nil && current.PodUID == string(pod.UID) &&
		current.ContainerID == containerStatus.ContainerID && current.NodeName == node.Name &&
		current.NodeUID != "" && current.NodeUID != string(node.UID) {
		return r.saveNodeDeletionTermination(ctx, instance, current)
	}

	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{
		Namespace: instance.Namespace,
		Name:      valkeyv1alpha1.AuthSecretName(instance.Spec.Slug),
	}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("прочитать Secret процесса Valkey: %w", err)
	}
	credentials, err := valkeyv1alpha1.ParseServiceCredentials(secret.Data)
	if err != nil {
		return ctrl.Result{}, err
	}
	if credentials == nil {
		return ctrl.Result{}, ErrServiceCredentialsMissing
	}
	appHash, err := valkeyv1alpha1.ParseAppPasswordHash(
		secret.Data,
		instance.Status.AcceptedConfiguration.PasswordVersion,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	inspect := r.InspectProcess
	if inspect == nil {
		inspect = r.inspectProcess
	}
	takeControl := processNeedsControl(current, currentFound, r.ObservationStartedAt)
	state, err := inspect(
		ctx,
		net.JoinHostPort(pod.Status.PodIP, "6379"),
		string(credentials.OperatorPassword),
		appHash,
		takeControl,
	)
	if err != nil {
		return r.recordProcessObservationError(ctx, instance, pod, node, containerStatus, ordinal, err)
	}

	observed := valkeyv1alpha1.NodeStatus{
		Ordinal:     ordinal,
		PodUID:      string(pod.UID),
		ContainerID: containerStatus.ContainerID,
		RunID:       state.RunID,
		NodeName:    node.Name,
		NodeUID:     string(node.UID),
		Role:        valkeyv1alpha1.NodeRole(state.Role),
		Readiness:   podReady(pod.Status.Conditions),
		Replication: observedReplication(state.Replication, r.now()),
	}
	observed.AppEnabled = state.AppEnabled
	if appACLMatches(state.AppPasswordHashes, appHash) {
		observed.AppPasswordVersion = instance.Status.AcceptedConfiguration.PasswordVersion
	}
	current, hasCurrent := nodeStatusAtOrdinal(instance.Status.Nodes, observed.Ordinal)
	if hasCurrent {
		observed.Recovery = retainedProcessRecovery(current, observed, pod.DeletionTimestamp)
	}
	observed.Replication = mergeObservedReplication(current.Replication, observed.Replication, observed.Role, r.now())
	identityChanged := hasCurrent && !sameProcess(current, observed)
	var priorTermination *valkeyv1alpha1.ProcessTermination
	if identityChanged && current.Termination == nil {
		deleted, checkErr := r.previousNodeDeleted(ctx, current)
		if checkErr != nil {
			return ctrl.Result{}, checkErr
		}
		if deleted {
			priorTermination = &valkeyv1alpha1.ProcessTermination{
				Reason:     "NodeDeleted",
				FinishedAt: metav1.NewTime(r.now()),
				Evidence:   "node_deleted",
			}
		}
	}
	if identityChanged {
		changed, updateErr := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			recordCurrentProcess(status, observed, priorTermination)
			if status.AcceptedConfiguration != nil &&
				status.AcceptedConfiguration.Mode == valkeyv1alpha1.ValkeyModeSingle &&
				current.Termination != nil {
				setPrimaryIdentity(status, observed)
			}
			if hasUnterminatedPreviousProcess(status.PreviousProcesses) {
				if status.Initialized {
					status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
					status.Reason = "FENCING_REQUIRED"
				}
				setCondition(
					instance,
					status,
					conditionTypeRecoveryRequired,
					metav1.ConditionTrue,
					"TerminationProofLost",
					"прежний процесс не имеет доказательства остановки",
				)
			}
		})
		if updateErr == nil && changed && hasUnterminatedPreviousProcess(instance.Status.PreviousProcesses) {
			r.recordTerminationProofLostEvent(instance, "прежний процесс не имеет доказательства остановки")
		}

		return requeueIf(changed), updateErr
	}

	unsafeHistory := hasUnterminatedPreviousProcess(instance.Status.PreviousProcesses)
	if (emptyPrimaryAssignmentAllowed(instance) || unsafeHistory) &&
		(state.AppEnabled || !appACLMatches(state.AppPasswordHashes, appHash)) {
		update := r.UpdateAppAccess
		if update == nil {
			update = r.updateAppAccess
		}
		if _, err := update(
			ctx,
			net.JoinHostPort(pod.Status.PodIP, "6379"),
			string(credentials.OperatorPassword),
			appHash,
			false,
		); err != nil {
			return ctrl.Result{}, err
		}

		return requeueIf(true), nil
	}

	_, err = r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		recordCurrentProcess(status, observed, priorTermination)
		if status.PrimaryOrdinal != nil && *status.PrimaryOrdinal == observed.Ordinal &&
			status.PrimaryPodUID == observed.PodUID &&
			status.PrimaryContainerID == observed.ContainerID &&
			status.PrimaryRunID == "" &&
			instance.Status.AcceptedConfiguration.Mode == valkeyv1alpha1.ValkeyModeSingle {
			status.PrimaryRunID = observed.RunID
			status.PrimaryNodeName = observed.NodeName
			status.PrimaryNodeUID = observed.NodeUID
		}
		if hasUnterminatedPreviousProcess(status.PreviousProcesses) {
			if status.Initialized {
				status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
				status.Reason = "FENCING_REQUIRED"
			}
			setCondition(
				instance,
				status,
				conditionTypeRecoveryRequired,
				metav1.ConditionTrue,
				"TerminationProofLost",
				"прежний процесс не имеет доказательства остановки",
			)
		}
		now := r.now()
		if heartbeatDue(status.ObservedAt, now) {
			observedAt := metav1.NewTime(now)
			status.ObservedAt = &observedAt
		}
	})

	return ctrl.Result{}, err
}

func retainedProcessRecovery(
	current valkeyv1alpha1.NodeStatus,
	observed valkeyv1alpha1.NodeStatus,
	deletionTimestamp *metav1.Time,
) *valkeyv1alpha1.ProcessRecoveryStatus {
	if !sameProcess(current, observed) || current.Recovery == nil ||
		current.Recovery.Stage != valkeyv1alpha1.ProcessRecoveryStageWaitingForTermination &&
			current.Recovery.DeleteRequestedAt == nil && deletionTimestamp.IsZero() {
		return nil
	}
	recovery := *current.Recovery
	if current.Recovery.DeleteRequestedAt != nil {
		recovery.DeleteRequestedAt = current.Recovery.DeleteRequestedAt.DeepCopy()
	}
	if !deletionTimestamp.IsZero() && recovery.Stage == valkeyv1alpha1.ProcessRecoveryStageDeleting {
		recovery.Stage = valkeyv1alpha1.ProcessRecoveryStageWaitingForTermination
		recovery.DeleteRequestedAt = deletionTimestamp.DeepCopy()
	}
	return &recovery
}

func (r *ValkeyInstanceReconciler) processUnavailable(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	ordinal int32,
) (ctrl.Result, error) {
	_, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		for index := range status.Nodes {
			if status.Nodes[index].Ordinal == ordinal {
				status.Nodes[index].Readiness = false
			}
		}
		if status.Initialized && (status.PrimaryOrdinal == nil || *status.PrimaryOrdinal == ordinal) {
			status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
			status.Reason = "PRIMARY_NOT_READY"
		}
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
}

func expectedProcessCount(instance *valkeyv1alpha1.ValkeyInstance) int32 {
	if instance.Status.AcceptedConfiguration != nil &&
		instance.Status.AcceptedConfiguration.Mode == valkeyv1alpha1.ValkeyModeHA {
		return 3
	}

	return 1
}

func earlierRequeue(left, right ctrl.Result) ctrl.Result {
	if left.RequeueAfter <= 0 {
		return right
	}
	if right.RequeueAfter <= 0 || left.RequeueAfter <= right.RequeueAfter {
		return left
	}

	return right
}

func heartbeatDue(previous *metav1.Time, now time.Time) bool {
	return previous == nil || now.Before(previous.Time) ||
		now.Sub(previous.Time) >= config.HealthCheckInterval
}

func processNeedsControl(
	current valkeyv1alpha1.NodeStatus,
	found bool,
	observationStartedAt time.Time,
) bool {
	return observationStartedAt.IsZero() || !found || current.Replication == nil ||
		current.Replication.ObservedAt.Time.Before(observationStartedAt)
}

func (r *ValkeyInstanceReconciler) inspectProcess(
	ctx context.Context,
	address string,
	operatorPassword string,
	appHash string,
	takeControl bool,
) (operatorvalkey.ProcessState, error) {
	cfg := operatorvalkey.ClientConfig{
		Address:        address,
		Username:       "operator",
		Password:       operatorPassword,
		DialTimeout:    config.ValkeyDialTimeout,
		CommandTimeout: config.ValkeyCommandTimeout,
	}
	connection, err := r.dialValkey(ctx, cfg)
	if err != nil {
		return operatorvalkey.ProcessState{}, err
	}
	defer connection.Close()

	session, err := connection.OpenSession(ctx)
	if err != nil {
		return operatorvalkey.ProcessState{}, err
	}
	defer session.Close()

	if takeControl {
		return session.TakeControl(ctx, operatorPassword)
	}
	return session.Observe(ctx, operatorPassword)
}

func valkeyContainerStatus(statuses []corev1.ContainerStatus) *corev1.ContainerStatus {
	for index := range statuses {
		if statuses[index].Name == "valkey" {
			return &statuses[index]
		}
	}

	return nil
}

func podReady(conditions []corev1.PodCondition) bool {
	for _, condition := range conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func sameProcess(current, observed valkeyv1alpha1.NodeStatus) bool {
	return sameContainerProcess(current, observed) && current.RunID == observed.RunID
}

func sameContainerProcess(current, observed valkeyv1alpha1.NodeStatus) bool {
	return current.Ordinal == observed.Ordinal && current.PodUID == observed.PodUID &&
		current.ContainerID == observed.ContainerID &&
		current.NodeName == observed.NodeName &&
		current.NodeUID == observed.NodeUID
}

func nodeStatusAtOrdinal(nodes []valkeyv1alpha1.NodeStatus, ordinal int32) (valkeyv1alpha1.NodeStatus, bool) {
	for _, node := range nodes {
		if node.Ordinal == ordinal {
			return node, true
		}
	}

	return valkeyv1alpha1.NodeStatus{}, false
}

func recordCurrentProcess(
	status *valkeyv1alpha1.ValkeyInstanceStatus,
	observed valkeyv1alpha1.NodeStatus,
	priorTermination *valkeyv1alpha1.ProcessTermination,
) {
	current, found := nodeStatusAtOrdinal(status.Nodes, observed.Ordinal)
	if found && sameProcess(current, observed) {
		observed.Termination = current.Termination
		status.Nodes = replaceNodeStatus(status.Nodes, observed)
		return
	}
	if found {
		if current.Termination == nil && priorTermination != nil {
			current.Termination = priorTermination.DeepCopy()
		}
		status.PreviousProcesses = appendPreviousProcess(status.PreviousProcesses, current)
	}

	status.Nodes = replaceNodeStatus(status.Nodes, observed)
}

func replaceNodeStatus(
	nodes []valkeyv1alpha1.NodeStatus,
	observed valkeyv1alpha1.NodeStatus,
) []valkeyv1alpha1.NodeStatus {
	result := make([]valkeyv1alpha1.NodeStatus, 0, len(nodes)+1)
	replaced := false
	for _, node := range nodes {
		if node.Ordinal == observed.Ordinal {
			result = append(result, observed)
			replaced = true
			continue
		}
		result = append(result, node)
	}
	if !replaced {
		result = append(result, observed)
	}
	slices.SortFunc(result, func(left, right valkeyv1alpha1.NodeStatus) int {
		return int(left.Ordinal - right.Ordinal)
	})

	return result
}

func appendPreviousProcess(
	processes []valkeyv1alpha1.NodeStatus,
	process valkeyv1alpha1.NodeStatus,
) []valkeyv1alpha1.NodeStatus {
	result := append([]valkeyv1alpha1.NodeStatus(nil), processes...)
	for index := range result {
		if sameProcess(result[index], process) {
			result[index] = process
			return result
		}
	}

	return append(result, process)
}

func hasUnterminatedPreviousProcess(processes []valkeyv1alpha1.NodeStatus) bool {
	return slices.ContainsFunc(processes, func(process valkeyv1alpha1.NodeStatus) bool {
		return process.Termination == nil
	})
}

func observedReplication(state *operatorvalkey.ReplicationState, now time.Time) *valkeyv1alpha1.ReplicationStatus {
	if state == nil {
		return nil
	}

	result := &valkeyv1alpha1.ReplicationStatus{
		ReplicationID:          state.ReplicationID,
		SecondaryReplicationID: state.SecondaryReplicationID,
		SecondaryOffset:        state.SecondaryOffset,
		Offset:                 state.Offset,
		UpstreamHost:           state.UpstreamHost,
		UpstreamPort:           state.UpstreamPort,
		LinkUp:                 state.LinkUp,
		SyncInProgress:         state.SyncInProgress,
		ObservedAt:             metav1.NewTime(now),
	}
	if state.SyncedAt != nil {
		syncedAt := metav1.NewTime(state.SyncedAt.UTC())
		result.SyncedAt = &syncedAt
	}

	return result
}

func mergeObservedReplication(
	previous *valkeyv1alpha1.ReplicationStatus,
	observed *valkeyv1alpha1.ReplicationStatus,
	role valkeyv1alpha1.NodeRole,
	now time.Time,
) *valkeyv1alpha1.ReplicationStatus {
	if observed == nil {
		return nil
	}
	sameHistory := previous != nil &&
		previous.ReplicationID == observed.ReplicationID &&
		previous.SecondaryReplicationID == observed.SecondaryReplicationID &&
		previous.UpstreamHost == observed.UpstreamHost &&
		previous.UpstreamPort == observed.UpstreamPort
	if sameHistory && previous.SyncedAt != nil {
		observed.SyncedAt = previous.SyncedAt.DeepCopy()
	}
	if role == valkeyv1alpha1.NodeRoleReplica && observed.LinkUp && !observed.SyncInProgress &&
		observed.SyncedAt == nil {
		syncedAt := metav1.NewTime(now)
		observed.SyncedAt = &syncedAt
	}

	return observed
}
