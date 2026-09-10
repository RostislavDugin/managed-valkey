package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ValkeyMode string

const (
	ValkeyModeSingle ValkeyMode = "single"
	ValkeyModeHA     ValkeyMode = "ha"

	InstanceFinalizer             = "valkey.h3llo-demo.com/instance-protection"
	InstanceLabelKey              = "valkey.h3llo-demo.com/instance"
	InstanceIDLabelKey            = "valkey.h3llo-demo.com/instance-id"
	UserIDLabelKey                = "valkey.h3llo-demo.com/user-id"
	ManagedByLabelKey             = "app.kubernetes.io/managed-by"
	ManagedByLabelValue           = "managed-valkey-api"
	ConditionTypeRecoveryRequired = "RecoveryRequired"
)

type InstancePhase string

const (
	InstancePhaseProvisioning InstancePhase = "provisioning"
	InstancePhaseRunning      InstancePhase = "running"
	InstancePhaseDegraded     InstancePhase = "degraded"
	InstancePhaseUpdating     InstancePhase = "updating"
	InstancePhaseUnavailable  InstancePhase = "unavailable"
	InstancePhaseError        InstancePhase = "error"
)

type NetworkVerificationStatus string

const (
	NetworkVerificationPending  NetworkVerificationStatus = "pending"
	NetworkVerificationVerified NetworkVerificationStatus = "verified"
	NetworkVerificationUnknown  NetworkVerificationStatus = "unknown"
)

type NodeRole string

const (
	NodeRolePrimary NodeRole = "primary"
	NodeRoleReplica NodeRole = "replica"
)

type ProcessObservationKind string

const (
	ProcessObservationTransportError ProcessObservationKind = "transportError"
	ProcessObservationBusy           ProcessObservationKind = "busy"
	ProcessObservationLoading        ProcessObservationKind = "loading"
	ProcessObservationAuthError      ProcessObservationKind = "authError"
	ProcessObservationServerError    ProcessObservationKind = "serverError"
)

type ProcessRecoveryReason string

const (
	ProcessRecoveryUnresponsive ProcessRecoveryReason = "unresponsive"
	ProcessRecoveryBusy         ProcessRecoveryReason = "busy"
)

type ProcessRecoveryStage string

const (
	ProcessRecoveryStageKillingBusy           ProcessRecoveryStage = "killingBusy"
	ProcessRecoveryStageDeleting              ProcessRecoveryStage = "deleting"
	ProcessRecoveryStageWaitingForTermination ProcessRecoveryStage = "waitingForTermination"
)

type DeletionStage string

const (
	DeletionStageRemovingNetwork DeletionStage = "removingNetwork"
	DeletionStageDisablingApp    DeletionStage = "disablingApp"
	DeletionStageStopping        DeletionStage = "stopping"
	DeletionStageVerifying       DeletionStage = "verifying"
)

type FailoverReason string

const (
	FailoverReasonFailure FailoverReason = "failure"
	FailoverReasonResize  FailoverReason = "resize"
)

type FailoverStage string

const (
	FailoverStageFencing       FailoverStage = "fencing"
	FailoverStageChoosing      FailoverStage = "choosing"
	FailoverStagePromoting     FailoverStage = "promoting"
	FailoverStageReconfiguring FailoverStage = "reconfiguring"
)

type RolloutStage string

const (
	RolloutStagePreparing         RolloutStage = "preparing"
	RolloutStageStopping          RolloutStage = "stopping"
	RolloutStageUpdatingTemplate  RolloutStage = "updatingTemplate"
	RolloutStageReplacingReplicas RolloutStage = "replacingReplicas"
	RolloutStageSwitchingPrimary  RolloutStage = "switchingPrimary"
	RolloutStageReplacingPrimary  RolloutStage = "replacingPrimary"
	RolloutStageStarting          RolloutStage = "starting"
	RolloutStageVerifying         RolloutStage = "verifying"
)

type CredentialRotationStage string

const (
	CredentialRotationStagePreparing        CredentialRotationStage = "preparing"
	CredentialRotationStageUpdatingReplicas CredentialRotationStage = "updatingReplicas"
	CredentialRotationStageUpdatingPrimary  CredentialRotationStage = "updatingPrimary"
	CredentialRotationStageCleaningSecret   CredentialRotationStage = "cleaningSecret"
)

