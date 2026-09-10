package operator

import (
	"context"
	"fmt"
	"net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

type ProcessPromoter func(context.Context, string, string, string) (operatorvalkey.ProcessState, error)

type ProcessFollower func(
	context.Context,
	string,
	string,
	string,
	string,
	int32,
) (operatorvalkey.ProcessState, error)

func (r *ValkeyInstanceReconciler) reconcileTopology(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	if instance.Status.AcceptedConfiguration.Mode != valkeyv1alpha1.ValkeyModeHA {
		return ctrl.Result{}, nil
	}
	if emptyPrimaryAssignmentAllowed(instance) {
		result, err := r.reconcileInitialPrimary(ctx, instance)
		if err != nil || !result.IsZero() || instance.Status.PrimaryOrdinal == nil {
			return result, err
		}
	}

	return r.reconcileReplicas(ctx, instance)
}

func emptyPrimaryAssignmentAllowed(instance *valkeyv1alpha1.ValkeyInstance) bool {
	status := &instance.Status
	if !status.Initialized {
		return true
	}
	if status.PrimaryOrdinal != nil {
		primary, found := nodeStatusAtOrdinal(status.Nodes, *status.PrimaryOrdinal)
		if found && primaryIdentityMatches(instance, primary) &&
			primary.Role == valkeyv1alpha1.NodeRolePrimary && primary.Termination == nil {
			return false
		}
	}
	if rolloutRequiresFullStop(instance) && status.Rollout != nil &&
		(status.Rollout.Stage == valkeyv1alpha1.RolloutStageStarting ||
			status.Rollout.Stage == valkeyv1alpha1.RolloutStageVerifying) {
		if status.PrimaryOrdinal == nil {
			return true
		}
		target, found := nodeStatusAtOrdinal(status.Nodes, *status.PrimaryOrdinal)
		return found && target.Termination == nil && primaryIdentityMatches(instance, target)
	}
	condition := findCondition(status.Conditions, conditionTypeDataLoss)
	return status.Reason == "EMPTY_RECOVERY" && condition != nil &&
		condition.Status == metav1.ConditionTrue && condition.Reason == "AllProcessesStopped"
}

func (r *ValkeyInstanceReconciler) reconcileInitialPrimary(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	target, found := nodeStatusAtOrdinal(instance.Status.Nodes, 0)
	if !found || target.Termination != nil || target.Observation != nil ||
		target.AppEnabled ||
		target.AppPasswordVersion != instance.Status.AcceptedConfiguration.PasswordVersion {
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}
	if instance.Status.PrimaryOrdinal == nil || !primaryIdentityMatches(instance, target) {
		if instance.Status.PrimaryOrdinal != nil && !initialPrimaryReplacementSafe(*instance, target) {
			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			ordinal := int32(0)
			status.PrimaryOrdinal = &ordinal
			status.PrimaryPodUID = target.PodUID
			status.PrimaryContainerID = target.ContainerID
			status.PrimaryRunID = target.RunID
			status.PrimaryNodeName = target.NodeName
			status.PrimaryNodeUID = target.NodeUID
		})

		return requeueIf(changed), err
	}
	if target.Role == valkeyv1alpha1.NodeRolePrimary {
		return ctrl.Result{}, nil
	}
	if target.Role != valkeyv1alpha1.NodeRoleReplica {
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}

	pod, err := r.currentProcessPod(ctx, instance, target)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("прочитать Pod первоначального primary: %w", err)
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
		target.RunID,
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	if state.RunID != target.RunID || state.AppEnabled ||
		state.Role != string(valkeyv1alpha1.NodeRoleReplica) {
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}

	return requeueIf(true), nil
}

func (r *ValkeyInstanceReconciler) promoteProcess(
	ctx context.Context,
	address string,
	operatorPassword string,
	expectedRunID string,
) (operatorvalkey.ProcessState, error) {
	connection, err := r.dialValkey(ctx, operatorvalkey.ClientConfig{
		Address: address, Username: "operator", Password: operatorPassword,
		DialTimeout: config.ValkeyDialTimeout, CommandTimeout: config.ValkeyCommandTimeout,
	})
	if err != nil {
		return operatorvalkey.ProcessState{}, err
	}
	defer connection.Close()
	session, err := connection.OpenSession(ctx)
	if err != nil {
		return operatorvalkey.ProcessState{}, err
	}
	defer session.Close()
	state, err := session.TakeControl(ctx, operatorPassword)
	if err != nil {
		return operatorvalkey.ProcessState{}, err
	}
	if state.RunID != expectedRunID || state.Role != string(valkeyv1alpha1.NodeRoleReplica) {
		return state, nil
	}
	if err := session.Promote(ctx); err != nil {
		return state, err
	}

	return state, nil
}

