package sync

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

const conflictRetries = 3

func (s *Service) deliverInstance(ctx context.Context, instanceID uuid.UUID) error {
	var err error
	for range conflictRetries {
		err = s.deliverInstanceAttempt(ctx, instanceID)
		if !apierrors.IsConflict(err) {
			return err
		}
	}

	return fmt.Errorf("исчерпаны повторы после конфликта Kubernetes: %w", err)
}

func (s *Service) deliverInstanceAttempt(ctx context.Context, instanceID uuid.UUID) error {
	instance, err := s.repository.FindValkeyInstanceForSync(ctx, instanceID)
	if err != nil {
		return err
	}
	if instance.DeletionRequestedAt != nil {
		return s.reconcileDeletion(ctx, instance)
	}

	if _, err := s.ensureNamespace(ctx, instance); err != nil {
		return s.deliveryError(ctx, instance.ID, "namespace_identity", err)
	}

	resource, resourceExists, err := s.readCR(ctx, instance)
	if err != nil {
		return s.deliveryError(ctx, instance.ID, "cr_identity", err)
	}
	secret, err := s.ensureSecret(ctx, instance, resource, resourceExists)
	if err != nil {
		return s.deliveryError(ctx, instance.ID, "secret_state", err)
	}
	if !resourceExists {
		resource, err = s.createCR(ctx, instance, secret)
	} else {
		resource, err = s.updateCR(ctx, instance, resource, secret)
	}
	if err != nil {
		return s.deliveryError(ctx, instance.ID, "cr_delivery", err)
	}
	if err := s.repository.BindValkeyCRUID(ctx, instance.ID, string(resource.UID)); err != nil {
		return s.deliveryError(ctx, instance.ID, "cr_identity", err)
	}

	if err := s.repository.SetValkeySyncRecoveryReason(ctx, instance.ID, nil); err != nil {
		return fmt.Errorf("снять причину восстановления: %w", err)
	}

	return nil
}

func (s *Service) readCR(
	ctx context.Context,
	instance store.ValkeyInstance,
) (*valkeyv1alpha1.ValkeyInstance, bool, error) {
	resource := &valkeyv1alpha1.ValkeyInstance{}
	key := client.ObjectKey{Namespace: namespaceName(instance.Slug), Name: instance.Slug}
	err := s.kubernetes.Get(ctx, key, resource)
	if apierrors.IsNotFound(err) {
		if instance.KubernetesCRUID != nil || instance.ObservedAt != nil ||
			instance.AppliedPasswordVersion > 0 || instance.ObservedGeneration > 0 {
			return nil, false, requiresRecovery("ранее известный ValkeyInstance отсутствует")
		}

		return resource, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("прочитать ValkeyInstance: %w", err)
	}
	if err := validateCR(resource, instance); err != nil {
		return nil, false, err
	}
	if err := s.repository.BindValkeyCRUID(ctx, instance.ID, string(resource.UID)); err != nil {
		return nil, false, fmt.Errorf("сохранить UID ValkeyInstance: %w", err)
	}

	return resource, true, nil
}

func (s *Service) ensureSecret(
	ctx context.Context,
	instance store.ValkeyInstance,
	resource *valkeyv1alpha1.ValkeyInstance,
	resourceExists bool,
) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{
		Namespace: namespaceName(instance.Slug),
		Name:      valkeyv1alpha1.AuthSecretName(instance.Slug),
	}
	err := s.kubernetes.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) {
		if resourceExists {
			return nil, requiresRecovery("Secret существующего ValkeyInstance отсутствует")
		}

		passwordKey, keyErr := valkeyv1alpha1.AppPasswordHashKey(int64(instance.PasswordVersion))
		if keyErr != nil {
			return nil, keyErr
		}
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: key.Namespace, Name: key.Name, Labels: objectLabels(instance),
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{passwordKey: []byte(instance.AppPasswordHash)},
		}
		if err := s.kubernetes.Create(ctx, secret); err != nil {
			return nil, fmt.Errorf("создать Secret: %w", err)
		}

		return secret, nil
	}
	if err != nil {
		return nil, fmt.Errorf("прочитать Secret: %w", err)
	}
	if err := validateSecret(secret, instance); err != nil {
		return nil, err
	}

	serviceCredentials, err := valkeyv1alpha1.ParseServiceCredentials(secret.Data)
	if err != nil {
		return nil, requiresRecovery("служебные поля Secret повреждены")
	}
	if !resourceExists && serviceCredentials != nil {
		return nil, requiresRecovery("Secret содержит служебные поля без ValkeyInstance")
	}
	if resourceExists {
		if resource.Status.CredentialsInitialized && serviceCredentials == nil {
			return nil, requiresRecovery("служебные поля Secret существующего ValkeyInstance отсутствуют")
		}
		if resource.Spec.PasswordVersion > 0 {
			if _, err := valkeyv1alpha1.ParseAppPasswordHash(secret.Data, resource.Spec.PasswordVersion); err != nil {
				return nil, requiresRecovery("действующий хеш пароля в Secret отсутствует или повреждён")
			}
		}
	}

	passwordKey, err := valkeyv1alpha1.AppPasswordHashKey(int64(instance.PasswordVersion))
	if err != nil {
		return nil, err
	}
	if saved, exists := secret.Data[passwordKey]; exists {
		if string(saved) != instance.AppPasswordHash {
			return nil, requiresRecovery("хеш одной версии пароля в Secret не совпадает")
		}

		return secret, nil
	}

	secret.Data = maps.Clone(secret.Data)
	if secret.Data == nil {
		secret.Data = make(map[string][]byte, 1)
	}
	secret.Data[passwordKey] = []byte(instance.AppPasswordHash)
	if err := s.kubernetes.Update(ctx, secret); err != nil {
		return nil, fmt.Errorf("добавить версию пароля в Secret: %w", err)
	}

	return secret, nil
}

