package operator

import (
	"context"
	"fmt"
	"net"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

const conditionTypeCredentialRotation = "CredentialRotation"

type PasswordRotator func(
	context.Context,
	string,
	string,
	string,
) (operatorvalkey.ProcessState, error)

type passwordRotationProgress struct {
	result   ctrl.Result
	complete bool
	blocked  bool
}

func (r *ValkeyInstanceReconciler) reconcileCredentialRotation(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	rotation := instance.Status.CredentialRotation
	if rotation == nil || instance.Status.Failover != nil {
		return ctrl.Result{}, nil
	}

	switch rotation.Stage {
	case valkeyv1alpha1.CredentialRotationStagePreparing:
		return r.advanceCredentialRotation(
			ctx,
			instance,
			valkeyv1alpha1.CredentialRotationStageUpdatingReplicas,
		)
	case valkeyv1alpha1.CredentialRotationStageUpdatingReplicas:
		progress, err := r.rotatePasswordForRole(ctx, instance, false)
		if err != nil || !progress.result.IsZero() {
			return progress.result, err
		}
		if progress.blocked && rotationRoleComplete(instance, true) {
			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}
		return r.advanceCredentialRotation(
			ctx,
			instance,
			valkeyv1alpha1.CredentialRotationStageUpdatingPrimary,
		)
	case valkeyv1alpha1.CredentialRotationStageUpdatingPrimary:
		progress, err := r.rotatePasswordForRole(ctx, instance, true)
		if err != nil || !progress.result.IsZero() {
			return progress.result, err
		}
		if !progress.complete {
			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}
		if !rotationRoleComplete(instance, false) {
			return r.advanceCredentialRotation(
				ctx,
				instance,
				valkeyv1alpha1.CredentialRotationStageUpdatingReplicas,
			)
		}
		return r.advanceCredentialRotation(
			ctx,
			instance,
			valkeyv1alpha1.CredentialRotationStageCleaningSecret,
		)
	case valkeyv1alpha1.CredentialRotationStageCleaningSecret:
		return r.finishCredentialRotation(ctx, instance)
	default:
		return ctrl.Result{}, fmt.Errorf("неизвестная стадия credentialRotation %q", rotation.Stage)
	}
}

func (r *ValkeyInstanceReconciler) rotatePasswordForRole(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	primary bool,
) (passwordRotationProgress, error) {
	rotation := instance.Status.CredentialRotation
	blocked := false
	for _, process := range rotationProcesses(instance, primary) {
		if process.Termination != nil {
			continue
		}
		if rotationConfirmed(rotation, process) {
			continue
		}
		if process.Observation != nil {
			blocked = true
			continue
		}
		pod, err := r.currentProcessPod(ctx, instance, process)
		if err != nil {
			if apierrors.IsNotFound(err) {
				blocked = true
				continue
			}
			return passwordRotationProgress{}, err
		}
		credentials, appHash, err := r.processCredentials(ctx, instance)
		if err != nil {
			return passwordRotationProgress{}, err
		}
		rotate := r.RotatePassword
		if rotate == nil {
			rotate = r.rotateProcessPassword
		}
		state, err := rotate(
			ctx,
			net.JoinHostPort(pod.Status.PodIP, "6379"),
			string(credentials.OperatorPassword),
			appHash,
		)
		if err != nil {
			if operatorvalkey.ClassifyError(err) == operatorvalkey.ErrorKindTransport {
				blocked = true
				continue
			}
			return passwordRotationProgress{}, err
		}
		if err := runProcessActionControl(ctx, "password-rotation-applied", pod, process); err != nil {
			return passwordRotationProgress{}, err
		}
		if state.RunID != process.RunID || state.AppEnabled != process.AppEnabled ||
			!appACLMatches(state.AppPasswordHashes, appHash) {
			blocked = true
			continue
		}
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			if status.CredentialRotation == nil ||
				status.CredentialRotation.TargetVersion != rotation.TargetVersion {
				return
			}
			confirmation := valkeyv1alpha1.CredentialRotationConfirmation{
				Process: processIdentity(process), Version: rotation.TargetVersion,
			}
			status.CredentialRotation.Confirmations = append(
				status.CredentialRotation.Confirmations,
				confirmation,
			)
			for index := range status.Nodes {
				if sameProcess(status.Nodes[index], process) {
					status.Nodes[index].AppPasswordVersion = rotation.TargetVersion
				}
			}
		})
		if err == nil && changed {
			err = runCredentialRotationActionControl(ctx, "process-confirmed", instance, process)
		}
		return passwordRotationProgress{result: requeueIf(changed), blocked: blocked}, err
	}

	return passwordRotationProgress{complete: !blocked, blocked: blocked}, nil
}

