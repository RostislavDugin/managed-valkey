package operator

import (
	"context"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
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

const (
	conditionTypeRollout  = "Rollout"
	conditionTypeDataLoss = "DataLoss"
)

func (r *ValkeyInstanceReconciler) reconcileRollout(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	rollout := instance.Status.Rollout
	if rollout == nil || instance.Status.Failover != nil {
		return ctrl.Result{}, nil
	}

	switch rollout.Stage {
	case valkeyv1alpha1.RolloutStagePreparing:
		return r.prepareRollout(ctx, instance)
	case valkeyv1alpha1.RolloutStageStopping:
		return r.reconcileStoppedRollout(ctx, instance)
	case valkeyv1alpha1.RolloutStageUpdatingTemplate:
		return r.reconcileRolloutTemplate(ctx, instance)
	case valkeyv1alpha1.RolloutStageReplacingReplicas:
		return r.reconcileRollingReplacement(ctx, instance, valkeyv1alpha1.RolloutStageSwitchingPrimary)
	case valkeyv1alpha1.RolloutStageSwitchingPrimary:
		return r.reconcileRolloutPrimarySwitch(ctx, instance)
	case valkeyv1alpha1.RolloutStageReplacingPrimary:
		return r.reconcileRollingReplacement(ctx, instance, valkeyv1alpha1.RolloutStageVerifying)
	case valkeyv1alpha1.RolloutStageStarting:
		ready, err := rolloutCompositionReady(ctx, r, instance)
		if err != nil || !ready {
			if err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		return r.advanceRollout(ctx, instance, valkeyv1alpha1.RolloutStageVerifying)
	case valkeyv1alpha1.RolloutStageVerifying:
		ready, err := rolloutCompositionReady(ctx, r, instance)
		if err != nil || !ready {
			if err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}
		return r.finishRollout(ctx, instance)
	default:
		return ctrl.Result{}, fmt.Errorf("неизвестная стадия rollout %q", rollout.Stage)
	}
}

func (r *ValkeyInstanceReconciler) prepareRollout(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	statefulSet := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.AcceptedConfiguration.Slug}
	if err := r.Get(ctx, key, statefulSet); err != nil {
		return ctrl.Result{}, fmt.Errorf("прочитать StatefulSet перед rollout: %w", err)
	}
	image := valkeyContainerImage(statefulSet.Spec.Template.Spec.Containers)
	if image == "" {
		return ctrl.Result{}, fmt.Errorf("образ Valkey в StatefulSet отсутствует")
	}
	stage := valkeyv1alpha1.RolloutStageUpdatingTemplate
	if rolloutRequiresFullStop(instance) {
		stage = valkeyv1alpha1.RolloutStageStopping
	}
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		if status.Rollout != nil {
			status.Rollout.Image = image
			status.Rollout.Stage = stage
			status.Phase = valkeyv1alpha1.InstancePhaseUpdating
			status.Reason = "ROLLOUT_" + string(stage)
			setCondition(
				instance,
				status,
				conditionTypeRollout,
				metav1.ConditionFalse,
				string(stage),
				"ресайз продолжается",
			)
		}
	})
	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) reconcileStoppedRollout(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	if !instance.Status.Rollout.AccessClosed {
		closed, changed, err := r.closeAllProcessesForRollout(ctx, instance)
		if err != nil || changed || !closed {
			if err != nil || changed {
				return requeueIf(changed), err
			}
			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}
		changed, err = r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			if status.Rollout != nil {
				status.Rollout.AccessClosed = true
			}
		})
		return requeueIf(changed), err
	}

	stopped, err := r.allRolloutProcessesStopped(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !stopped {
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		if status.Rollout == nil {
			return
		}
		status.Rollout.Stage = valkeyv1alpha1.RolloutStageUpdatingTemplate
		status.PrimaryOrdinal = nil
		status.PrimaryPodUID = ""
		status.PrimaryContainerID = ""
		status.PrimaryRunID = ""
		status.PrimaryNodeName = ""
		status.PrimaryNodeUID = ""
		setCondition(
			instance,
			status,
			conditionTypePublicReady,
			metav1.ConditionFalse,
			"RolloutStopped",
			"процессы остановлены для ресайза",
		)
		setCondition(
			instance,
			status,
			conditionTypeDataLoss,
			metav1.ConditionTrue,
			"EmptyRestart",
			"ресайз запускает пустой кэш",
		)
	})
	if changed && r.Recorder != nil {
		r.Recorder.Eventf(instance, nil, corev1.EventTypeWarning, "CacheEmptied", "Resize", "кэш очищен при ресайзе")
	}
	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) closeAllProcessesForRollout(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, bool, error) {
	for _, process := range instance.Status.Nodes {
		if process.Termination != nil {
			continue
		}
		changed, err := r.removeProcessRoleLabel(ctx, instance, process)
		if err != nil || changed {
			return false, changed, err
		}
		if process.Observation != nil || !process.AppEnabled &&
			process.AppPasswordVersion == instance.Status.AcceptedConfiguration.PasswordVersion {
			continue
		}
		fenced, err := r.fenceProcess(ctx, instance, process)
		if err != nil {
			if operatorvalkey.ClassifyError(err) == operatorvalkey.ErrorKindTransport {
				continue
			}
			return false, false, err
		}
		if !fenced {
			return false, false, nil
		}
		return false, true, nil
	}
	return true, false, nil
}