func (s *Service) createCR(
	ctx context.Context,
	instance store.ValkeyInstance,
	secret *corev1.Secret,
) (*valkeyv1alpha1.ValkeyInstance, error) {
	if instance.KubernetesCRUID != nil || instance.ObservedAt != nil ||
		instance.AppliedPasswordVersion > 0 || instance.ObservedGeneration > 0 {
		return nil, requiresRecovery("ValkeyInstance с сохранённой историей нельзя создать заново")
	}
	if credentials, err := valkeyv1alpha1.ParseServiceCredentials(secret.Data); err != nil || credentials != nil {
		return nil, requiresRecovery("Secret не соответствует незавершённому созданию")
	}

	resource := &valkeyv1alpha1.ValkeyInstance{
		TypeMeta: metav1.TypeMeta{APIVersion: valkeyv1alpha1.GroupVersion.String(), Kind: "ValkeyInstance"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespaceName(instance.Slug), Name: instance.Slug,
			Labels: objectLabels(instance), Finalizers: []string{valkeyv1alpha1.InstanceFinalizer},
		},
		Spec: desiredSpec(instance),
	}
	if err := s.kubernetes.Create(ctx, resource); err != nil {
		return nil, fmt.Errorf("создать ValkeyInstance: %w", err)
	}

	return resource, nil
}

func (s *Service) updateCR(
	ctx context.Context,
	instance store.ValkeyInstance,
	resource *valkeyv1alpha1.ValkeyInstance,
	secret *corev1.Secret,
) (*valkeyv1alpha1.ValkeyInstance, error) {
	if resource.DeletionTimestamp != nil {
		return nil, requiresRecovery("ValkeyInstance удаляется без запроса в PostgreSQL")
	}
	if resource.Spec.DesiredGeneration > int64(instance.DesiredGeneration) ||
		resource.Spec.PasswordVersion > int64(instance.PasswordVersion) {
		return nil, requiresRecovery("ValkeyInstance содержит версию новее намерения PostgreSQL")
	}
	if _, err := valkeyv1alpha1.ParseAppPasswordHash(secret.Data, int64(instance.PasswordVersion)); err != nil {
		return nil, requiresRecovery("новая версия пароля не подготовлена в Secret")
	}

	desired := desiredSpec(instance)
	finalizerPresent := slices.Contains(resource.Finalizers, valkeyv1alpha1.InstanceFinalizer)
	if reflect.DeepEqual(resource.Spec, desired) && finalizerPresent {
		return resource, nil
	}

	resource.Spec = desired
	if !finalizerPresent {
		resource.Finalizers = append(resource.Finalizers, valkeyv1alpha1.InstanceFinalizer)
	}
	if err := s.kubernetes.Update(ctx, resource); err != nil {
		return nil, fmt.Errorf("обновить ValkeyInstance: %w", err)
	}

	return resource, nil
}

func desiredSpec(instance store.ValkeyInstance) valkeyv1alpha1.ValkeyInstanceSpec {
	return valkeyv1alpha1.ValkeyInstanceSpec{
		InstanceID: instance.ID.String(),
		Slug:       instance.Slug,
		Mode:       valkeyv1alpha1.ValkeyMode(instance.Mode),
		VCPU:       int32(instance.VCPU),
		RAMGB:      int32(instance.RAMGB),
		PublicPort: int32(instance.Port),
		Whitelist: &valkeyv1alpha1.WhitelistSpec{
			IsEnabled: instance.IsWhitelistEnabled,
			CIDRs:     append([]string(nil), instance.WhitelistCIDRs...),
		},
		PasswordVersion:   int64(instance.PasswordVersion),
		DesiredGeneration: int64(instance.DesiredGeneration),
	}
}

func (s *Service) deliveryError(ctx context.Context, instanceID uuid.UUID, reason string, cause error) error {
	if !persistentDeliveryError(cause) {
		return cause
	}
	if err := s.repository.SetValkeySyncRecoveryReason(ctx, instanceID, &reason); err != nil {
		return errors.Join(cause, fmt.Errorf("сохранить причину восстановления: %w", err))
	}

	return cause
}

func persistentDeliveryError(err error) bool {
	return errors.Is(err, errObjectOwnership) || errors.Is(err, errObjectReplaced) ||
		errors.Is(err, errRecoveryRequired) || errors.Is(err, store.ErrKubernetesIdentityConflict)
}

func (s *Service) reportOrphanNamespaces(ctx context.Context) error {
	list := &corev1.NamespaceList{}
	if err := s.kubernetes.List(ctx, list, client.MatchingLabels{
		valkeyv1alpha1.ManagedByLabelKey: valkeyv1alpha1.ManagedByLabelValue,
	}); err != nil {
		return fmt.Errorf("прочитать Namespace для сверки владельцев: %w", err)
	}

	for index := range list.Items {
		namespace := &list.Items[index]
		instanceID, err := parseInstanceID(namespace.Labels[valkeyv1alpha1.InstanceIDLabelKey])
		if err != nil {
			s.logger.Warn("Namespace имеет повреждённую метку инстанса", "namespace", namespace.Name)

			continue
		}
		_, err = s.repository.FindValkeyInstanceForSync(ctx, instanceID)
		if errors.Is(err, store.ErrNotFound) {
			s.logger.Warn(
				"Namespace не имеет строки PostgreSQL",
				"namespace",
				namespace.Name,
				"instance_id",
				instanceID,
			)

			continue
		}
		if err != nil {
			return fmt.Errorf("проверить владельца Namespace %s: %w", namespace.Name, err)
		}
	}

	return nil
}