// +kubebuilder:validation:XValidation:rule="!has(self.vcpu) || !has(self.ramGb) || (self.ramGb >= self.vcpu && self.ramGb <= 16 * self.vcpu)",message="ramGb должен быть от vcpu до 16 * vcpu"

type ValkeyInstanceSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="instanceId изменить нельзя"
	// +required
	InstanceID string `json:"instanceId"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="slug изменить нельзя"
	// +required
	Slug string `json:"slug"`

	// +kubebuilder:validation:Enum=single;ha
	// +optional
	Mode ValkeyMode `json:"mode,omitempty"`

	// +kubebuilder:validation:Enum=1;2;4;8;16
	// +optional
	VCPU int32 `json:"vcpu,omitempty"`

	// +kubebuilder:validation:Enum=1;2;4;8;16;32;64;128
	// +optional
	RAMGB int32 `json:"ramGb,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	PublicPort int32 `json:"publicPort,omitempty"`

	// +optional
	Whitelist *WhitelistSpec `json:"whitelist,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +optional
	PasswordVersion int64 `json:"passwordVersion,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +optional
	DesiredGeneration int64 `json:"desiredGeneration,omitempty"`
}

type WhitelistSpec struct {
	// +required
	IsEnabled bool `json:"isEnabled"`

	// +listType=set
	// +optional
	CIDRs []string `json:"cidrs,omitempty"`
}

type AcceptedConfiguration struct {
	InstanceID        string        `json:"instanceId"`
	Slug              string        `json:"slug"`
	Mode              ValkeyMode    `json:"mode"`
	VCPU              int32         `json:"vcpu"`
	RAMGB             int32         `json:"ramGb"`
	PublicPort        int32         `json:"publicPort"`
	Whitelist         WhitelistSpec `json:"whitelist"`
	PasswordVersion   int64         `json:"passwordVersion"`
	DesiredGeneration int64         `json:"desiredGeneration"`
}

type AppliedConfiguration struct {
	Mode  ValkeyMode `json:"mode"`
	VCPU  int32      `json:"vcpu"`
	RAMGB int32      `json:"ramGb"`
}

type ProcessIdentity struct {
	PodUID      string `json:"podUID"`
	ContainerID string `json:"containerID"`
	RunID       string `json:"runId"`
	NodeName    string `json:"nodeName"`
	NodeUID     string `json:"nodeUID"`
}

type ProcessTermination struct {
	Reason     string      `json:"reason"`
	ExitCode   int32       `json:"exitCode"`
	FinishedAt metav1.Time `json:"finishedAt"`
	Evidence   string      `json:"evidence"`
}

type ReplicationStatus struct {
	ReplicationID          string       `json:"replicationId,omitempty"`
	SecondaryReplicationID string       `json:"secondaryReplicationId,omitempty"`
	SecondaryOffset        int64        `json:"secondaryOffset,omitempty"`
	Offset                 int64        `json:"offset,omitempty"`
	UpstreamHost           string       `json:"upstreamHost,omitempty"`
	UpstreamPort           int32        `json:"upstreamPort,omitempty"`
	LinkUp                 bool         `json:"linkUp,omitempty"`
	SyncInProgress         bool         `json:"syncInProgress,omitempty"`
	SyncedAt               *metav1.Time `json:"syncedAt,omitempty"`
	ObservedAt             metav1.Time  `json:"observedAt"`
}

type ProcessObservationStatus struct {
	// +kubebuilder:validation:Enum=transportError;busy;loading;authError;serverError
	Kind ProcessObservationKind `json:"kind"`

	ObservedAt                 metav1.Time  `json:"observedAt"`
	ConsecutiveTransportErrors int32        `json:"consecutiveTransportErrors,omitempty"`
	TransportErrorSince        *metav1.Time `json:"transportErrorSince,omitempty"`
	BusySince                  *metav1.Time `json:"busySince,omitempty"`
}

type ProcessRecoveryStatus struct {
	// +kubebuilder:validation:Enum=unresponsive;busy
	Reason ProcessRecoveryReason `json:"reason"`
	// +kubebuilder:validation:Enum=killingBusy;deleting;waitingForTermination
	Stage             ProcessRecoveryStage `json:"stage"`
	StartedAt         metav1.Time          `json:"startedAt"`
	DeleteRequestedAt *metav1.Time         `json:"deleteRequestedAt,omitempty"`
}

