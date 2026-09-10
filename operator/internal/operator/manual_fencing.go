package operator

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

const manualFencingAnnotation = "valkey.h3llo-demo.com/manual-fencing"

const conditionTypeManualFencing = "ManualFencing"

func (r *ValkeyInstanceReconciler) reconcileManualFencing(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	value, found := instance.Annotations[manualFencingAnnotation]
	if !found {
		return ctrl.Result{}, nil
	}
	identity := valkeyv1alpha1.ProcessIdentity{}
	if err := json.Unmarshal([]byte(value), &identity); err != nil {
		return ctrl.Result{}, nil
	}
	process, found := knownProcess(instance.Status, identity)
	if !found {
		return ctrl.Result{}, nil
	}
	if process.Termination != nil {
		return r.removeManualFencingAnnotation(ctx, instance)
	}
	if !manualFencingExpected(instance, process) {
		return ctrl.Result{}, nil
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	node := &corev1.Node{}
	err := reader.Get(ctx, client.ObjectKey{Name: process.NodeName}, node)
	if err == nil && string(node.UID) == process.NodeUID && nodeReady(node.Status.Conditions) {
		return r.manualFencingPending(ctx, instance, process)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("проверить Node ручного fencing: %w", err)
	}

	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		termination := &valkeyv1alpha1.ProcessTermination{
			Reason: "ManualFencing", FinishedAt: metav1.NewTime(r.now()), Evidence: "manual_fencing",
		}
		for index := range status.Nodes {
			if sameIdentity(processIdentity(status.Nodes[index]), identity) &&
				status.Nodes[index].Termination == nil {
				status.Nodes[index].Readiness = false
				status.Nodes[index].Termination = termination.DeepCopy()
			}
		}
		for index := range status.PreviousProcesses {
			if sameIdentity(processIdentity(status.PreviousProcesses[index]), identity) &&
				status.PreviousProcesses[index].Termination == nil {
				status.PreviousProcesses[index].Readiness = false
				status.PreviousProcesses[index].Termination = termination.DeepCopy()
			}
		}
		apimeta.RemoveStatusCondition(&status.Conditions, conditionTypeManualFencing)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		if r.Recorder != nil {
			r.Recorder.Eventf(
				instance,
				nil,
				corev1.EventTypeWarning,
				"ManualFencingAccepted",
				"Fencing",
				"ручное fencing принято для процесса ordinal %d на Node %s",
				process.Ordinal,
				process.NodeName,
			)
		}
		return requeueIf(changed), err
	}
	return r.removeManualFencingAnnotation(ctx, instance)
}

func (r *ValkeyInstanceReconciler) manualFencingPending(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		setCondition(
			instance,
			status,
			conditionTypeManualFencing,
			metav1.ConditionFalse,
			"NodeStillReady",
			"ручное fencing ждёт остановки указанной ноды",
		)
	})
	if err == nil && changed && r.Recorder != nil {
		r.Recorder.Eventf(
			instance,
			nil,
			corev1.EventTypeWarning,
			"ManualFencingPending",
			"Fencing",
			"ручное fencing процесса ordinal %d ждёт остановки Node %s",
			process.Ordinal,
			process.NodeName,
		)
	}
	return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, err
}

func (r *ValkeyInstanceReconciler) removeManualFencingAnnotation(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	before := instance.DeepCopy()
	instance.Annotations = cloneWithoutKey(instance.Annotations, manualFencingAnnotation)
	if err := r.Patch(ctx, instance, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, fmt.Errorf("удалить принятое ручное fencing: %w", err)
	}
	return requeueIf(true), nil
}

func manualFencingExpected(
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) bool {
	if instance.Status.Deletion != nil || process.Recovery != nil {
		return true
	}
	if instance.Status.Failover != nil {
		if sameIdentity(instance.Status.Failover.Source, processIdentity(process)) {
			return true
		}
		if instance.Status.Failover.Candidate != nil &&
			sameIdentity(*instance.Status.Failover.Candidate, processIdentity(process)) {
			return true
		}
	}
	if instance.Status.Rollout != nil && instance.Status.Rollout.Process != nil &&
		sameIdentity(*instance.Status.Rollout.Process, processIdentity(process)) {
		return true
	}
	for _, previous := range instance.Status.PreviousProcesses {
		if previous.Termination == nil && sameProcess(previous, process) {
			return true
		}
	}
	return false
}
