package domain

type ValkeyInstanceMode string

const (
	ValkeyInstanceModeSingle ValkeyInstanceMode = "single"
	ValkeyInstanceModeHA     ValkeyInstanceMode = "ha"
)

type ValkeyInstancePhase string

const (
	ValkeyInstancePhaseProvisioning ValkeyInstancePhase = "provisioning"
	ValkeyInstancePhaseRunning      ValkeyInstancePhase = "running"
	ValkeyInstancePhaseUpdating     ValkeyInstancePhase = "updating"
	ValkeyInstancePhaseDegraded     ValkeyInstancePhase = "degraded"
	ValkeyInstancePhaseUnavailable  ValkeyInstancePhase = "unavailable"
	ValkeyInstancePhaseError        ValkeyInstancePhase = "error"
)

type ValkeyInstanceStatus string

const (
	ValkeyInstanceStatusProvisioning ValkeyInstanceStatus = ValkeyInstanceStatus(ValkeyInstancePhaseProvisioning)
	ValkeyInstanceStatusRunning      ValkeyInstanceStatus = ValkeyInstanceStatus(ValkeyInstancePhaseRunning)
	ValkeyInstanceStatusUpdating     ValkeyInstanceStatus = ValkeyInstanceStatus(ValkeyInstancePhaseUpdating)
	ValkeyInstanceStatusDegraded     ValkeyInstanceStatus = ValkeyInstanceStatus(ValkeyInstancePhaseDegraded)
	ValkeyInstanceStatusUnavailable  ValkeyInstanceStatus = ValkeyInstanceStatus(ValkeyInstancePhaseUnavailable)
	ValkeyInstanceStatusError        ValkeyInstanceStatus = ValkeyInstanceStatus(ValkeyInstancePhaseError)
	ValkeyInstanceStatusDeleting     ValkeyInstanceStatus = "deleting"
	ValkeyInstanceStatusDeleted      ValkeyInstanceStatus = "deleted"
)

type ValkeyNetworkVerificationStatus string

const (
	ValkeyNetworkVerificationPending  ValkeyNetworkVerificationStatus = "pending"
	ValkeyNetworkVerificationVerified ValkeyNetworkVerificationStatus = "verified"
	ValkeyNetworkVerificationUnknown  ValkeyNetworkVerificationStatus = "unknown"
)

type ValkeyNodeRole string

const (
	ValkeyNodeRolePrimary ValkeyNodeRole = "primary"
	ValkeyNodeRoleReplica ValkeyNodeRole = "replica"
	ValkeyNodeRoleUnknown ValkeyNodeRole = "unknown"
)
