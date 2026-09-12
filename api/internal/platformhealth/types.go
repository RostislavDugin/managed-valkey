package platformhealth

import "time"

const (
	RequestTimeout    = 5 * time.Second
	OperationDeadline = 5 * time.Minute
	ObservationMaxAge = time.Minute
)

type Status string

const (
	StatusOK      Status = "ok"
	StatusWarning Status = "warning"
	StatusFail    Status = "fail"
	StatusUnknown Status = "unknown"
)

type Issue struct {
	Code       string `json:"code"`
	InstanceID string `json:"instance_id,omitempty"`
	Slug       string `json:"slug,omitempty"`
	Operation  string `json:"operation,omitempty"`
	Phase      string `json:"phase,omitempty"`
	Reason     string `json:"reason,omitempty"`
	AgeSeconds int64  `json:"age_seconds,omitempty"`
}

type Check struct {
	Status    Status  `json:"status"`
	LatencyMS int64   `json:"latency_ms,omitempty"`
	Issues    []Issue `json:"issues"`
}

type PlatformChecks struct {
	PostgreSQL Check `json:"postgresql"`
	Kubernetes Check `json:"kubernetes"`
	Operations Check `json:"operations"`
	Instances  Check `json:"instances"`
}

type PlatformReport struct {
	Status    Status         `json:"status"`
	CheckedAt time.Time      `json:"checked_at"`
	Checks    PlatformChecks `json:"checks"`
}

type ValkeyTargetCheck struct {
	Status    Status `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Code      string `json:"code,omitempty"`
}

type ValkeyChecks struct {
	Primary ValkeyTargetCheck  `json:"primary"`
	Read    *ValkeyTargetCheck `json:"read,omitempty"`
}

type ValkeyReport struct {
	Status    Status       `json:"status"`
	CheckedAt time.Time    `json:"checked_at"`
	Checks    ValkeyChecks `json:"checks"`
}