func (r *ValkeyInstanceReconciler) allRolloutProcessesStopped(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
	for _, process := range append(slices.Clone(instance.Status.Nodes), instance.Status.PreviousProcesses...) {
		if process.Termination == nil {
			return false, nil
		}
	}
	for ordinal := range expectedProcessCount(instance) {
		pod := &corev1.Pod{}
		key := client.ObjectKey{
			Namespace: instance.Namespace,
			Name:      fmt.Sprintf("%s-%d", instance.Status.AcceptedConfiguration.Slug, ordinal),
		}
		if err := r.Get(ctx, key, pod); err == nil {
			return false, nil
		} else if !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return true, nil
}

func (r *ValkeyInstanceReconciler) reconcileRolloutTemplate(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	statefulSet := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.AcceptedConfiguration.Slug}
	if err := r.Get(ctx, key, statefulSet); err != nil {
		return ctrl.Result{}, err
	}
	if !statefulSetTemplateMatchesRollout(statefulSet, instance) {
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}
	next := valkeyv1alpha1.RolloutStageReplacingReplicas
	if rolloutRequiresFullStop(instance) {
		next = valkeyv1alpha1.RolloutStageStarting
	}
	if err := runRolloutStatusActionControl(ctx, "rollout-template-applied", instance); err != nil {
		return ctrl.Result{}, err
	}
	return r.advanceRollout(ctx, instance, next)
}

func (r *ValkeyInstanceReconciler) reconcileRollingReplacement(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	next valkeyv1alpha1.RolloutStage,
) (ctrl.Result, error) {
	rollout := instance.Status.Rollout
	if rollout.Process != nil {
		old, found := knownProcess(instance.Status, *rollout.Process)
		if !found {
			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}
		if old.Termination == nil {
			changed, err := r.removeProcessRoleLabel(ctx, instance, old)
			if err != nil || changed {
				return requeueIf(changed), err
			}
			if old.AppEnabled || old.AppPasswordVersion != instance.Status.AcceptedConfiguration.PasswordVersion {
				fenced, err := r.fenceProcess(ctx, instance, old)
				if err != nil {
					if operatorvalkey.ClassifyError(err) == operatorvalkey.ErrorKindTransport {
						return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
					}
					return ctrl.Result{}, err
				}
				if !fenced {
					return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
				}
				return requeueIf(true), nil
			}
			return r.deleteRolloutPod(ctx, instance, old)
		}
		current, found := nodeStatusAtOrdinal(instance.Status.Nodes, old.Ordinal)
		if !found || sameProcess(current, old) {
			return ctrl.Result{}, nil
		}
		ready, err := r.rolloutProcessReady(ctx, instance, current)
		if err != nil || !ready {
			return ctrl.Result{}, err
		}
		pod, err := r.currentProcessPod(ctx, instance, current)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
			}
			return ctrl.Result{}, err
		}
		if err := runProcessActionControl(ctx, "rollout-replacement-ready", pod, current); err != nil {
			return ctrl.Result{}, err
		}
		replacedProcess := *rollout.Process
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			if status.Rollout != nil && status.Rollout.Process != nil &&
				sameIdentity(*status.Rollout.Process, replacedProcess) {
				status.Rollout.Process = nil
			}
		})
		return requeueIf(changed), err
	}

	for _, process := range instance.Status.Nodes {
		if instance.Status.PrimaryOrdinal != nil && process.Ordinal == *instance.Status.PrimaryOrdinal {
			continue
		}
		if process.Termination != nil {
			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}
		matches, err := r.rolloutPodMatches(ctx, instance, process)
		if err != nil {
			return ctrl.Result{}, err
		}
		if matches {
			ready, err := r.rolloutProcessReady(ctx, instance, process)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !ready {
				return ctrl.Result{}, nil
			}
			continue
		}
		identity := processIdentity(process)
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			if status.Rollout != nil && status.Rollout.Process == nil {
				status.Rollout.Process = &identity
			}
		})
		return requeueIf(changed), err
	}

	return r.advanceRollout(ctx, instance, next)
}

