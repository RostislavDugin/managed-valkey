package operator

import (
	"context"
	"fmt"
	"net"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
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

		return requeueIf(changed), err
	}

	switch instance.Status.Deletion.Stage {
	case valkeyv1alpha1.DeletionStageRemovingNetwork:
		if _, err := r.removeNetworkResources(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}

		return r.advanceDeletion(ctx, instance, valkeyv1alpha1.DeletionStageDisablingApp)
	case valkeyv1alpha1.DeletionStageDisablingApp:
		changed, err := r.disableAppForDeletion(ctx, instance)
		if err != nil || changed {
			return requeueIf(changed), err
		}

		return r.advanceDeletion(ctx, instance, valkeyv1alpha1.DeletionStageStopping)
	case valkeyv1alpha1.DeletionStageStopping:
		changed, err := r.stopStatefulSet(ctx, instance)
		if err != nil || changed {
			return requeueIf(changed), err
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

	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) disableAppForDeletion(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Spec.Slug + "-0"}
	if err := r.Get(ctx, key, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("прочитать Pod при удалении: %w", err)
	}
	if _, exists := pod.Labels[applicationRoleLabel]; exists {
		before := pod.DeepCopy()
		pod.Labels = cloneWithoutKey(pod.Labels, applicationRoleLabel)
		if err := r.Patch(ctx, pod, client.MergeFrom(before)); err != nil {
			return false, fmt.Errorf("закрыть primary Service при удалении: %w", err)
		}

		return true, nil
	}
	container := valkeyContainerStatus(pod.Status.ContainerStatuses)
	if pod.Status.PodIP == "" || container == nil || container.State.Running == nil {
		return false, nil
	}
	credentials, appHash, err := r.processCredentials(ctx, instance)
	if err != nil {
		return false, nil
	}
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
		return false, nil
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

func (r *ValkeyInstanceReconciler) stopStatefulSet(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
	statefulSet := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Spec.Slug}
	if err := r.Get(ctx, key, statefulSet); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("прочитать StatefulSet при удалении: %w", err)
	}
	if statefulSet.Spec.Replicas != nil && *statefulSet.Spec.Replicas == 0 {
		return false, nil
	}
	before := statefulSet.DeepCopy()
	replicas := int32(0)
	statefulSet.Spec.Replicas = &replicas
	if err := r.Patch(ctx, statefulSet, client.MergeFrom(before)); err != nil {
		return false, fmt.Errorf("остановить StatefulSet при удалении: %w", err)
	}

	return true, nil
}

func (r *ValkeyInstanceReconciler) verifyDeletion(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Spec.Slug + "-0"}
	if err := r.Get(ctx, key, pod); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("проверить Pod при удалении: %w", err)
		}
		if len(instance.Status.Nodes) > 0 && instance.Status.Nodes[0].Termination == nil {
			deleted, checkErr := r.previousNodeDeleted(ctx, instance.Status.Nodes[0])
			if checkErr != nil {
				return ctrl.Result{}, checkErr
			}
			if deleted {
				return r.saveNodeDeletionTermination(ctx, instance, instance.Status.Nodes[0])
			}

			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}

		return r.finishDeletion(ctx, instance)
	}
	container := valkeyContainerStatus(pod.Status.ContainerStatuses)
	if container != nil && container.State.Terminated != nil {
		return r.saveContainerTermination(ctx, instance, pod, container)
	}
	if len(instance.Status.Nodes) > 0 && instance.Status.Nodes[0].Termination != nil &&
		instance.Status.Nodes[0].PodUID == string(pod.UID) {
		return r.releaseTerminatedPod(ctx, instance, pod, instance.Status.Nodes[0])
	}

	return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
}

func (r *ValkeyInstanceReconciler) finishDeletion(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	before := instance.DeepCopy()
	instance.Finalizers = slices.DeleteFunc(instance.Finalizers, func(value string) bool {
		return value == instanceFinalizer
	})
	if err := r.Patch(ctx, instance, client.MergeFrom(before)); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("снять finalizer ValkeyInstance: %w", err)
	}

	return ctrl.Result{}, nil
}
