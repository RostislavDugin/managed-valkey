package sync

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func (s *Service) reconcileDeletion(ctx context.Context, instance store.ValkeyInstance) error {
	namespace, exists, err := s.readDeletionNamespace(ctx, instance)
	if err != nil {
		return s.deliveryError(ctx, instance.ID, "deletion_namespace_identity", err)
	}
	if !exists {
		return s.finishDeletionWithoutNamespace(ctx, instance)
	}
	if instance.DeletionStage != nil &&
		*instance.DeletionStage == domain.ValkeyDeletionStageNamespacePrepared {
		return s.deletePreparedNamespace(ctx, instance, namespace)
	}

	resource, exists, err := s.readDeletionCR(ctx, instance)
	if err != nil {
		return s.deliveryError(ctx, instance.ID, "deletion_cr_identity", err)
	}
	if !exists {
		return s.continueDeletionWithoutCR(ctx, instance, namespace)
	}
	if !slices.Contains(resource.Finalizers, valkeyv1alpha1.InstanceFinalizer) {
		return s.deliveryError(
			ctx,
			instance.ID,
			"deletion_finalizer_missing",
			requiresRecovery("защитная отметка удаляемого ValkeyInstance отсутствует"),
		)
	}

	namespaceUID := string(namespace.UID)
	crUID := string(resource.UID)
	if err := s.repository.PrepareValkeyDeletion(
		ctx,
		instance.ID,
		domain.ValkeyDeletionStageCRPrepared,
		namespaceUID,
		&crUID,
	); err != nil {
		return fmt.Errorf("подготовить удаление ValkeyInstance: %w", err)
	}
	if resource.DeletionTimestamp != nil {
		return nil
	}

	fresh, err := s.repository.FindValkeyInstanceForSync(ctx, instance.ID)
	if err != nil {
		return err
	}
	if fresh.DeletionRequestedAt == nil || fresh.DeletionStage == nil ||
		*fresh.DeletionStage != domain.ValkeyDeletionStageCRPrepared {
		return store.ErrValkeyDeletionState
	}
	current, exists, err := s.readDeletionCR(ctx, fresh)
	if err != nil {
		return err
	}
	if !exists || current.DeletionTimestamp != nil {
		return nil
	}

	uid := types.UID(crUID)
	resourceVersion := current.ResourceVersion
	if err := s.kubernetes.Delete(
		ctx,
		current,
		client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
	); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("удалить ValkeyInstance: %w", err)
	}

	return nil
}

func (s *Service) readDeletionNamespace(
	ctx context.Context,
	instance store.ValkeyInstance,
) (*corev1.Namespace, bool, error) {
	namespace := &corev1.Namespace{}
	err := s.kubernetes.Get(ctx, client.ObjectKey{Name: namespaceName(instance.Slug)}, namespace)
	if apierrors.IsNotFound(err) {
		return namespace, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("прочитать Namespace при удалении: %w", err)
	}
	if err := validateNamespace(namespace, instance); err != nil {
		return nil, false, err
	}
	if err := s.repository.BindValkeyNamespaceUID(ctx, instance.ID, string(namespace.UID)); err != nil {
		return nil, false, err
	}

	return namespace, true, nil
}

func (s *Service) readDeletionCR(
	ctx context.Context,
	instance store.ValkeyInstance,
) (*valkeyv1alpha1.ValkeyInstance, bool, error) {
	resource := &valkeyv1alpha1.ValkeyInstance{}
	key := client.ObjectKey{Namespace: namespaceName(instance.Slug), Name: instance.Slug}
	err := s.kubernetes.Get(ctx, key, resource)
	if apierrors.IsNotFound(err) {
		return resource, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("прочитать ValkeyInstance при удалении: %w", err)
	}
	if err := validateCR(resource, instance); err != nil {
		return nil, false, err
	}
	if err := s.repository.BindValkeyCRUID(ctx, instance.ID, string(resource.UID)); err != nil {
		return nil, false, err
	}

	return resource, true, nil
}