func (r *ValkeyInstanceReconciler) deleteRolloutPod(
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
	if !pod.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if err := runProcessActionControl(ctx, "rollout-request-delete", pod, process); err != nil {
		return ctrl.Result{}, err
	}
	deleting, err := r.rolloutDeletionRequested(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	if deleting {
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}
	uid := pod.UID
	resourceVersion := pod.ResourceVersion
	err = r.Delete(ctx, pod, &client.DeleteOptions{
		GracePeriodSeconds: ptr.To(config.ProcessDeletionGracePeriod),
		Preconditions:      &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("удалить Pod для rollout: %w", err)
	}
	if err := runProcessActionControl(ctx, "rollout-delete-accepted", pod, process); err != nil {
		return ctrl.Result{}, err
	}
	return requeueIf(true), nil
}

func (r *ValkeyInstanceReconciler) rolloutDeletionRequested(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	current := &valkeyv1alpha1.ValkeyInstance{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(instance), current); err != nil {
		return false, fmt.Errorf("проверить удаление инстанса перед DELETE Pod: %w", err)
	}
	return !current.DeletionTimestamp.IsZero(), nil
}

func (r *ValkeyInstanceReconciler) reconcileRolloutPrimarySwitch(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	if instance.Status.PrimaryOrdinal == nil {
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}
	primary, found := nodeStatusAtOrdinal(instance.Status.Nodes, *instance.Status.PrimaryOrdinal)
	if !found {
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}
	matches, err := r.rolloutPodMatches(ctx, instance, primary)
	if err != nil {
		return ctrl.Result{}, err
	}
	if matches {
		return r.advanceRollout(ctx, instance, valkeyv1alpha1.RolloutStageReplacingPrimary)
	}
	candidates := make([]valkeyv1alpha1.NodeStatus, 0, len(instance.Status.Nodes))
	for _, process := range instance.Status.Nodes {
		ready, err := r.rolloutProcessReady(ctx, instance, process)
		if err != nil {
			return ctrl.Result{}, err
		}
		if ready {
			candidates = append(candidates, process)
		}
	}
	candidate, found := chooseFailoverCandidate(primary, candidates)
	if !found {
		return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
	}
	identity := processIdentity(candidate)
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		if status.Failover != nil || status.Rollout == nil {
			return
		}
		status.Failover = &valkeyv1alpha1.FailoverStatus{
			Reason: valkeyv1alpha1.FailoverReasonResize, Stage: valkeyv1alpha1.FailoverStageFencing,
			StartedAt: metav1.NewTime(r.now()), Source: processIdentity(primary), Candidate: &identity,
		}
		status.Phase = valkeyv1alpha1.InstancePhaseUpdating
		status.Reason = "ROLLOUT_SWITCHING_PRIMARY"
	})
	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) rolloutProcessReady(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) (bool, error) {
	if process.Termination != nil || process.Observation != nil || !process.Readiness || !process.AppEnabled ||
		process.AppPasswordVersion != instance.Status.AcceptedConfiguration.PasswordVersion {
		return false, nil
	}
	if process.Role == valkeyv1alpha1.NodeRoleReplica &&
		(process.Replication == nil || !process.Replication.LinkUp || process.Replication.SyncInProgress ||
			process.Replication.SyncedAt == nil) {
		return false, nil
	}
	return r.rolloutPodMatches(ctx, instance, process)
}

