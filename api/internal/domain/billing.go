package domain

type ManagedService string

const ManagedServiceValkey ManagedService = "valkey"

type BillingPeriodStartReason string

const (
	BillingPeriodStartReasonCreated BillingPeriodStartReason = "created"
	BillingPeriodStartReasonResized BillingPeriodStartReason = "resized"
)

type BillingPeriodEndReason string

const (
	BillingPeriodEndReasonResized BillingPeriodEndReason = "resized"
	BillingPeriodEndReasonDeleted BillingPeriodEndReason = "deleted"
)
