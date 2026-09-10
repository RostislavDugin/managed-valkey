package operator

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

func (r *ValkeyInstanceReconciler) reconcileProcessHistory(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		status.PreviousProcesses = slices.DeleteFunc(
			status.PreviousProcesses,
			func(process valkeyv1alpha1.NodeStatus) bool {
				return process.Termination != nil && !previousProcessRequired(*status, process)
			},
		)
	})
	return requeueIf(changed), err
}

func previousProcessRequired(
	status valkeyv1alpha1.ValkeyInstanceStatus,
	process valkeyv1alpha1.NodeStatus,
) bool {
	identity := processIdentity(process)
	if status.PrimaryOrdinal != nil &&
		status.PrimaryPodUID == identity.PodUID &&
		status.PrimaryContainerID == identity.ContainerID &&
		status.PrimaryRunID == identity.RunID &&
		status.PrimaryNodeName == identity.NodeName &&
		status.PrimaryNodeUID == identity.NodeUID {
		return true
	}
	if status.Failover != nil {
		if sameIdentity(status.Failover.Source, identity) ||
			(status.Failover.Candidate != nil && sameIdentity(*status.Failover.Candidate, identity)) {
			return true
		}
	}
	return status.Rollout != nil && status.Rollout.Process != nil && sameIdentity(*status.Rollout.Process, identity)
}

