package operator

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	valkeyclient "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

const (
	conditionTypeCredentialsReady = "CredentialsReady"
	conditionTypeRecoveryRequired = "RecoveryRequired"
)

func (r *ValkeyInstanceReconciler) reconcileCredentials(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: instance.Namespace, Name: valkeyv1alpha1.AuthSecretName(instance.Spec.Slug)}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return r.credentialsFailure(ctx, instance, "SecretNotFound", instance.Status.CredentialsInitialized)
		}

		return ctrl.Result{}, fmt.Errorf("прочитать Secret учётных данных: %w", err)
	}
	if secret.Type != corev1.SecretTypeOpaque {
		return r.credentialsFailure(ctx, instance, "SecretTypeInvalid", instance.Status.CredentialsInitialized)
	}

	accepted := instance.Status.AcceptedConfiguration
	appHash, err := valkeyv1alpha1.ParseAppPasswordHash(secret.Data, accepted.PasswordVersion)
	if err != nil {
		return r.credentialsFailure(ctx, instance, credentialFailureReason(err), instance.Status.CredentialsInitialized)
	}

	credentials, err := valkeyv1alpha1.ParseServiceCredentials(secret.Data)
	if err != nil {
		return r.credentialsFailure(ctx, instance, credentialFailureReason(err), instance.Status.CredentialsInitialized)
	}

	if credentials == nil {
		if instance.Status.CredentialsInitialized {
			return r.credentialsFailure(ctx, instance, "ServiceCredentialsMissing", true)
		}

		credentials, err = newServiceCredentials(appHash)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	expectedACL := valkeyclient.InitialACL(
		appHash,
		string(credentials.OperatorPassword),
		string(credentials.ReplicaPassword),
		string(credentials.HealthPassword),
	)
	if len(secret.Data[valkeyv1alpha1.UsersACLKey]) > 0 &&
		!bytes.Equal(secret.Data[valkeyv1alpha1.UsersACLKey], expectedACL) &&
		instance.Status.CredentialsInitialized {
		return r.credentialsFailure(ctx, instance, "ACLInvalid", true)
	}
	credentials.UsersACL = expectedACL

	changed, err := updateCredentialsSecret(instance, secret, credentials, r.Scheme)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		if err := r.Update(ctx, secret); err != nil {
			return ctrl.Result{}, fmt.Errorf("обновить Secret учётных данных: %w", err)
		}
	}

	statusChanged, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		status.CredentialsInitialized = true
		setCondition(
			instance,
			status,
			conditionTypeCredentialsReady,
			metav1.ConditionTrue,
			"Initialized",
			"служебные учётные данные сохранены",
		)
		condition := apimeta.FindStatusCondition(status.Conditions, conditionTypeRecoveryRequired)
		if condition != nil && credentialRecoveryReason(condition.Reason) {
			apimeta.RemoveStatusCondition(&status.Conditions, conditionTypeRecoveryRequired)
		}
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	return requeueIf(changed || statusChanged), nil
}

func credentialRecoveryReason(reason string) bool {
	switch reason {
	case "SecretNotFound", "SecretTypeInvalid", "AppPasswordHashMissing", "AppPasswordHashInvalid",
		"ServiceCredentialsMissing", "ServiceCredentialsPartial", "ServiceCredentialsInvalid", "ACLInvalid":
		return true
	default:
		return false
	}
}

func (r *ValkeyInstanceReconciler) credentialsFailure(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	reason string,
	recoveryRequired bool,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		setCondition(
			instance,
			status,
			conditionTypeCredentialsReady,
			metav1.ConditionFalse,
			reason,
			"служебные учётные данные недоступны",
		)
		if recoveryRequired {
			setCondition(
				instance,
				status,
				conditionTypeRecoveryRequired,
				metav1.ConditionTrue,
				reason,
				"требуется ручное восстановление Secret",
			)
		}
	})
	if changed && recoveryRequired && r.Recorder != nil {
		r.Recorder.Eventf(
			instance,
			nil,
			corev1.EventTypeWarning,
			"RecoveryRequired",
			"RecoverCredentials",
			"RECOVERY_REQUIRED: %s",
			reason,
		)
	}

	return requeueIf(changed), err
}

func newServiceCredentials(appHash string) (*valkeyv1alpha1.ServiceCredentials, error) {
	operatorPassword, err := randomServicePassword()
	if err != nil {
		return nil, err
	}
	replicaPassword, err := randomServicePassword()
	if err != nil {
		return nil, err
	}
	healthPassword, err := randomServicePassword()
	if err != nil {
		return nil, err
	}

	credentials := &valkeyv1alpha1.ServiceCredentials{
		OperatorPassword: operatorPassword,
		ReplicaPassword:  replicaPassword,
		HealthPassword:   healthPassword,
	}
	credentials.UsersACL = valkeyclient.InitialACL(
		appHash,
		string(operatorPassword),
		string(replicaPassword),
		string(healthPassword),
	)

	return credentials, nil
}

func randomServicePassword() ([]byte, error) {
	randomBytes := make([]byte, valkeyv1alpha1.ServicePasswordRandomBytes)
	if _, err := rand.Read(randomBytes); err != nil {
		return nil, fmt.Errorf("сгенерировать служебный пароль: %w", err)
	}

	return []byte(base64.RawURLEncoding.EncodeToString(randomBytes)), nil
}

func updateCredentialsSecret(
	instance *valkeyv1alpha1.ValkeyInstance,
	secret *corev1.Secret,
	credentials *valkeyv1alpha1.ServiceCredentials,
	scheme *runtime.Scheme,
) (bool, error) {
	before := secret.DeepCopy()
	if secret.Type != corev1.SecretTypeOpaque {
		return false, valkeyv1alpha1.ErrServiceCredentialsInvalid
	}
	if err := controllerutil.SetControllerReference(instance, secret, scheme); err != nil {
		return false, fmt.Errorf("назначить ownerReference Secret: %w", err)
	}

	data, err := valkeyv1alpha1.ServiceCredentialsData(*credentials)
	if err != nil {
		return false, err
	}
	if secret.Data == nil {
		secret.Data = make(map[string][]byte, len(data))
	}
	maps.Copy(secret.Data, data)

	return !reflect.DeepEqual(before, secret), nil
}

func credentialFailureReason(err error) string {
	switch {
	case errors.Is(err, valkeyv1alpha1.ErrAppPasswordHashMissing):
		return "AppPasswordHashMissing"
	case errors.Is(err, valkeyv1alpha1.ErrAppPasswordHashInvalid):
		return "AppPasswordHashInvalid"
	case errors.Is(err, valkeyv1alpha1.ErrServiceCredentialsPartial):
		return "ServiceCredentialsPartial"
	default:
		return "ServiceCredentialsInvalid"
	}
}
