//go:build integration

package config

import "time"

const (
	MetricsInterval            = time.Second
	HealthCheckInterval        = 250 * time.Millisecond
	ValkeyDialTimeout          = 500 * time.Millisecond
	ValkeyCommandTimeout       = time.Second
	PrimaryFailureMinDuration  = 3 * time.Second
	ScriptBusyTimeout          = 5 * time.Second
	ProcessUnresponsiveTimeout = 10 * time.Second
	NetworkVerifyInterval      = time.Second
	NetworkVerifyTimeout       = time.Second
	ProvisionTimeout           = 2 * time.Minute
	UnschedulableTimeout       = 5 * time.Second
	NodeUnreachableTimeout     = 10 * time.Second
	EmptyPrimaryTimeout        = 20 * time.Second
	FailoverTimeout            = time.Second
	ProcessDeletionGracePeriod = int64(2)
	ReadinessCommandTimeout    = 1
	ReadinessProbePeriod       = 1
	ReadinessProbeTimeout      = 5
	ReadinessProbeFailures     = 1
)
