package sync

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

var (
	errObjectOwnership  = errors.New("объект Kubernetes принадлежит другому инстансу")
	errObjectReplaced   = errors.New("UID объекта Kubernetes изменился")
	errRecoveryRequired = errors.New("требуется восстановление объектов Kubernetes")
)

func requiresRecovery(message string) error {
	return fmt.Errorf("%w: %s", errRecoveryRequired, message)
}

func parseInstanceID(value string) (uuid.UUID, error) {
	instanceID, err := uuid.Parse(value)
	if err != nil || instanceID.Version() != uuid.Version(7) {
		return uuid.Nil, errObjectOwnership
	}

	return instanceID, nil
}

func namespaceName(slug string) string {
	return "valkey-" + slug
}

func objectLabels(instance store.ValkeyInstance) map[string]string {
	return map[string]string{
		valkeyv1alpha1.InstanceLabelKey:   instance.Slug,
		valkeyv1alpha1.InstanceIDLabelKey: instance.ID.String(),
		valkeyv1alpha1.UserIDLabelKey:     instance.UserID.String(),
		valkeyv1alpha1.ManagedByLabelKey:  valkeyv1alpha1.ManagedByLabelValue,
	}
}

func validateLabels(labels map[string]string, instance store.ValkeyInstance) error {
	for key, expected := range objectLabels(instance) {
		if labels[key] != expected {
			return errObjectOwnership
		}
	}

	return nil
}

func validateNamespace(namespace *corev1.Namespace, instance store.ValkeyInstance) error {
	if namespace.Name != namespaceName(instance.Slug) {
		return errObjectOwnership
	}
	if err := validateLabels(namespace.Labels, instance); err != nil {
		return err
	}
	if instance.KubernetesNamespaceUID != nil && string(namespace.UID) != *instance.KubernetesNamespaceUID {
		return errObjectReplaced
	}

	return nil
}

func validateSecret(secret *corev1.Secret, instance store.ValkeyInstance) error {
	if secret.Namespace != namespaceName(instance.Slug) ||
		secret.Name != valkeyv1alpha1.AuthSecretName(instance.Slug) {
		return errObjectOwnership
	}

	return validateLabels(secret.Labels, instance)
}

func validateCR(resource *valkeyv1alpha1.ValkeyInstance, instance store.ValkeyInstance) error {
	if resource.Namespace != namespaceName(instance.Slug) || resource.Name != instance.Slug ||
		resource.Spec.InstanceID != instance.ID.String() || resource.Spec.Slug != instance.Slug {
		return errObjectOwnership
	}
	if err := validateLabels(resource.Labels, instance); err != nil {
		return err
	}
	if instance.KubernetesCRUID != nil && string(resource.UID) != *instance.KubernetesCRUID {
		return errObjectReplaced
	}

	return nil
}

func (s *Service) ensureNamespace(
	ctx context.Context,
	instance store.ValkeyInstance,
) (*corev1.Namespace, error) {
	namespace := &corev1.Namespace{}
	key := client.ObjectKey{Name: namespaceName(instance.Slug)}
	err := s.kubernetes.Get(ctx, key, namespace)
	if apierrors.IsNotFound(err) {
		if instance.KubernetesNamespaceUID != nil || creationHasHistory(instance) {
			return nil, requiresRecovery("ранее известный Namespace отсутствует")
		}
		namespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: key.Name, Labels: objectLabels(instance),
		}}
		if err := s.kubernetes.Create(ctx, namespace); err != nil {
			return nil, fmt.Errorf("создать Namespace: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("прочитать Namespace: %w", err)
	}

	if err := validateNamespace(namespace, instance); err != nil {
		return nil, err
	}
	if namespace.DeletionTimestamp != nil {
		return nil, requiresRecovery("Namespace уже удаляется")
	}
	if err := s.repository.BindValkeyNamespaceUID(ctx, instance.ID, string(namespace.UID)); err != nil {
		return nil, fmt.Errorf("сохранить UID Namespace: %w", err)
	}

	return namespace, nil
}
