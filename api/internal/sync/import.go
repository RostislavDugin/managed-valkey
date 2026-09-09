package sync

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func (s *Service) importInstance(ctx context.Context, instanceID uuid.UUID) error {
	instance, err := s.repository.FindValkeyInstanceForSync(ctx, instanceID)
	if err != nil {
		return err
	}
	if instance.DeletionRequestedAt != nil {
		return nil
	}

	namespace := &corev1.Namespace{}
	if err := s.kubernetes.Get(ctx, client.ObjectKey{Name: namespaceName(instance.Slug)}, namespace); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("прочитать Namespace для импорта: %w", err)
	}
	if err := validateNamespace(namespace, instance); err != nil {
		return err
	}
	if err := s.repository.BindValkeyNamespaceUID(ctx, instance.ID, string(namespace.UID)); err != nil {
		return err
	}

	resource := &valkeyv1alpha1.ValkeyInstance{}
	key := client.ObjectKey{Namespace: namespace.Name, Name: instance.Slug}
	if err := s.kubernetes.Get(ctx, key, resource); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("прочитать ValkeyInstance для импорта: %w", err)
	}
	if resource.DeletionTimestamp != nil {
		return nil
	}
	if err := validateCR(resource, instance); err != nil {
		return err
	}
	if err := s.repository.BindValkeyCRUID(ctx, instance.ID, string(resource.UID)); err != nil {
		return err
	}

	observation, present, err := buildObservation(resource)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if err := s.repository.ImportValkeyObservation(ctx, instance.ID, observation); err != nil {
		if errors.Is(err, store.ErrValkeyObservationStale) {
			return nil
		}

		return err
	}

	return nil
}

func buildObservation(resource *valkeyv1alpha1.ValkeyInstance) (store.ValkeyObservation, bool, error) {
	status := resource.Status
	if status.ObservedAt == nil {
		return store.ValkeyObservation{}, false, nil
	}
	if !validPhase(status.Phase) {
		return store.ValkeyObservation{}, false, store.ErrValkeyObservationInvalid
	}
	if status.ObservedGeneration < 0 || status.ObservedGeneration > resource.Spec.DesiredGeneration ||
		status.AppliedPasswordVersion < 0 || status.AppliedPasswordVersion > resource.Spec.PasswordVersion {
		return store.ValkeyObservation{}, false, store.ErrValkeyObservationInvalid
	}
	if status.AcceptedConfiguration != nil && !acceptedIdentityMatches(resource, status.AcceptedConfiguration) {
		return store.ValkeyObservation{}, false, store.ErrValkeyObservationInvalid
	}
	if status.AcceptedConfiguration != nil &&
		(status.ObservedGeneration > status.AcceptedConfiguration.DesiredGeneration ||
			status.AppliedPasswordVersion > status.AcceptedConfiguration.PasswordVersion) {
		return store.ValkeyObservation{}, false, store.ErrValkeyObservationInvalid
	}

	reason := optionalString(status.Reason)
	recoveryCondition := apimeta.FindStatusCondition(
		status.Conditions,
		valkeyv1alpha1.ConditionTypeRecoveryRequired,
	)
	operatorRecovery := recoveryCondition != nil && recoveryCondition.Status == metav1.ConditionTrue
	operatorRecoveryReason := (*string)(nil)
	if operatorRecovery {
		operatorRecoveryReason = optionalString(recoveryCondition.Reason)
		if operatorRecoveryReason == nil {
			value := valkeyv1alpha1.ConditionTypeRecoveryRequired
			operatorRecoveryReason = &value
		}
	}

	observation := store.ValkeyObservation{
		Phase:                      domain.ValkeyInstancePhase(status.Phase),
		PhaseReason:                reason,
		ObservedGeneration:         int(status.ObservedGeneration),
		ObservedAt:                 status.ObservedAt.UTC(),
		AppliedPasswordVersion:     int(status.AppliedPasswordVersion),
		IsOperatorRecoveryRequired: operatorRecovery,
		OperatorRecoveryReason:     operatorRecoveryReason,
	}
	if status.Applied != nil {
		if !validAppliedConfiguration(*status.Applied) {
			return store.ValkeyObservation{}, false, store.ErrValkeyObservationInvalid
		}
		observation.HasAppliedConfiguration = true
		observation.AppliedMode = domain.ValkeyInstanceMode(status.Applied.Mode)
		observation.AppliedVCPU = int(status.Applied.VCPU)
		observation.AppliedRAMGB = int(status.Applied.RAMGB)
	}
	if status.Network != nil {
		if !validNetworkStatus(status.Network.VerificationStatus) {
			return store.ValkeyObservation{}, false, store.ErrValkeyObservationInvalid
		}
		observation.HasNetwork = true
		observation.NetworkVerificationStatus = domain.ValkeyNetworkVerificationStatus(
			status.Network.VerificationStatus,
		)
		if status.Network.VerifiedAt != nil {
			verifiedAt := status.Network.VerifiedAt.UTC()
			observation.NetworkVerifiedAt = &verifiedAt
		}
	}

	nodes, err := buildNodeObservations(resource)
	if err != nil {
		return store.ValkeyObservation{}, false, err
	}
	observation.Nodes = nodes

	return observation, true, nil
}

