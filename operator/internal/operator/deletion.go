package operator

import (
	"context"
	"fmt"
	"net"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

const instanceFinalizer = valkeyv1alpha1.InstanceFinalizer

func (r *ValkeyInstanceReconciler) ensureInstanceFinalizer(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
	if slices.Contains(instance.Finalizers, instanceFinalizer) {
		return false, nil
	}
	before := instance.DeepCopy()
	instance.Finalizers = append(instance.Finalizers, instanceFinalizer)
	if err := r.Patch(ctx, instance, client.MergeFrom(before)); err != nil {
		return false, fmt.Errorf("добавить finalizer ValkeyInstance: %w", err)
	}

	return true, nil
}

func (r *ValkeyInstanceReconciler) reconcileDeletion(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	if !slices.Contains(instance.Finalizers, instanceFinalizer) {
		return ctrl.Result{}, nil
	}
	if instance.Status.Deletion == nil {
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			status.Deletion = &valkeyv1alpha1.DeletionStatus{
				Stage: valkeyv1alpha1.DeletionStageRemovingNetwork, StartedAt: metav1.NewTime(r.now()),
			}
			setCondition(
				instance,
				status,
				conditionTypePublicReady,
				metav1.ConditionFalse,
				"Deleting",
				"инстанс удаляется",
			)
		})
		if err == nil && changed {
			err = runDeletionActionControl(ctx, "stage-saved", instance)
		}

		return requeueIf(changed), err
	}

	switch instance.Status.Deletion.Stage {
	case valkeyv1alpha1.DeletionStageRemovingNetwork:
		if _, err := r.removeNetworkResources(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		if err := runDeletionActionControl(ctx, "action-completed", instance); err != nil {
			return ctrl.Result{}, err
		}

		return r.advanceDeletion(ctx, instance, valkeyv1alpha1.DeletionStageDisablingApp)
	case valkeyv1alpha1.DeletionStageDisablingApp:
		changed, err := r.disableAppForDeletion(ctx, instance)
		if err != nil || changed {
			return requeueIf(changed), err
		}
		if err := runDeletionActionControl(ctx, "action-completed", instance); err != nil {
			return ctrl.Result{}, err
		}

		return r.advanceDeletion(ctx, instance, valkeyv1alpha1.DeletionStageStopping)
	case valkeyv1alpha1.DeletionStageStopping:
		changed, err := r.requestDeletionPods(ctx, instance)
		if err != nil {
			return ctrl.Result{}, err
		}
		if changed {
			if err := runDeletionActionControl(ctx, "action-completed", instance); err != nil {
				return ctrl.Result{}, err
			}

			return requeueIf(true), nil
		}

		return r.advanceDeletion(ctx, instance, valkeyv1alpha1.DeletionStageVerifying)
	case valkeyv1alpha1.DeletionStageVerifying:
		return r.verifyDeletion(ctx, instance)
	default:
		return ctrl.Result{}, fmt.Errorf("неизвестная стадия удаления %q", instance.Status.Deletion.Stage)
	}
}

func (r *ValkeyInstanceReconciler) advanceDeletion(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	stage valkeyv1alpha1.DeletionStage,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		status.Deletion.Stage = stage
	})
	if err == nil && changed {
		err = runDeletionActionControl(ctx, "stage-saved", instance)
	}

	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) disableAppForDeletion(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
	credentials, appHash, err := r.processCredentials(ctx, instance)
	if err != nil {
		if !deletionHasProcesses(instance.Status) {
			exists, checkErr := r.deletionProcessExists(ctx, instance)
			if checkErr != nil {
				return false, checkErr
			}
			if !exists {
				return false, nil
			}
		}
		return false, err
	}
	update := r.UpdateAppAccess
	if update == nil {
		update = r.updateAppAccess
	}
	for ordinal := range expectedProcessCount(instance) {
		pod := &corev1.Pod{}
		key := client.ObjectKey{
			Namespace: instance.Namespace,
			Name:      fmt.Sprintf("%s-%d", instance.Status.AcceptedConfiguration.Slug, ordinal),
		}
		if err := r.Get(ctx, key, pod); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return false, fmt.Errorf("прочитать Pod при удалении: %w", err)
		}
		if !podOwnedByInstance(pod, instance) {
			continue
		}
		if _, exists := pod.Labels[applicationRoleLabel]; exists {
			before := pod.DeepCopy()
			pod.Labels = cloneWithoutKey(pod.Labels, applicationRoleLabel)
			if err := r.Patch(ctx, pod, client.MergeFrom(before)); err != nil {
				return false, fmt.Errorf("закрыть Service при удалении: %w", err)
			}

			return true, nil
		}
		container := valkeyContainerStatus(pod.Status.ContainerStatuses)
		if pod.Status.PodIP == "" || container == nil || container.State.Running == nil {
			continue
		}
		if _, err := update(
			ctx,
			net.JoinHostPort(pod.Status.PodIP, "6379"),
			string(credentials.OperatorPassword),
			appHash,
			false,
		); err != nil {
			continue
		}
	}

	return false, nil
}

func deletionHasProcesses(status valkeyv1alpha1.ValkeyInstanceStatus) bool {
	return len(status.Nodes) > 0 || len(status.PreviousProcesses) > 0 || status.Initialized
}

func (r *ValkeyInstanceReconciler) deletionProcessExists(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
	for ordinal := range expectedProcessCount(instance) {
		pod := &corev1.Pod{}
		key := client.ObjectKey{
			Namespace: instance.Namespace,
			Name:      fmt.Sprintf("%s-%d", instance.Status.AcceptedConfiguration.Slug, ordinal),
		}
		if err := r.Get(ctx, key, pod); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("проверить Pod перед удалением без Secret: %w", err)
		}
		if podOwnedByInstance(pod, instance) {
			return true, nil
		}
	}
	return false, nil
}