type FailoverStatus struct {
	// +kubebuilder:validation:Enum=failure;resize
	Reason FailoverReason `json:"reason"`
	// +kubebuilder:validation:Enum=fencing;choosing;promoting;reconfiguring
	Stage     FailoverStage    `json:"stage"`
	StartedAt metav1.Time      `json:"startedAt"`
	Source    ProcessIdentity  `json:"source"`
	Candidate *ProcessIdentity `json:"candidate,omitempty"`

	SourceAppDisabled      bool `json:"sourceAppDisabled,omitempty"`
	SourceClientsKilled    bool `json:"sourceClientsKilled,omitempty"`
	CandidateAppDisabled   bool `json:"candidateAppDisabled,omitempty"`
	CandidateClientsKilled bool `json:"candidateClientsKilled,omitempty"`
	CandidateMayBePrimary  bool `json:"candidateMayBePrimary,omitempty"`

	ReplicationID  string       `json:"replicationId,omitempty"`
	ControlOffset  int64        `json:"controlOffset,omitempty"`
	OffsetDeadline *metav1.Time `json:"offsetDeadline,omitempty"`
	EmptySince     *metav1.Time `json:"emptySince,omitempty"`
}

type RolloutStatus struct {
	DesiredGeneration int64  `json:"desiredGeneration"`
	VCPU              int32  `json:"vcpu"`
	RAMGB             int32  `json:"ramGb"`
	Image             string `json:"image"`
	// +kubebuilder:validation:Enum=preparing;stopping;updatingTemplate;replacingReplicas;switchingPrimary;replacingPrimary;starting;verifying
	Stage        RolloutStage     `json:"stage"`
	Process      *ProcessIdentity `json:"process,omitempty"`
	AccessClosed bool             `json:"accessClosed,omitempty"`
}

type CredentialRotationConfirmation struct {
	Process ProcessIdentity `json:"process"`
	Version int64           `json:"version"`
}

type CredentialRotationStatus struct {
	TargetVersion   int64 `json:"targetVersion"`
	PreviousVersion int64 `json:"previousVersion"`
	// +kubebuilder:validation:Enum=preparing;updatingReplicas;updatingPrimary;cleaningSecret
	Stage CredentialRotationStage `json:"stage"`

	// +listType=atomic
	// +optional
	Confirmations []CredentialRotationConfirmation `json:"confirmations,omitempty"`
}

type NodeStatus struct {
	Ordinal     int32  `json:"ordinal"`
	PodUID      string `json:"podUID"`
	ContainerID string `json:"containerID"`
	RunID       string `json:"runId"`
	NodeName    string `json:"nodeName"`
	NodeUID     string `json:"nodeUID"`

	// +kubebuilder:validation:Enum=primary;replica
	// +optional
	Role NodeRole `json:"role,omitempty"`

	Readiness          bool                      `json:"readiness"`
	AppEnabled         bool                      `json:"appEnabled,omitempty"`
	AppPasswordVersion int64                     `json:"appPasswordVersion,omitempty"`
	Replication        *ReplicationStatus        `json:"replication,omitempty"`
	Observation        *ProcessObservationStatus `json:"observation,omitempty"`
	Recovery           *ProcessRecoveryStatus    `json:"recovery,omitempty"`
	Termination        *ProcessTermination       `json:"termination,omitempty"`
}

type NodeMetricStatus struct {
	// +kubebuilder:validation:Minimum=0
	// +required
	Ordinal int32 `json:"ordinal"`

	// +kubebuilder:validation:MinLength=1
	// +required
	PodUID string `json:"podUID"`

	// +kubebuilder:validation:MinLength=1
	// +required
	ContainerID string `json:"containerID"`

	// +kubebuilder:validation:MinLength=1
	// +required
	RunID string `json:"runId"`

	// +required
	CollectedAt metav1.MicroTime `json:"collectedAt"`

	// +kubebuilder:validation:Enum=primary;replica
	// +required
	Role NodeRole `json:"role"`

	// +kubebuilder:validation:Minimum=0
	// +required
	UsedMemoryBytes int64 `json:"usedMemoryBytes"`

	// +kubebuilder:validation:Minimum=0
	// +required
	MaxmemoryBytes int64 `json:"maxmemoryBytes"`

	// +kubebuilder:validation:Minimum=0
	// +required
	ConnectedClients int64 `json:"connectedClients"`

	// +kubebuilder:validation:Minimum=0
	// +required
	OpsPerSec int64 `json:"opsPerSec"`

	// +kubebuilder:validation:Minimum=0
	// +required
	KeyspaceHits int64 `json:"keyspaceHits"`

	// +kubebuilder:validation:Minimum=0
	// +required
	KeyspaceMisses int64 `json:"keyspaceMisses"`

	// +kubebuilder:validation:Minimum=0
	// +required
	EvictedKeys int64 `json:"evictedKeys"`

	// +kubebuilder:validation:Minimum=0
	// +nullable
	// +required
	CPUMillicores *int64 `json:"cpuMillicores"`
}

