package operator

import (
	"context"
	"errors"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

type BusyProcessStopper func(context.Context, string, string) error

func (r *ValkeyInstanceReconciler) reconcileProcessRecovery(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	var pending ctrl.Result
	for _, process := range instance.Status.Nodes {
		if process.Termination != nil {
			continue
		}
		if process.Recovery != nil {
			if processRecoveryCanceledByResponse(process) {
				return r.clearProcessRecovery(ctx, instance, process)
			}
			thresholds := processThresholds(process.Observation, nil, r.now())
			if process.Recovery.Stage == valkeyv1alpha1.ProcessRecoveryStageKillingBusy &&
				thresholds.Unresponsive {
				return r.advanceProcessRecoveryToDeletion(ctx, instance, process)
			}
			result, err := r.continueProcessRecovery(ctx, instance, process)
			if err != nil || resultRequestsImmediateRequeue(result) {
				return result, err
			}
			pending = earlierRequeue(pending, result)
			continue
		}
		thresholds := processThresholds(process.Observation, nil, r.now())
		switch {
		case thresholds.BusyIntervention:
			return r.startProcessRecovery(
				ctx,
				instance,
				process,
				valkeyv1alpha1.ProcessRecoveryBusy,
				valkeyv1alpha1.ProcessRecoveryStageKillingBusy,
			)
		case thresholds.Unresponsive:
			eligible, err := r.processEligibleForDeletion(ctx, instance, process)
			if err != nil {
				return ctrl.Result{}, err
			}
			if eligible {
				return r.startProcessRecovery(
					ctx,
					instance,
					process,
					valkeyv1alpha1.ProcessRecoveryUnresponsive,
					valkeyv1alpha1.ProcessRecoveryStageDeleting,
				)
			}
		}
	}
	return pending, nil
}

func processRecoveryCanceledByResponse(process valkeyv1alpha1.NodeStatus) bool {
	if process.Recovery == nil || process.Observation == nil {
		return false
	}
	if process.Recovery.Reason == valkeyv1alpha1.ProcessRecoveryUnresponsive &&
		process.Recovery.Stage == valkeyv1alpha1.ProcessRecoveryStageDeleting {
		return process.Observation.Kind != valkeyv1alpha1.ProcessObservationTransportError
	}
	return process.Recovery.Reason == valkeyv1alpha1.ProcessRecoveryBusy &&
		process.Recovery.Stage == valkeyv1alpha1.ProcessRecoveryStageKillingBusy &&
		process.Observation.Kind != valkeyv1alpha1.ProcessObservationBusy &&
		process.Observation.Kind != valkeyv1alpha1.ProcessObservationTransportError
}

func (r *ValkeyInstanceReconciler) advanceProcessRecoveryToDeletion(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		for index := range status.Nodes {
			if sameProcess(status.Nodes[index], process) && status.Nodes[index].Recovery != nil {
				status.Nodes[index].Recovery.Stage = valkeyv1alpha1.ProcessRecoveryStageDeleting
			}
		}
	})
	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) startProcessRecovery(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
	reason valkeyv1alpha1.ProcessRecoveryReason,
	stage valkeyv1alpha1.ProcessRecoveryStage,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		for index := range status.Nodes {
			if sameProcess(status.Nodes[index], process) && status.Nodes[index].Recovery == nil {
				status.Nodes[index].Recovery = &valkeyv1alpha1.ProcessRecoveryStatus{
					Reason: reason, Stage: stage, StartedAt: metav1.NewTime(r.now()),
				}
			}
		}
	})
	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) continueProcessRecovery(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) (ctrl.Result, error) {
	switch process.Recovery.Stage {
	case valkeyv1alpha1.ProcessRecoveryStageKillingBusy:
		return r.recoverBusyProcess(ctx, instance, process)
	case valkeyv1alpha1.ProcessRecoveryStageDeleting:
		return r.requestProcessDeletion(ctx, instance, process)
	case valkeyv1alpha1.ProcessRecoveryStageWaitingForTermination:
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	default:
		return ctrl.Result{}, fmt.Errorf("неизвестная стадия восстановления процесса %q", process.Recovery.Stage)
	}
}