func rotationRoleComplete(instance *valkeyv1alpha1.ValkeyInstance, primary bool) bool {
	rotation := instance.Status.CredentialRotation
	for _, process := range rotationProcesses(instance, primary) {
		if process.Termination == nil && !rotationConfirmed(rotation, process) {
			return false
		}
	}

	return true
}

func rotationProcesses(
	instance *valkeyv1alpha1.ValkeyInstance,
	primary bool,
) []valkeyv1alpha1.NodeStatus {
	result := make([]valkeyv1alpha1.NodeStatus, 0, len(instance.Status.Nodes)+len(instance.Status.PreviousProcesses))
	for _, process := range append(slices.Clone(instance.Status.Nodes), instance.Status.PreviousProcesses...) {
		isPrimary := instance.Status.PrimaryOrdinal != nil &&
			*instance.Status.PrimaryOrdinal == process.Ordinal && primaryIdentityMatches(instance, process)
		if isPrimary == primary {
			result = append(result, process)
		}
	}
	slices.SortFunc(result, func(left, right valkeyv1alpha1.NodeStatus) int {
		return int(left.Ordinal - right.Ordinal)
	})
	return result
}

func rotationConfirmed(
	rotation *valkeyv1alpha1.CredentialRotationStatus,
	process valkeyv1alpha1.NodeStatus,
) bool {
	return slices.ContainsFunc(
		rotation.Confirmations,
		func(confirmation valkeyv1alpha1.CredentialRotationConfirmation) bool {
			return confirmation.Version == rotation.TargetVersion &&
				sameIdentity(confirmation.Process, processIdentity(process))
		},
	)
}

func (r *ValkeyInstanceReconciler) rotateProcessPassword(
	ctx context.Context,
	address string,
	operatorPassword string,
	appHash string,
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
	before, err := session.TakeControl(ctx, operatorPassword)
	if err != nil {
		return operatorvalkey.ProcessState{}, err
	}
	if err := session.RotateAppPassword(ctx, appHash); err != nil {
		return before, err
	}
	if err := session.KillAppClients(ctx); err != nil {
		return before, err
	}
	after, err := session.TakeControl(ctx, operatorPassword)
	if err != nil {
		return before, err
	}
	if after.AppEnabled != before.AppEnabled {
		return after, errAppAdmissionIncomplete
	}
	return after, nil
}

func (r *ValkeyInstanceReconciler) advanceCredentialRotation(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	stage valkeyv1alpha1.CredentialRotationStage,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		if status.CredentialRotation != nil {
			status.CredentialRotation.Stage = stage
			status.Phase = valkeyv1alpha1.InstancePhaseUpdating
			status.Reason = "PASSWORD_ROTATION"
			setCondition(
				instance,
				status,
				conditionTypeCredentialRotation,
				metav1.ConditionFalse,
				string(stage),
				"смена пароля продолжается",
			)
		}
	})
	if err == nil && changed {
		err = runCredentialRotationActionControl(
			ctx,
			"stage-saved",
			instance,
			valkeyv1alpha1.NodeStatus{},
		)
	}
	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) finishCredentialRotation(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	rotation := instance.Status.CredentialRotation
	if rotation == nil {
		return ctrl.Result{}, nil
	}
	previousKey, err := valkeyv1alpha1.AppPasswordHashKey(rotation.PreviousVersion)
	if err != nil {
		return ctrl.Result{}, err
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	secretChanged := false
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret := &corev1.Secret{}
		key := client.ObjectKey{
			Namespace: instance.Namespace,
			Name:      valkeyv1alpha1.AuthSecretName(instance.Status.AcceptedConfiguration.Slug),
		}
		if err := reader.Get(ctx, key, secret); err != nil {
			return err
		}
		if _, found := secret.Data[previousKey]; !found {
			return nil
		}
		before := secret.DeepCopy()
		secret.Data = make(map[string][]byte, len(before.Data)-1)
		for name, value := range before.Data {
			if name != previousKey {
				secret.Data[name] = slices.Clone(value)
			}
		}
		if err := r.Patch(
			ctx,
			secret,
			client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}),
		); err != nil {
			return err
		}
		secretChanged = true
		return nil
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("очистить прежний хеш пароля: %w", err)
	}
	if secretChanged {
		if err := runCredentialRotationActionControl(
			ctx,
			"secret-cleaned",
			instance,
			valkeyv1alpha1.NodeStatus{},
		); err != nil {
			return ctrl.Result{}, err
		}
		return requeueIf(true), nil
	}

	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		if status.CredentialRotation == nil || status.CredentialRotation.TargetVersion != rotation.TargetVersion {
			return
		}
		status.AppliedPasswordVersion = rotation.TargetVersion
		status.CredentialRotation = nil
		setCondition(
			instance,
			status,
			conditionTypeCredentialRotation,
			metav1.ConditionTrue,
			"Completed",
			"смена пароля завершена",
		)
		applyOperationalPhase(instance, status)
	})
	return requeueIf(changed), err
}