func acceptedIdentityMatches(
	resource *valkeyv1alpha1.ValkeyInstance,
	accepted *valkeyv1alpha1.AcceptedConfiguration,
) bool {
	return accepted.InstanceID == resource.Spec.InstanceID && accepted.Slug == resource.Spec.Slug &&
		accepted.DesiredGeneration <= resource.Spec.DesiredGeneration &&
		accepted.PasswordVersion <= resource.Spec.PasswordVersion
}

func buildNodeObservations(
	resource *valkeyv1alpha1.ValkeyInstance,
) ([]store.ValkeyNodeObservation, error) {
	maximum := 1
	if resource.Spec.Mode == valkeyv1alpha1.ValkeyModeHA {
		maximum = 3
	}
	if len(resource.Status.Nodes) > maximum {
		return nil, store.ErrValkeyObservationInvalid
	}

	seen := make(map[int32]struct{}, len(resource.Status.Nodes))
	observations := make([]store.ValkeyNodeObservation, 0, len(resource.Status.Nodes))
	for _, node := range resource.Status.Nodes {
		if node.Ordinal < 0 || int(node.Ordinal) >= maximum || node.PodUID == "" || node.ContainerID == "" ||
			node.RunID == "" || node.NodeName == "" || node.NodeUID == "" {
			return nil, store.ErrValkeyObservationInvalid
		}
		if _, exists := seen[node.Ordinal]; exists {
			return nil, store.ErrValkeyObservationInvalid
		}
		seen[node.Ordinal] = struct{}{}

		role := domain.ValkeyNodeRole(node.Role)
		if node.Role == "" {
			role = domain.ValkeyNodeRoleUnknown
		} else if node.Role != valkeyv1alpha1.NodeRolePrimary && node.Role != valkeyv1alpha1.NodeRoleReplica {
			return nil, store.ErrValkeyObservationInvalid
		}
		observations = append(observations, store.ValkeyNodeObservation{
			Ordinal: int(node.Ordinal), Role: role,
			PodName: resource.Spec.Slug + "-" + strconv.Itoa(int(node.Ordinal)),
			PodUID:  node.PodUID, ContainerID: node.ContainerID, RunID: node.RunID,
			NodeName: node.NodeName, NodeUID: node.NodeUID, IsReady: node.Readiness,
			ObservedAt: resource.Status.ObservedAt.UTC(),
		})
	}

	return observations, nil
}

func validPhase(phase valkeyv1alpha1.InstancePhase) bool {
	switch phase {
	case valkeyv1alpha1.InstancePhaseProvisioning,
		valkeyv1alpha1.InstancePhaseRunning,
		valkeyv1alpha1.InstancePhaseUpdating,
		valkeyv1alpha1.InstancePhaseDegraded,
		valkeyv1alpha1.InstancePhaseUnavailable,
		valkeyv1alpha1.InstancePhaseError:
		return true
	default:
		return false
	}
}

func validAppliedConfiguration(applied valkeyv1alpha1.AppliedConfiguration) bool {
	return (applied.Mode == valkeyv1alpha1.ValkeyModeSingle || applied.Mode == valkeyv1alpha1.ValkeyModeHA) &&
		slices.Contains([]int32{1, 2, 4, 8, 16}, applied.VCPU) &&
		slices.Contains([]int32{1, 2, 4, 8, 16, 32, 64, 128}, applied.RAMGB) &&
		applied.RAMGB >= applied.VCPU && applied.RAMGB <= 16*applied.VCPU
}

func validNetworkStatus(status valkeyv1alpha1.NetworkVerificationStatus) bool {
	return status == valkeyv1alpha1.NetworkVerificationPending ||
		status == valkeyv1alpha1.NetworkVerificationVerified ||
		status == valkeyv1alpha1.NetworkVerificationUnknown
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}
