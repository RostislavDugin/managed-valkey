package store

import (
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

type ValkeyInstance struct {
	ID                         uuid.UUID                              `gorm:"column:id;type:uuid;default:uuidv7();primaryKey"`
	UserID                     uuid.UUID                              `gorm:"column:user_id;type:uuid"`
	UserEmail                  string                                 `gorm:"column:user_email"`
	Name                       string                                 `gorm:"column:name"`
	Slug                       string                                 `gorm:"column:slug"`
	Mode                       domain.ValkeyInstanceMode              `gorm:"column:mode"`
	VCPU                       int                                    `gorm:"column:vcpu"`
	RAMGB                      int                                    `gorm:"column:ram_gb"`
	DesiredGeneration          int                                    `gorm:"column:desired_generation"`
	CreatedAt                  time.Time                              `gorm:"column:created_at"`
	UpdatedAt                  time.Time                              `gorm:"column:updated_at"`
	ConfigurationRequestedAt   time.Time                              `gorm:"column:configuration_requested_at"`
	AppPasswordHash            string                                 `gorm:"column:app_password_hash"`
	PasswordPrefix             string                                 `gorm:"column:password_prefix"`
	PasswordVersion            int                                    `gorm:"column:password_version"`
	Host                       string                                 `gorm:"column:host"`
	HostRO                     *string                                `gorm:"column:host_ro"`
	Port                       int                                    `gorm:"column:port"`
	IsWhitelistEnabled         bool                                   `gorm:"column:is_whitelist_enabled"`
	WhitelistCIDRs             pq.StringArray                         `gorm:"column:whitelist_cidrs;type:text[]"`
	MaintenanceDOW             *int                                   `gorm:"column:maintenance_dow"`
	MaintenanceHourUTC         *int                                   `gorm:"column:maintenance_hour_utc"`
	MaintenanceDurationMin     *int                                   `gorm:"column:maintenance_duration_min"`
	DeletionRequestedAt        *time.Time                             `gorm:"column:deletion_requested_at"`
	KubernetesNamespaceUID     *string                                `gorm:"column:kubernetes_namespace_uid"`
	KubernetesCRUID            *string                                `gorm:"column:kubernetes_cr_uid"`
	DeletionStage              *domain.ValkeyDeletionStage            `gorm:"column:deletion_stage"`
	Phase                      domain.ValkeyInstancePhase             `gorm:"column:phase"`
	PhaseReason                *string                                `gorm:"column:phase_reason"`
	ObservedGeneration         int                                    `gorm:"column:observed_generation"`
	ObservedAt                 *time.Time                             `gorm:"column:observed_at"`
	IsRecoveryRequired         bool                                   `gorm:"column:is_recovery_required"`
	SyncRecoveryReason         *string                                `gorm:"column:sync_recovery_reason"`
	IsOperatorRecoveryRequired bool                                   `gorm:"column:is_operator_recovery_required"`
	OperatorRecoveryReason     *string                                `gorm:"column:operator_recovery_reason"`
	NetworkVerificationStatus  domain.ValkeyNetworkVerificationStatus `gorm:"column:network_verification_status"`
	NetworkVerifiedAt          *time.Time                             `gorm:"column:network_verified_at"`
	AppliedPasswordVersion     int                                    `gorm:"column:applied_password_version"`
	AppliedVCPU                int                                    `gorm:"column:applied_vcpu"`
	AppliedRAMGB               int                                    `gorm:"column:applied_ram_gb"`
	DeletedAt                  *time.Time                             `gorm:"column:deleted_at"`
}

func (ValkeyInstance) TableName() string { return "valkey_instances" }
