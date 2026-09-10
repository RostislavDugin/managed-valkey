//go:build !integration

package config

import "time"

const (
	MetricsInterval            = 10 * time.Second
	HealthCheckInterval        = time.Second
	ValkeyDialTimeout          = time.Second
	ValkeyCommandTimeout       = 3 * time.Second
	PrimaryFailureMinDuration  = 10 * time.Second
	ScriptBusyTimeout          = 30 * time.Second
	ProcessUnresponsiveTimeout = 60 * time.Second
	NetworkVerifyInterval      = 10 * time.Second
	NetworkVerifyTimeout       = 3 * time.Second
	ProvisionTimeout           = 10 * time.Minute
	UnschedulableTimeout       = 30 * time.Second
	NodeUnreachableTimeout     = 60 * time.Second
	EmptyPrimaryTimeout        = 2 * time.Minute
	FailoverTimeout            = 5 * time.Second
	ProcessDeletionGracePeriod = int64(30)
	ReadinessCommandTimeout    = 2
	ReadinessProbePeriod       = 5
	ReadinessProbeTimeout      = 3
	ReadinessProbeFailures     = 3
)
