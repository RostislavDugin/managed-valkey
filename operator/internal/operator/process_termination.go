package operator

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func (r *ValkeyInstanceReconciler) saveContainerTermination(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	pod *corev1.Pod,
	container *corev1.ContainerStatus,
) (ctrl.Result, error) {
	terminated := container.State.Terminated
	if terminated == nil {
		return ctrl.Result{}, nil
	}
	if len(instance.Status.Nodes) > 0 {
		current := instance.Status.Nodes[0]
		if current.PodUID != string(pod.UID) || current.ContainerID != container.ContainerID {
			return r.terminationProofLost(ctx, instance)
		}
		if current.Termination != nil {
			return r.releaseTerminatedPod(ctx, instance, pod, current)
		}
	}

	nodeName := pod.Spec.NodeName
	nodeUID := ""
	if len(instance.Status.Nodes) > 0 {
		nodeName = instance.Status.Nodes[0].NodeName
		nodeUID = instance.Status.Nodes[0].NodeUID
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
		node := valkeyv1alpha1.NodeStatus{
			Ordinal:     0,
			PodUID:      string(pod.UID),
			ContainerID: container.ContainerID,
			NodeName:    nodeName,
			NodeUID:     nodeUID,
			Termination: &valkeyv1alpha1.ProcessTermination{
				Reason:     reason,
				ExitCode:   terminated.ExitCode,
				FinishedAt: finishedAt,
				Evidence:   "container_status",
			},
		}
		if len(status.Nodes) > 0 {
			node = status.Nodes[0]
			node.Readiness = false
			node.Termination = &valkeyv1alpha1.ProcessTermination{
				Reason:     reason,
				ExitCode:   terminated.ExitCode,
				FinishedAt: finishedAt,
				Evidence:   "container_status",
			}
		}
		status.Nodes = []valkeyv1alpha1.NodeStatus{node}
		if status.Initialized {
			status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
			status.Reason = "PRIMARY_NOT_READY"
		}
		setCondition(
			instance,
			status,
			conditionTypeProcessReady,
			metav1.ConditionFalse,
			"ProcessTerminated",
			"завершение процесса подтверждено kubelet",
		)
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
	if pod.DeletionTimestamp.IsZero() {
		uid := pod.UID
		resourceVersion := pod.ResourceVersion
		err := r.Delete(ctx, pod, &client.DeleteOptions{Raw: &metav1.DeleteOptions{
			GracePeriodSeconds: ptr.To[int64](30),
			Preconditions: &metav1.Preconditions{
				UID: &uid, ResourceVersion: &resourceVersion,
			},
		}})
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
		if status.Initialized {
			status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
			status.Reason = "PRIMARY_NOT_READY"
		}
	})
	if err != nil {
		return ctrl.Result{}, err
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

	return requeueIf(changed), err
}