func (r *ValkeyInstanceReconciler) recoverBusyProcess(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) (ctrl.Result, error) {
	pod, err := r.currentProcessPod(ctx, instance, process)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}
		return ctrl.Result{}, err
	}
	credentials, _, err := r.processCredentials(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	stop := r.StopBusyProcess
	if stop == nil {
		stop = r.stopBusyProcess
	}
	err = stop(ctx, net.JoinHostPort(pod.Status.PodIP, "6379"), string(credentials.OperatorPassword))
	switch {
	case err == nil, errors.Is(err, operatorvalkey.ErrNoBusyProcess):
		return r.clearProcessRecovery(ctx, instance, process)
	case errors.Is(err, operatorvalkey.ErrUnkillable):
		return r.advanceProcessRecoveryToDeletion(ctx, instance, process)
	case operatorvalkey.ClassifyError(err) == operatorvalkey.ErrorKindTransport:
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	default:
		return ctrl.Result{}, err
	}
}

func (r *ValkeyInstanceReconciler) stopBusyProcess(
	ctx context.Context,
	address string,
	operatorPassword string,
) error {
	connection, err := r.dialValkey(ctx, operatorvalkey.ClientConfig{
		Address: address, Username: "operator", Password: operatorPassword,
		DialTimeout: config.ValkeyDialTimeout, CommandTimeout: config.ValkeyCommandTimeout,
	})
	if err != nil {
		return err
	}
	defer connection.Close()
	session, err := connection.OpenSession(ctx)
	if err != nil {
		return err
	}
	defer session.Close()
	return session.StopBusy(ctx, operatorPassword)
}

func (r *ValkeyInstanceReconciler) requestProcessDeletion(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) (ctrl.Result, error) {
	eligible, err := r.processEligibleForDeletion(ctx, instance, process)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !eligible {
		return r.clearProcessRecovery(ctx, instance, process)
	}
	pod, err := r.currentProcessPod(ctx, instance, process)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}
		return ctrl.Result{}, err
	}
	if pod.DeletionTimestamp.IsZero() {
		if err := runProcessActionControl(ctx, "request-delete", pod, process); err != nil {
			return ctrl.Result{}, err
		}
		uid := pod.UID
		resourceVersion := pod.ResourceVersion
		err = r.Delete(ctx, pod, &client.DeleteOptions{
			GracePeriodSeconds: ptr.To(config.ProcessDeletionGracePeriod),
			Preconditions:      &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("удалить зависший Pod: %w", err)
		}
	}
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		for index := range status.Nodes {
			if sameProcess(status.Nodes[index], process) && status.Nodes[index].Recovery != nil {
				status.Nodes[index].Recovery.Stage = valkeyv1alpha1.ProcessRecoveryStageWaitingForTermination
				requestedAt := metav1.NewTime(r.now())
				status.Nodes[index].Recovery.DeleteRequestedAt = &requestedAt
			}
		}
	})
	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) processEligibleForDeletion(
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
	if podReady(pod.Status.Conditions) && pod.DeletionTimestamp.IsZero() {
		return false, nil
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	node := &corev1.Node{}
	if err := reader.Get(ctx, client.ObjectKey{Name: process.NodeName}, node); err != nil {
		return false, err
	}
	return string(node.UID) == process.NodeUID && nodeReady(node.Status.Conditions), nil
}

func nodeReady(conditions []corev1.NodeCondition) bool {
	for _, condition := range conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (r *ValkeyInstanceReconciler) clearProcessRecovery(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		for index := range status.Nodes {
			if sameProcess(status.Nodes[index], process) {
				status.Nodes[index].Recovery = nil
			}
		}
	})
	return requeueIf(changed), err
}