func (r *ValkeyInstanceReconciler) saveContainerTermination(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	pod *corev1.Pod,
	container *corev1.ContainerStatus,
	ordinal int32,
) (ctrl.Result, error) {
	terminated := container.State.Terminated
	if terminated == nil {
		return ctrl.Result{}, nil
	}
	current, found := nodeStatusAtOrdinal(instance.Status.Nodes, ordinal)
	identityChanged := found &&
		(current.PodUID != string(pod.UID) || current.ContainerID != container.ContainerID)
	if found {
		if identityChanged && instance.Status.Deletion == nil {
			return r.terminationProofLost(ctx, instance)
		}
		if !identityChanged && current.Termination != nil {
			return r.releaseTerminatedPod(ctx, instance, pod, current)
		}
	}

	nodeName := pod.Spec.NodeName
	nodeUID := ""
	if found && !identityChanged {
		nodeName = current.NodeName
		nodeUID = current.NodeUID
	} else if nodeName != "" {
		node := &corev1.Node{}
		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("прочитать Node завершённого процесса: %w", err)
		}
		nodeUID = string(node.UID)
	}
	finishedAt := terminated.FinishedAt
	if finishedAt.IsZero() {
		finishedAt = metav1.NewTime(r.now())
	}
	reason := terminated.Reason
	if reason == "" {
		reason = "Terminated"
	}

	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		termination := &valkeyv1alpha1.ProcessTermination{
			Reason: reason, ExitCode: terminated.ExitCode, FinishedAt: finishedAt, Evidence: "container_status",
		}
		node := valkeyv1alpha1.NodeStatus{
			Ordinal:     ordinal,
			PodUID:      string(pod.UID),
			ContainerID: container.ContainerID,
			NodeName:    nodeName,
			NodeUID:     nodeUID,
			Termination: termination.DeepCopy(),
		}
		if saved, exists := nodeStatusAtOrdinal(status.Nodes, ordinal); exists {
			if sameContainerProcess(saved, node) {
				node = saved
				node.Readiness = false
				node.Termination = termination.DeepCopy()
			} else if status.Deletion != nil {
				status.PreviousProcesses = appendPreviousProcess(status.PreviousProcesses, saved)
			}
		}
		status.Nodes = replaceNodeStatus(status.Nodes, node)
		for index := range status.PreviousProcesses {
			if sameContainerProcess(status.PreviousProcesses[index], node) &&
				status.PreviousProcesses[index].Termination == nil {
				status.PreviousProcesses[index].Readiness = false
				status.PreviousProcesses[index].Termination = termination.DeepCopy()
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

	return requeueIf(changed), nil
}

func (r *ValkeyInstanceReconciler) releaseTerminatedPod(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	pod *corev1.Pod,
	node valkeyv1alpha1.NodeStatus,
) (ctrl.Result, error) {
	if node.Termination == nil || node.PodUID != string(pod.UID) {
		return ctrl.Result{}, nil
	}
	if err := runProcessActionControl(ctx, "release-terminated", pod, node); err != nil {
		return ctrl.Result{}, err
	}
	if pod.DeletionTimestamp.IsZero() {
		uid := pod.UID
		resourceVersion := pod.ResourceVersion
		gracePeriod := config.ProcessDeletionGracePeriod
		if node.Termination.Evidence == "manual_fencing" || node.Termination.Evidence == "node_deleted" {
			gracePeriod = 0
		}
		err := r.Delete(ctx, pod, &client.DeleteOptions{
			GracePeriodSeconds: new(gracePeriod),
			Preconditions: &metav1.Preconditions{
				UID: &uid, ResourceVersion: &resourceVersion,
			},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("удалить завершённый Pod: %w", err)
		}

		return requeueIf(true), nil
	}
	if !slices.Contains(pod.Finalizers, processFinalizer) {
		return ctrl.Result{}, nil
	}
	before := pod.DeepCopy()
	pod.Finalizers = slices.DeleteFunc(pod.Finalizers, func(value string) bool {
		return value == processFinalizer
	})
	if err := r.Patch(ctx, pod, client.MergeFrom(before)); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("снять finalizer завершённого Pod: %w", err)
	}

	return requeueIf(true), nil
}

func (r *ValkeyInstanceReconciler) saveNodeDeletionTermination(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	node valkeyv1alpha1.NodeStatus,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		for index := range status.Nodes {
			if !sameProcess(status.Nodes[index], node) {
				continue
			}
			status.Nodes[index].Readiness = false
			status.Nodes[index].Termination = &valkeyv1alpha1.ProcessTermination{
				Reason:     "NodeDeleted",
				FinishedAt: metav1.NewTime(r.now()),
				Evidence:   "node_deleted",
			}
		}
		if status.Initialized && processFailureAffectsPrimary(status, node.Ordinal) {
			status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
			status.Reason = "PRIMARY_NOT_READY"
		} else if status.Initialized && status.AcceptedConfiguration != nil &&
			status.AcceptedConfiguration.Mode == valkeyv1alpha1.ValkeyModeHA &&
			status.Failover == nil && status.Rollout == nil && status.CredentialRotation == nil {
			status.Phase = valkeyv1alpha1.InstancePhaseDegraded
			status.Reason = "REPLICAS_NOT_READY"
		}
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed && r.Recorder != nil {
		r.Recorder.Eventf(
			instance,
			nil,
			corev1.EventTypeWarning,
			"ProcessNodeDeleted",
			"Fencing",
			"остановка процесса ordinal %d подтверждена удалением Node %s",
			node.Ordinal,
			node.NodeName,
		)
	}

	return requeueIf(changed), nil
}

func (r *ValkeyInstanceReconciler) previousNodeDeleted(
	ctx context.Context,
	node valkeyv1alpha1.NodeStatus,
) (bool, error) {
	if node.NodeName == "" || node.NodeUID == "" {
		return false, nil
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	current := &corev1.Node{}
	if err := reader.Get(ctx, client.ObjectKey{Name: node.NodeName}, current); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}

		return false, fmt.Errorf("проверить прежний Node: %w", err)
	}

	return string(current.UID) != node.NodeUID, nil
}

func (r *ValkeyInstanceReconciler) terminationProofLost(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
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
			"идентичность процесса изменилась без доказательства остановки",
		)
	})
	if err == nil && changed {
		r.recordTerminationProofLostEvent(
			instance,
			"идентичность процесса изменилась без доказательства остановки",
		)
	}

	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) recordTerminationProofLostEvent(
	instance *valkeyv1alpha1.ValkeyInstance,
	message string,
) {
	if r.Recorder != nil {
		r.Recorder.Eventf(
			instance,
			nil,
			corev1.EventTypeWarning,
			"TerminationProofLost",
			"Fencing",
			message,
		)
	}
}
