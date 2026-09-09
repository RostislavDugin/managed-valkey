package valkey

import (
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

type Actor struct {
	ID    uuid.UUID
	Email string
}

type Maintenance struct {
	DOW         int `json:"dow"`
	HourUTC     int `json:"hour_utc"`
	DurationMin int `json:"duration_min"`
}

type Instance struct {
	ID                        uuid.UUID                              `json:"id"`
	Name                      string                                 `json:"name"`
	Slug                      string                                 `json:"slug"`
	Mode                      domain.ValkeyInstanceMode              `json:"mode"`
	VCPU                      int                                    `json:"vcpu"`
	RAMGB                     int                                    `json:"ram_gb"`
	AppliedVCPU               int                                    `json:"applied_vcpu"`
	AppliedRAMGB              int                                    `json:"applied_ram_gb"`
	Host                      string                                 `json:"host"`
	HostRO                    *string                                `json:"host_ro"`
	Port                      int                                    `json:"port"`
	IsWhitelistEnabled        bool                                   `json:"is_whitelist_enabled"`
	WhitelistCIDRs            []string                               `json:"whitelist_cidrs"`
	Maintenance               *Maintenance                           `json:"maintenance"`
	PasswordHint              string                                 `json:"password_hint"`
	PasswordVersion           int                                    `json:"password_version"`
	AppliedPasswordVersion    int                                    `json:"applied_password_version"`
	Status                    domain.ValkeyInstanceStatus            `json:"status"`
	PhaseReason               *string                                `json:"phase_reason"`
	DesiredGeneration         int                                    `json:"desired_generation"`
	ObservedGeneration        int                                    `json:"observed_generation"`
	ObservedAt                *time.Time                             `json:"observed_at"`
	IsStale                   bool                                   `json:"is_stale"`
	IsUpdating                bool                                   `json:"is_updating"`
	IsRecoveryRequired        bool                                   `json:"is_recovery_required"`
	NetworkVerificationStatus domain.ValkeyNetworkVerificationStatus `json:"network_verification_status"`
	NetworkVerifiedAt         *time.Time                             `json:"network_verified_at"`
	CreatedAt                 time.Time                              `json:"created_at"`
	UpdatedAt                 time.Time                              `json:"updated_at"`
	ConfigurationRequestedAt  time.Time                              `json:"configuration_requested_at"`
	DeletionRequestedAt       *time.Time                             `json:"deletion_requested_at"`
}

type Credentials struct {
	Host                   string  `json:"host"`
	HostRO                 *string `json:"host_ro"`
	Port                   int     `json:"port"`
	Username               string  `json:"username"`
	PasswordHint           string  `json:"password_hint"`
	PasswordVersion        int     `json:"password_version"`
	AppliedPasswordVersion int     `json:"applied_password_version"`
}

type CreateInput struct {
	Name             string
	Prefix           string
	Mode             domain.ValkeyInstanceMode
	Size             Size
	Password         string
	WhitelistEnabled bool
	WhitelistCIDRs   []string
	IdempotencyKey   uuid.UUID
	RequestID        string
}

type PatchInput struct {
	Name           *string
	MaintenanceSet bool
	Maintenance    *Maintenance
	RequestID      string
}

type ResizeInput struct {
	Size           Size
	IdempotencyKey uuid.UUID
	RequestID      string
}

type WhitelistInput struct {
	Enabled   bool
	CIDRs     []string
	RequestID string
}

type RotateInput struct {
	Password                string
	ExpectedPasswordVersion int
	IdempotencyKey          uuid.UUID
	RequestID               string
}

type InstanceResult struct {
	Status   int
	Instance Instance
	Replay   bool
}

type CredentialsResult struct {
	Status      int
	Credentials Credentials
	Replay      bool
}

func instanceDTO(record store.ValkeyInstance, now time.Time) Instance {
	status := domain.ValkeyInstanceStatus(record.Phase)
	if record.DeletedAt != nil {
		status = domain.ValkeyInstanceStatusDeleted
	} else if record.DeletionRequestedAt != nil {
		status = domain.ValkeyInstanceStatusDeleting
	}

	var maintenance *Maintenance
	if record.MaintenanceDOW != nil && record.MaintenanceHourUTC != nil && record.MaintenanceDurationMin != nil {
		maintenance = &Maintenance{
			DOW: *record.MaintenanceDOW, HourUTC: *record.MaintenanceHourUTC,
			DurationMin: *record.MaintenanceDurationMin,
		}
	}

	cidrs := append([]string(nil), record.WhitelistCIDRs...)
	if cidrs == nil {
		cidrs = []string{}
	}

	return Instance{
		ID: record.ID, Name: record.Name, Slug: record.Slug,
		Mode: record.Mode, VCPU: record.VCPU, RAMGB: record.RAMGB,
		AppliedVCPU: record.AppliedVCPU, AppliedRAMGB: record.AppliedRAMGB,
		Host: record.Host, HostRO: record.HostRO, Port: record.Port,
		IsWhitelistEnabled: record.IsWhitelistEnabled, WhitelistCIDRs: cidrs, Maintenance: maintenance,
		PasswordHint: record.PasswordPrefix + "*****", PasswordVersion: record.PasswordVersion,
		AppliedPasswordVersion: record.AppliedPasswordVersion,
		Status:                 status, PhaseReason: record.PhaseReason,
		DesiredGeneration: record.DesiredGeneration, ObservedGeneration: record.ObservedGeneration,
		ObservedAt: record.ObservedAt, IsStale: isStale(record.ObservedAt, now),
		IsUpdating:                record.DesiredGeneration != record.ObservedGeneration,
		IsRecoveryRequired:        record.IsRecoveryRequired,
		NetworkVerificationStatus: record.NetworkVerificationStatus,
		NetworkVerifiedAt:         record.NetworkVerifiedAt,
		CreatedAt:                 record.CreatedAt, UpdatedAt: record.UpdatedAt,
		ConfigurationRequestedAt: record.ConfigurationRequestedAt,
		DeletionRequestedAt:      record.DeletionRequestedAt,
	}
}

func credentialsDTO(record store.ValkeyInstance) Credentials {
	return Credentials{
		Host: record.Host, HostRO: record.HostRO, Port: record.Port, Username: "app",
		PasswordHint: record.PasswordPrefix + "*****", PasswordVersion: record.PasswordVersion,
		AppliedPasswordVersion: record.AppliedPasswordVersion,
	}
}

func isStale(observedAt *time.Time, now time.Time) bool {
	return observedAt == nil || now.Sub(*observedAt) > time.Minute
}
