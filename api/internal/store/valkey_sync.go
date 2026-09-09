package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

var (
	ErrKubernetesIdentityConflict = errors.New("идентичность объекта Kubernetes изменилась")
	ErrValkeyObservationStale     = errors.New("наблюдение Valkey устарело")
	ErrValkeyObservationInvalid   = errors.New("наблюдение Valkey недопустимо")
	ErrValkeyDeletionState        = errors.New("этап удаления Valkey недопустим")
)

type ValkeyNodeObservation struct {
	Ordinal     int
	Role        domain.ValkeyNodeRole
	PodName     string
	PodUID      string
	ContainerID string
	RunID       string
	NodeName    string
	NodeUID     string
	IsReady     bool
	ObservedAt  time.Time
}

type ValkeyObservation struct {
	Phase                      domain.ValkeyInstancePhase
	PhaseReason                *string
	ObservedGeneration         int
	ObservedAt                 time.Time
	AppliedPasswordVersion     int
	HasAppliedConfiguration    bool
	AppliedMode                domain.ValkeyInstanceMode
	AppliedVCPU                int
	AppliedRAMGB               int
	HasNetwork                 bool
	NetworkVerificationStatus  domain.ValkeyNetworkVerificationStatus
	NetworkVerifiedAt          *time.Time
	IsOperatorRecoveryRequired bool
	OperatorRecoveryReason     *string
	Nodes                      []ValkeyNodeObservation
}

func (s *Store) ListValkeyInstancesForSync(ctx context.Context) ([]ValkeyInstance, error) {
	var instances []ValkeyInstance
	if err := s.db.WithContext(ctx).
		Where("deleted_at IS NULL").
		Order("id").
		Find(&instances).Error; err != nil {
		return nil, fmt.Errorf("прочитать инстансы Valkey для синхронизации: %w", err)
	}

	return instances, nil
}

func (s *Store) FindValkeyInstanceForSync(ctx context.Context, instanceID uuid.UUID) (ValkeyInstance, error) {
	return findValkeyInstance(s.db.WithContext(ctx).Where("id = ? AND deleted_at IS NULL", instanceID))
}

func (s *Store) BindValkeyNamespaceUID(ctx context.Context, instanceID uuid.UUID, uid string) error {
	return s.bindValkeyUID(ctx, instanceID, "kubernetes_namespace_uid", uid)
}

func (s *Store) BindValkeyCRUID(ctx context.Context, instanceID uuid.UUID, uid string) error {
	return s.bindValkeyUID(ctx, instanceID, "kubernetes_cr_uid", uid)
}

func (s *Store) bindValkeyUID(ctx context.Context, instanceID uuid.UUID, column, uid string) error {
	if uid == "" {
		return ErrKubernetesIdentityConflict
	}

	return s.WithinTransaction(ctx, func(tx *gorm.DB) error {
		instance, err := findValkeyInstanceForSyncUpdate(ctx, tx, instanceID)
		if err != nil {
			return err
		}

		var saved *string
		switch column {
		case "kubernetes_namespace_uid":
			saved = instance.KubernetesNamespaceUID
		case "kubernetes_cr_uid":
			saved = instance.KubernetesCRUID
		default:
			return ErrKubernetesIdentityConflict
		}
		if saved != nil {
			if *saved != uid {
				return ErrKubernetesIdentityConflict
			}

			return nil
		}

		return updateValkeySyncColumns(ctx, tx, instanceID, map[string]any{column: uid})
	})
}