func cloneWithoutKey(values map[string]string, key string) map[string]string {
	result := make(map[string]string, len(values))
	for name, value := range values {
		if name != key {
			result[name] = value
		}
	}

	return result
}

func (r *ValkeyInstanceReconciler) requestDeletionPods(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
	for ordinal := range expectedProcessCount(instance) {
		pod := &corev1.Pod{}
		key := client.ObjectKey{
			Namespace: instance.Namespace,
			Name:      fmt.Sprintf("%s-%d", instance.Status.AcceptedConfiguration.Slug, ordinal),
		}
		if err := r.Get(ctx, key, pod); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("прочитать Pod при удалении: %w", err)
		}
		if !podOwnedByInstance(pod, instance) || !pod.DeletionTimestamp.IsZero() {
			continue
		}
		uid := pod.UID
		resourceVersion := pod.ResourceVersion
		gracePeriod := config.ProcessDeletionGracePeriod
		if err := r.Delete(ctx, pod, &client.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
			Preconditions:      &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
		}); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("остановить Pod при удалении: %w", err)
		}

		return true, nil
	}

	return false, nil
}

func (r *ValkeyInstanceReconciler) verifyDeletion(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	for ordinal := range expectedProcessCount(instance) {
		pod := &corev1.Pod{}
		key := client.ObjectKey{
			Namespace: instance.Namespace,
			Name:      fmt.Sprintf("%s-%d", instance.Status.AcceptedConfiguration.Slug, ordinal),
		}
		if err := r.Get(ctx, key, pod); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("проверить Pod при удалении: %w", err)
			}
			current, found := nodeStatusAtOrdinal(instance.Status.Nodes, ordinal)
			if found && current.Termination == nil {
				deleted, checkErr := r.previousNodeDeleted(ctx, current)
				if checkErr != nil {
					return ctrl.Result{}, checkErr
				}
				if deleted {
					return r.saveNodeDeletionTermination(ctx, instance, current)
				}

				return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
			}
			continue
		}
		if !podOwnedByInstance(pod, instance) {
			current, found := nodeStatusAtOrdinal(instance.Status.Nodes, ordinal)
			if !found || current.Termination != nil {
				continue
			}
			deleted, checkErr := r.previousNodeDeleted(ctx, current)
			if checkErr != nil {
				return ctrl.Result{}, checkErr
			}
			if deleted {
				return r.saveNodeDeletionTermination(ctx, instance, current)
			}

			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}
		container := valkeyContainerStatus(pod.Status.ContainerStatuses)
		if container != nil && container.State.Terminated != nil {
			return r.saveContainerTermination(ctx, instance, pod, container, ordinal)
		}
		current, found := nodeStatusAtOrdinal(instance.Status.Nodes, ordinal)
		if found && current.Termination != nil && current.PodUID == string(pod.UID) {
			return r.releaseTerminatedPod(ctx, instance, pod, current)
		}
		if found && current.Termination == nil && current.PodUID == string(pod.UID) {
			deleted, err := r.previousNodeDeleted(ctx, current)
			if err != nil {
				return ctrl.Result{}, err
			}
			if deleted {
				return r.saveNodeDeletionTermination(ctx, instance, current)
			}
		}

		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}
	for _, previous := range instance.Status.PreviousProcesses {
		if previous.Termination != nil {
			continue
		}
		current, found := nodeStatusAtOrdinal(instance.Status.Nodes, previous.Ordinal)
		if found && current.Termination != nil && sameContainerProcess(previous, current) {
			return r.copyPreviousProcessTermination(ctx, instance, previous, current.Termination)
		}
		deleted, err := r.previousNodeDeleted(ctx, previous)
		if err != nil {
			return ctrl.Result{}, err
		}
		if deleted {
			return r.savePreviousNodeDeletionTermination(ctx, instance, previous)
		}

		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}

	return r.finishDeletion(ctx, instance)
}

func (r *ValkeyInstanceReconciler) copyPreviousProcessTermination(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
	termination *valkeyv1alpha1.ProcessTermination,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		for index := range status.PreviousProcesses {
			if sameContainerProcess(status.PreviousProcesses[index], process) &&
				status.PreviousProcesses[index].Termination == nil {
				status.PreviousProcesses[index].Readiness = false
				status.PreviousProcesses[index].Termination = termination.DeepCopy()
			}
		}
	})

	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) savePreviousNodeDeletionTermination(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		for index := range status.PreviousProcesses {
			if !sameProcess(status.PreviousProcesses[index], process) {
				continue
			}
			status.PreviousProcesses[index].Readiness = false
			status.PreviousProcesses[index].Termination = &valkeyv1alpha1.ProcessTermination{
				Reason: "NodeDeleted", FinishedAt: metav1.NewTime(r.now()), Evidence: "node_deleted",
			}
		}
	})

	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) finishDeletion(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	if err := runDeletionActionControl(ctx, "before-finalizer-removal", instance); err != nil {
		return ctrl.Result{}, err
	}
	before := instance.DeepCopy()
	instance.Finalizers = slices.DeleteFunc(instance.Finalizers, func(value string) bool {
		return value == instanceFinalizer
	})
	if err := r.Patch(ctx, instance, client.MergeFrom(before)); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("снять finalizer ValkeyInstance: %w", err)
	}

	return ctrl.Result{}, nil
}