type EnvoyProcessStatus struct {
	PodUID      string `json:"podUID"`
	NodeName    string `json:"nodeName"`
	NodeUID     string `json:"nodeUID"`
	ContainerID string `json:"containerID"`
}

type NetworkStatus struct {
	// +kubebuilder:validation:Enum=pending;verified;unknown
	VerificationStatus  NetworkVerificationStatus `json:"verificationStatus"`
	VerifiedAt          *metav1.Time              `json:"verifiedAt,omitempty"`
	DesiredFingerprint  string                    `json:"desiredFingerprint,omitempty"`
	VerifiedFingerprint string                    `json:"verifiedFingerprint,omitempty"`

	// +listType=map
	// +listMapKey=podUID
	// +optional
	EnvoyProcesses []EnvoyProcessStatus `json:"envoyProcesses,omitempty"`
}

type DeletionStatus struct {
	// +kubebuilder:validation:Enum=removingNetwork;disablingApp;stopping;verifying
	Stage     DeletionStage `json:"stage"`
	StartedAt metav1.Time   `json:"startedAt"`
}

type ValkeyInstanceStatus struct {
	CredentialsInitialized bool `json:"credentialsInitialized,omitempty"`
	Initialized            bool `json:"initialized,omitempty"`
	// +kubebuilder:validation:Enum=provisioning;running;degraded;updating;unavailable;error
	// +optional
	Phase                  InstancePhase             `json:"phase,omitempty"`
	Reason                 string                    `json:"reason,omitempty"`
	ObservedAt             *metav1.Time              `json:"observedAt,omitempty"`
	PrimaryOrdinal         *int32                    `json:"primaryOrdinal,omitempty"`
	PrimaryPodUID          string                    `json:"primaryPodUID,omitempty"`
	PrimaryContainerID     string                    `json:"primaryContainerID,omitempty"`
	PrimaryRunID           string                    `json:"primaryRunId,omitempty"`
	PrimaryNodeName        string                    `json:"primaryNodeName,omitempty"`
	PrimaryNodeUID         string                    `json:"primaryNodeUID,omitempty"`
	ObservedGeneration     int64                     `json:"observedGeneration,omitempty"`
	AppliedPasswordVersion int64                     `json:"appliedPasswordVersion,omitempty"`
	AcceptedConfiguration  *AcceptedConfiguration    `json:"acceptedConfiguration,omitempty"`
	Applied                *AppliedConfiguration     `json:"applied,omitempty"`
	Network                *NetworkStatus            `json:"network,omitempty"`
	Failover               *FailoverStatus           `json:"failover,omitempty"`
	Rollout                *RolloutStatus            `json:"rollout,omitempty"`
	CredentialRotation     *CredentialRotationStatus `json:"credentialRotation,omitempty"`
	Deletion               *DeletionStatus           `json:"deletion,omitempty"`

	// +listType=map
	// +listMapKey=ordinal
	// +optional
	Nodes []NodeStatus `json:"nodes,omitempty"`

	// +listType=atomic
	// +optional
	PreviousProcesses []NodeStatus `json:"previousProcesses,omitempty"`

	// +listType=map
	// +listMapKey=ordinal
	// +kubebuilder:validation:MaxItems=3
	// +optional
	Metrics []NodeMetricStatus `json:"metrics,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vki
// +kubebuilder:printcolumn:name="Slug",type=string,JSONPath=`.spec.slug`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type ValkeyInstance struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec ValkeyInstanceSpec `json:"spec"`

	// +optional
	Status ValkeyInstanceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

type ValkeyInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ValkeyInstance `json:"items"`
}