func (s *Store) SetValkeySyncRecoveryReason(
	ctx context.Context,
	instanceID uuid.UUID,
	reason *string,
) error {
	return s.WithinTransaction(ctx, func(tx *gorm.DB) error {
		instance, err := findValkeyInstanceForSyncUpdate(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if equalStrings(instance.SyncRecoveryReason, reason) &&
			instance.IsRecoveryRequired == (reason != nil || instance.IsOperatorRecoveryRequired) {
			return nil
		}

		return updateValkeySyncColumns(ctx, tx, instanceID, map[string]any{
			"sync_recovery_reason": reason,
			"is_recovery_required": reason != nil || instance.IsOperatorRecoveryRequired,
		})
	})
}

func (s *Store) PrepareValkeyDeletion(
	ctx context.Context,
	instanceID uuid.UUID,
	stage domain.ValkeyDeletionStage,
	namespaceUID string,
	crUID *string,
) error {
	return s.WithinTransaction(ctx, func(tx *gorm.DB) error {
		instance, err := findValkeyInstanceForSyncUpdate(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if instance.DeletionRequestedAt == nil {
			return ErrValkeyDeletionState
		}
		if err := validateDeletionStage(instance, stage, namespaceUID, crUID); err != nil {
			return err
		}
		if instance.DeletionStage != nil && *instance.DeletionStage == stage {
			return nil
		}

		values := map[string]any{"deletion_stage": stage}
		if namespaceUID != "" {
			values["kubernetes_namespace_uid"] = namespaceUID
		}
		if crUID != nil {
			values["kubernetes_cr_uid"] = *crUID
		}

		return updateValkeySyncColumns(ctx, tx, instanceID, values)
	})
}

func validateDeletionStage(
	instance ValkeyInstance,
	stage domain.ValkeyDeletionStage,
	namespaceUID string,
	crUID *string,
) error {
	if stage != domain.ValkeyDeletionStageCRPrepared && stage != domain.ValkeyDeletionStageNamespacePrepared {
		return ErrValkeyDeletionState
	}
	if namespaceUID == "" && (stage != domain.ValkeyDeletionStageNamespacePrepared ||
		instance.KubernetesNamespaceUID != nil) {
		return ErrValkeyDeletionState
	}
	if namespaceUID != "" && instance.KubernetesNamespaceUID != nil &&
		*instance.KubernetesNamespaceUID != namespaceUID {
		return ErrKubernetesIdentityConflict
	}
	if instance.DeletionStage != nil &&
		*instance.DeletionStage == domain.ValkeyDeletionStageNamespacePrepared &&
		stage == domain.ValkeyDeletionStageCRPrepared {
		return ErrValkeyDeletionState
	}
	if stage == domain.ValkeyDeletionStageCRPrepared && (crUID == nil || *crUID == "") {
		return ErrValkeyDeletionState
	}
	if crUID != nil && instance.KubernetesCRUID != nil && *instance.KubernetesCRUID != *crUID {
		return ErrKubernetesIdentityConflict
	}

	return nil
}

func (s *Store) CompleteValkeyDeletion(ctx context.Context, instanceID uuid.UUID) error {
	return s.WithinTransaction(ctx, func(tx *gorm.DB) error {
		if err := s.AcquireValkeyMutationLock(ctx, tx); err != nil {
			return err
		}

		instance, err := findValkeyInstanceForSyncUpdateIncludingDeleted(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if instance.DeletedAt != nil {
			return nil
		}
		if instance.DeletionRequestedAt == nil || instance.DeletionStage == nil ||
			*instance.DeletionStage != domain.ValkeyDeletionStageNamespacePrepared {
			return ErrValkeyDeletionState
		}

		now, err := s.DatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		event := ValkeyInstancePhaseEvent{
			InstanceID: instance.ID,
			FromPhase:  instance.Phase,
			ToPhase:    domain.ValkeyInstanceStatusDeleted,
			CreatedAt:  now,
		}
		if err := tx.WithContext(ctx).Create(&event).Error; err != nil {
			return fmt.Errorf("сохранить завершение удаления Valkey: %w", err)
		}
		if err := tx.WithContext(ctx).Where("instance_id = ?", instanceID).
			Delete(&ValkeyInstanceNode{}).Error; err != nil {
			return fmt.Errorf("удалить ноды завершённого инстанса Valkey: %w", err)
		}

		return updateValkeySyncColumns(ctx, tx, instanceID, map[string]any{"deleted_at": now})
	})
}

func (s *Store) ImportValkeyObservation(
	ctx context.Context,
	instanceID uuid.UUID,
	observation ValkeyObservation,
) error {
	return s.WithinTransaction(ctx, func(tx *gorm.DB) error {
		if err := s.AcquireValkeyMutationLock(ctx, tx); err != nil {
			return err
		}

		instance, err := findValkeyInstanceForSyncUpdate(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if err := validateValkeyObservation(instance, observation); err != nil {
			return err
		}

		nodes := valkeyObservationNodes(instanceID, observation.Nodes)
		currentNodes, err := readValkeyNodes(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if valkeyObservationMatches(instance, observation, currentNodes, nodes) {
			return nil
		}

		if instance.Phase != observation.Phase {
			event := ValkeyInstancePhaseEvent{
				InstanceID: instance.ID,
				FromPhase:  instance.Phase,
				ToPhase:    domain.ValkeyInstanceStatus(observation.Phase),
				Reason:     observation.PhaseReason,
				CreatedAt:  observation.ObservedAt,
			}
			if err := tx.WithContext(ctx).Create(&event).Error; err != nil {
				return fmt.Errorf("сохранить переход фазы Valkey: %w", err)
			}
		}

		values := valkeyObservationColumns(instance, observation)
		if err := updateValkeySyncColumns(ctx, tx, instanceID, values); err != nil {
			return err
		}
		if err := tx.WithContext(ctx).Where("instance_id = ?", instanceID).
			Delete(&ValkeyInstanceNode{}).Error; err != nil {
			return fmt.Errorf("удалить прежние ноды Valkey: %w", err)
		}
		if len(nodes) > 0 {
			if err := tx.WithContext(ctx).Create(&nodes).Error; err != nil {
				return fmt.Errorf("сохранить ноды Valkey: %w", err)
			}
		}

		return nil
	})
}

func validateValkeyObservation(instance ValkeyInstance, observation ValkeyObservation) error {
	if instance.DeletionRequestedAt != nil || observation.ObservedAt.IsZero() {
		return ErrValkeyObservationStale
	}
	if observation.ObservedGeneration < instance.ObservedGeneration ||
		observation.AppliedPasswordVersion < instance.AppliedPasswordVersion ||
		(instance.ObservedAt != nil && observation.ObservedAt.Before(*instance.ObservedAt)) {
		return ErrValkeyObservationStale
	}
	if observation.ObservedGeneration > instance.DesiredGeneration ||
		observation.AppliedPasswordVersion > instance.PasswordVersion {
		return ErrValkeyObservationInvalid
	}
	if observation.HasAppliedConfiguration &&
		(observation.AppliedMode != instance.Mode || observation.AppliedVCPU <= 0 || observation.AppliedRAMGB <= 0) {
		return ErrValkeyObservationInvalid
	}

	return nil
}

func valkeyObservationColumns(instance ValkeyInstance, observation ValkeyObservation) map[string]any {
	values := map[string]any{
		"phase":                         observation.Phase,
		"phase_reason":                  observation.PhaseReason,
		"observed_generation":           observation.ObservedGeneration,
		"observed_at":                   observation.ObservedAt,
		"applied_password_version":      observation.AppliedPasswordVersion,
		"is_operator_recovery_required": observation.IsOperatorRecoveryRequired,
		"operator_recovery_reason":      observation.OperatorRecoveryReason,
		"is_recovery_required":          instance.SyncRecoveryReason != nil || observation.IsOperatorRecoveryRequired,
	}
	if observation.HasAppliedConfiguration {
		values["applied_vcpu"] = observation.AppliedVCPU
		values["applied_ram_gb"] = observation.AppliedRAMGB
	}
	if observation.HasNetwork {
		values["network_verification_status"] = observation.NetworkVerificationStatus
		values["network_verified_at"] = observation.NetworkVerifiedAt
	}

	return values
}

func valkeyObservationMatches(
	instance ValkeyInstance,
	observation ValkeyObservation,
	currentNodes []ValkeyInstanceNode,
	nodes []ValkeyInstanceNode,
) bool {
	if instance.Phase != observation.Phase || !equalStrings(instance.PhaseReason, observation.PhaseReason) ||
		instance.ObservedGeneration != observation.ObservedGeneration ||
		instance.ObservedAt == nil || !instance.ObservedAt.Equal(observation.ObservedAt) ||
		instance.AppliedPasswordVersion != observation.AppliedPasswordVersion ||
		instance.IsOperatorRecoveryRequired != observation.IsOperatorRecoveryRequired ||
		!equalStrings(instance.OperatorRecoveryReason, observation.OperatorRecoveryReason) ||
		instance.IsRecoveryRequired != (instance.SyncRecoveryReason != nil || observation.IsOperatorRecoveryRequired) {
		return false
	}
	if observation.HasAppliedConfiguration &&
		(instance.AppliedVCPU != observation.AppliedVCPU || instance.AppliedRAMGB != observation.AppliedRAMGB) {
		return false
	}
	if observation.HasNetwork &&
		(instance.NetworkVerificationStatus != observation.NetworkVerificationStatus ||
			!equalTimes(instance.NetworkVerifiedAt, observation.NetworkVerifiedAt)) {
		return false
	}

	return reflect.DeepEqual(currentNodes, nodes)
}

func valkeyObservationNodes(instanceID uuid.UUID, observations []ValkeyNodeObservation) []ValkeyInstanceNode {
	nodes := make([]ValkeyInstanceNode, 0, len(observations))
	for _, observation := range observations {
		nodes = append(nodes, ValkeyInstanceNode{
			InstanceID: instanceID, Ordinal: observation.Ordinal, Role: observation.Role,
			PodName: observation.PodName, PodUID: observation.PodUID, ContainerID: observation.ContainerID,
			RunID: observation.RunID, NodeName: observation.NodeName, NodeUID: observation.NodeUID,
			IsReady: observation.IsReady, ObservedAt: observation.ObservedAt,
		})
	}

	return nodes
}

func readValkeyNodes(ctx context.Context, tx *gorm.DB, instanceID uuid.UUID) ([]ValkeyInstanceNode, error) {
	var nodes []ValkeyInstanceNode
	if err := tx.WithContext(ctx).Where("instance_id = ?", instanceID).Order("ordinal").Find(&nodes).Error; err != nil {
		return nil, fmt.Errorf("прочитать ноды Valkey: %w", err)
	}

	return nodes, nil
}

func findValkeyInstanceForSyncUpdate(
	ctx context.Context,
	tx *gorm.DB,
	instanceID uuid.UUID,
) (ValkeyInstance, error) {
	return findValkeyInstance(tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND deleted_at IS NULL", instanceID))
}

func findValkeyInstanceForSyncUpdateIncludingDeleted(
	ctx context.Context,
	tx *gorm.DB,
	instanceID uuid.UUID,
) (ValkeyInstance, error) {
	return findValkeyInstance(tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", instanceID))
}

func updateValkeySyncColumns(
	ctx context.Context,
	tx *gorm.DB,
	instanceID uuid.UUID,
	values map[string]any,
) error {
	result := tx.WithContext(ctx).Model(&ValkeyInstance{}).Where("id = ?", instanceID).UpdateColumns(values)
	if result.Error != nil {
		return fmt.Errorf("сохранить состояние синхронизации Valkey: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}

	return nil
}

func equalStrings(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func equalTimes(left, right *time.Time) bool {
	return left == nil && right == nil || left != nil && right != nil && left.Equal(*right)
}