func (r *ValkeyInstanceReconciler) rolloutPodMatches(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
) (bool, error) {
	pod, err := r.currentProcessPod(ctx, instance, process)
	if err != nil {
		return false, err
	}
	return podTemplateMatchesRollout(&pod.Spec, instance), nil
}

func rolloutCompositionReady(
	ctx context.Context,
	r *ValkeyInstanceReconciler,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
	if !instance.Status.Initialized || !primaryPubliclyReady(&instance.Status) {
		return false, nil
	}
	if len(instance.Status.Nodes) != int(expectedProcessCount(instance)) {
		return false, nil
	}
	for _, process := range instance.Status.Nodes {
		ready, err := r.rolloutProcessReady(ctx, instance, process)
		if err != nil || !ready {
			return false, err
		}
	}
	return instance.Status.AcceptedConfiguration.Mode != valkeyv1alpha1.ValkeyModeHA ||
		readyReplicaCount(&instance.Status) == 2, nil
}

func statefulSetTemplateMatchesRollout(
	statefulSet *appsv1.StatefulSet,
	instance *valkeyv1alpha1.ValkeyInstance,
) bool {
	return podTemplateMatchesRollout(&statefulSet.Spec.Template.Spec, instance)
}

func podTemplateMatchesRollout(
	podSpec *corev1.PodSpec,
	instance *valkeyv1alpha1.ValkeyInstance,
) bool {
	rollout := instance.Status.Rollout
	if rollout == nil {
		return false
	}
	configMapName := rolloutConfigMapName(instance)
	configMatches := slices.ContainsFunc(podSpec.Volumes, func(volume corev1.Volume) bool {
		return volume.Name == "config" && volume.ConfigMap != nil && volume.ConfigMap.Name == configMapName
	})
	for _, container := range podSpec.Containers {
		if container.Name != "valkey" {
			continue
		}
		cpu := container.Resources.Requests.Cpu()
		memory := container.Resources.Requests.Memory()
		return configMatches && container.Image == rollout.Image && cpu != nil && memory != nil &&
			cpu.CmpInt64(int64(rollout.VCPU)) == 0 &&
			memory.CmpInt64(int64(rollout.RAMGB)*gibibyte) == 0
	}
	return false
}

func rolloutConfigMapName(instance *valkeyv1alpha1.ValkeyInstance) string {
	accepted := *instance.Status.AcceptedConfiguration
	accepted.VCPU = instance.Status.Rollout.VCPU
	accepted.RAMGB = instance.Status.Rollout.RAMGB
	data := configMapData(accepted)
	return accepted.Slug + "-config-" + configDigest(data)
}

func valkeyContainerImage(containers []corev1.Container) string {
	for _, container := range containers {
		if container.Name == "valkey" {
			return container.Image
		}
	}
	return ""
}

func (r *ValkeyInstanceReconciler) advanceRollout(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	stage valkeyv1alpha1.RolloutStage,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		if status.Rollout != nil {
			status.Rollout.Stage = stage
			status.Rollout.Process = nil
			status.Phase = valkeyv1alpha1.InstancePhaseUpdating
			status.Reason = "ROLLOUT_" + string(stage)
			setCondition(
				instance,
				status,
				conditionTypeRollout,
				metav1.ConditionFalse,
				string(stage),
				"ресайз продолжается",
			)
		}
	})
	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) finishRollout(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	if err := runRolloutStatusActionControl(ctx, "before-rollout-applied", instance); err != nil {
		return ctrl.Result{}, err
	}
	rollout := instance.Status.Rollout
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		if status.Rollout == nil || status.Rollout.DesiredGeneration != rollout.DesiredGeneration {
			return
		}
		status.Applied = &valkeyv1alpha1.AppliedConfiguration{
			Mode: status.AcceptedConfiguration.Mode, VCPU: rollout.VCPU, RAMGB: rollout.RAMGB,
		}
		status.Rollout = nil
		setCondition(instance, status, conditionTypeRollout, metav1.ConditionTrue, "Completed", "ресайз завершён")
		applyOperationalPhase(instance, status)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		if err := runRolloutStatusActionControl(ctx, "after-rollout-applied", instance); err != nil {
			return ctrl.Result{}, err
		}
	}
	return requeueIf(changed), err
}