func (r *ValkeyInstanceReconciler) reconcileReplicas(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	if instance.Status.PrimaryOrdinal == nil {
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}
	primary, found := nodeStatusAtOrdinal(instance.Status.Nodes, *instance.Status.PrimaryOrdinal)
	if !found || primary.Role != valkeyv1alpha1.NodeRolePrimary || primary.Termination != nil ||
		primary.Observation != nil || !primaryIdentityMatches(instance, primary) {
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}
	primaryPod, err := r.currentProcessPod(ctx, instance, primary)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("прочитать Pod primary для репликации: %w", err)
	}

	for _, replica := range instance.Status.Nodes {
		if replica.Ordinal == primary.Ordinal || replica.Termination != nil || replica.Observation != nil {
			continue
		}
		allowFencedSource := instance.Status.Failover != nil &&
			instance.Status.Failover.Stage == valkeyv1alpha1.FailoverStageReconfiguring &&
			sameIdentity(instance.Status.Failover.Source, processIdentity(replica)) && !replica.AppEnabled
		if replica.Role != valkeyv1alpha1.NodeRoleReplica && !allowFencedSource ||
			replica.AppPasswordVersion != instance.Status.AcceptedConfiguration.PasswordVersion {
			continue
		}
		if replica.Replication != nil &&
			replica.Replication.UpstreamHost == primaryPod.Status.PodIP &&
			replica.Replication.UpstreamPort == valkeyPort {
			continue
		}
		replicaPod, err := r.currentProcessPod(ctx, instance, replica)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("прочитать Pod реплики: %w", err)
		}
		credentials, _, err := r.processCredentials(ctx, instance)
		if err != nil {
			return ctrl.Result{}, err
		}
		follow := r.FollowProcess
		if follow == nil {
			follow = r.followProcess
		}
		state, err := follow(
			ctx,
			net.JoinHostPort(replicaPod.Status.PodIP, "6379"),
			string(credentials.OperatorPassword),
			replica.RunID,
			primaryPod.Status.PodIP,
			valkeyPort,
		)
		if err != nil {
			return ctrl.Result{}, err
		}
		if state.RunID != replica.RunID || state.Role != string(valkeyv1alpha1.NodeRoleReplica) ||
			state.AppEnabled {
			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}

		return requeueIf(true), nil
	}

	return ctrl.Result{}, nil
}

func (r *ValkeyInstanceReconciler) followProcess(
	ctx context.Context,
	address string,
	operatorPassword string,
	expectedRunID string,
	upstreamHost string,
	upstreamPort int32,
) (operatorvalkey.ProcessState, error) {
	connection, err := r.dialValkey(ctx, operatorvalkey.ClientConfig{
		Address: address, Username: "operator", Password: operatorPassword,
		DialTimeout: config.ValkeyDialTimeout, CommandTimeout: config.ValkeyCommandTimeout,
	})
	if err != nil {
		return operatorvalkey.ProcessState{}, err
	}
	defer connection.Close()
	session, err := connection.OpenSession(ctx)
	if err != nil {
		return operatorvalkey.ProcessState{}, err
	}
	defer session.Close()
	state, err := session.TakeControl(ctx, operatorPassword)
	if err != nil {
		return operatorvalkey.ProcessState{}, err
	}
	if state.RunID != expectedRunID {
		return state, nil
	}
	if state.Role == string(valkeyv1alpha1.NodeRoleReplica) &&
		state.Replication != nil && state.Replication.UpstreamHost == upstreamHost &&
		state.Replication.UpstreamPort == upstreamPort {
		return state, nil
	}
	if state.Role == string(valkeyv1alpha1.NodeRolePrimary) && state.AppEnabled {
		return state, nil
	}
	if err := session.Follow(ctx, upstreamHost, upstreamPort); err != nil {
		return state, err
	}

	return state, nil
}

func primaryIdentityMatches(
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) bool {
	status := instance.Status
	legacySingle := instance.Status.AcceptedConfiguration != nil &&
		instance.Status.AcceptedConfiguration.Mode == valkeyv1alpha1.ValkeyModeSingle &&
		((status.PrimaryOrdinal == nil && status.PrimaryPodUID == "" && status.PrimaryContainerID == "") ||
			(status.PrimaryRunID == "" && status.PrimaryNodeName == "" && status.PrimaryNodeUID == ""))
	if legacySingle {
		return process.Ordinal == 0
	}

	return status.PrimaryOrdinal != nil && *status.PrimaryOrdinal == process.Ordinal &&
		status.PrimaryPodUID == process.PodUID &&
		status.PrimaryContainerID == process.ContainerID &&
		(legacySingle || status.PrimaryRunID == process.RunID &&
			status.PrimaryNodeName == process.NodeName &&
			status.PrimaryNodeUID == process.NodeUID)
}

func selectedPrimaryOrdinal(instance *valkeyv1alpha1.ValkeyInstance) (int32, bool) {
	if instance.Status.PrimaryOrdinal != nil {
		return *instance.Status.PrimaryOrdinal, true
	}
	if instance.Status.AcceptedConfiguration != nil &&
		instance.Status.AcceptedConfiguration.Mode == valkeyv1alpha1.ValkeyModeSingle {
		return 0, true
	}

	return 0, false
}

func initialPrimaryReplacementSafe(
	instance valkeyv1alpha1.ValkeyInstance,
	current valkeyv1alpha1.NodeStatus,
) bool {
	if instance.Status.PrimaryOrdinal == nil || *instance.Status.PrimaryOrdinal != current.Ordinal {
		return false
	}
	previous := valkeyv1alpha1.NodeStatus{
		Ordinal: *instance.Status.PrimaryOrdinal,
		PodUID:  instance.Status.PrimaryPodUID, ContainerID: instance.Status.PrimaryContainerID,
		RunID: instance.Status.PrimaryRunID, NodeName: instance.Status.PrimaryNodeName,
		NodeUID: instance.Status.PrimaryNodeUID,
	}
	for _, process := range instance.Status.PreviousProcesses {
		if sameProcess(process, previous) {
			return process.Termination != nil
		}
	}

	return false
}