func (s *Service) finishDeletionWithoutNamespace(ctx context.Context, instance store.ValkeyInstance) error {
	if instance.DeletionStage != nil {
		switch *instance.DeletionStage {
		case domain.ValkeyDeletionStageCRPrepared:
			namespaceUID := savedUID(instance.KubernetesNamespaceUID)
			if err := s.repository.PrepareValkeyDeletion(
				ctx,
				instance.ID,
				domain.ValkeyDeletionStageNamespacePrepared,
				namespaceUID,
				instance.KubernetesCRUID,
			); err != nil {
				return err
			}

			return s.repository.CompleteValkeyDeletion(ctx, instance.ID)
		case domain.ValkeyDeletionStageNamespacePrepared:
			return s.repository.CompleteValkeyDeletion(ctx, instance.ID)
		}
	}
	if creationHasHistory(instance) {
		return s.deliveryError(
			ctx,
			instance.ID,
			"namespace_missing",
			requiresRecovery("Namespace работающего инстанса отсутствует"),
		)
	}
	if err := s.repository.PrepareValkeyDeletion(
		ctx,
		instance.ID,
		domain.ValkeyDeletionStageNamespacePrepared,
		savedUID(instance.KubernetesNamespaceUID),
		nil,
	); err != nil {
		return err
	}

	return s.repository.CompleteValkeyDeletion(ctx, instance.ID)
}

func (s *Service) continueDeletionWithoutCR(
	ctx context.Context,
	instance store.ValkeyInstance,
	namespace *corev1.Namespace,
) error {
	if instance.DeletionStage != nil && *instance.DeletionStage == domain.ValkeyDeletionStageCRPrepared {
		if err := s.prepareNamespaceDeletion(ctx, instance, namespace); err != nil {
			return err
		}

		return s.deletePreparedNamespace(ctx, instance, namespace)
	}

	safe, err := s.safeCanceledCreation(ctx, instance)
	if err != nil {
		return s.deliveryError(ctx, instance.ID, "canceled_creation_state", err)
	}
	if !safe {
		return s.deliveryError(
			ctx,
			instance.ID,
			"cr_missing",
			requiresRecovery("ValkeyInstance с историей обработки отсутствует"),
		)
	}
	if err := s.prepareNamespaceDeletion(ctx, instance, namespace); err != nil {
		return err
	}

	return s.deletePreparedNamespace(ctx, instance, namespace)
}

func (s *Service) safeCanceledCreation(ctx context.Context, instance store.ValkeyInstance) (bool, error) {
	if creationHasHistory(instance) {
		return false, nil
	}

	secret := &corev1.Secret{}
	key := client.ObjectKey{
		Namespace: namespaceName(instance.Slug),
		Name:      valkeyv1alpha1.AuthSecretName(instance.Slug),
	}
	err := s.kubernetes.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("прочитать Secret отменённого создания: %w", err)
	}
	if err := validateSecret(secret, instance); err != nil {
		return false, err
	}
	credentials, err := valkeyv1alpha1.ParseServiceCredentials(secret.Data)
	if err != nil {
		return false, requiresRecovery("служебные поля Secret отменённого создания повреждены")
	}

	return credentials == nil, nil
}

func creationHasHistory(instance store.ValkeyInstance) bool {
	return instance.KubernetesCRUID != nil || instance.ObservedAt != nil ||
		instance.ObservedGeneration > 0 || instance.AppliedPasswordVersion > 0 ||
		instance.AppliedVCPU > 0 || instance.AppliedRAMGB > 0
}

func (s *Service) prepareNamespaceDeletion(
	ctx context.Context,
	instance store.ValkeyInstance,
	namespace *corev1.Namespace,
) error {
	namespaceUID := string(namespace.UID)
	return s.repository.PrepareValkeyDeletion(
		ctx,
		instance.ID,
		domain.ValkeyDeletionStageNamespacePrepared,
		namespaceUID,
		instance.KubernetesCRUID,
	)
}

func (s *Service) deletePreparedNamespace(
	ctx context.Context,
	instance store.ValkeyInstance,
	namespace *corev1.Namespace,
) error {
	fresh, err := s.repository.FindValkeyInstanceForSync(ctx, instance.ID)
	if err != nil {
		return err
	}
	if fresh.DeletionStage == nil || *fresh.DeletionStage != domain.ValkeyDeletionStageNamespacePrepared {
		return store.ErrValkeyDeletionState
	}
	if err := validateNamespace(namespace, fresh); err != nil {
		return err
	}

	uid := namespace.UID
	resourceVersion := namespace.ResourceVersion
	if err := s.kubernetes.Delete(
		ctx,
		namespace,
		client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
	); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("удалить Namespace: %w", err)
	}

	return nil
}

func savedUID(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}
