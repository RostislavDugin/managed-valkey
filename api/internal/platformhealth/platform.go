package platformhealth

import (
	"context"
	"errors"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

const systemNamespace = "valkey-system"

type Clock interface {
	Now() time.Time
}

type Repository interface {
	Ping(context.Context) error
	ListValkeyInstancesForHealth(context.Context) ([]store.ValkeyInstance, error)
}

type KubernetesProbe interface {
	Check(context.Context) error
}

type KubernetesReader interface {
	Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error
}

type NamespaceProbe struct {
	reader KubernetesReader
}

func NewNamespaceProbe(reader KubernetesReader) NamespaceProbe {
	return NamespaceProbe{reader: reader}
}

func (p NamespaceProbe) Check(ctx context.Context) error {
	return p.reader.Get(ctx, client.ObjectKey{Name: systemNamespace}, &corev1.Namespace{})
}

type Service struct {
	repository        Repository
	kubernetes        KubernetesProbe
	kubernetesEnabled bool
	clock             Clock
}

func NewService(repository Repository, kubernetes KubernetesProbe, kubernetesEnabled bool, clock Clock) *Service {
	return &Service{
		repository:        repository,
		kubernetes:        kubernetes,
		kubernetesEnabled: kubernetesEnabled,
		clock:             clock,
	}
}

type databaseResult struct {
	check     Check
	instances []store.ValkeyInstance
}

func (s *Service) Check(ctx context.Context) PlatformReport {
	checkedAt := s.clock.Now().UTC()
	checkContext, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	databaseResults := make(chan databaseResult, 1)
	kubernetesResults := make(chan Check, 1)
	go func() {
		databaseResults <- s.checkDatabase(checkContext)
	}()
	go func() {
		kubernetesResults <- s.checkKubernetes(checkContext)
	}()

	var database databaseResult
	var kubernetes Check
	databaseDone := false
	kubernetesDone := false
	for !databaseDone || !kubernetesDone {
		select {
		case database = <-databaseResults:
			databaseDone = true
		case kubernetes = <-kubernetesResults:
			kubernetesDone = true
		case <-checkContext.Done():
			if !databaseDone {
				database = databaseResult{check: failedCheck("timeout", RequestTimeout)}
				databaseDone = true
			}
			if !kubernetesDone {
				kubernetes = failedCheck("timeout", RequestTimeout)
				kubernetesDone = true
			}
		}
	}

	operations := unknownCheck()
	instances := unknownCheck()
	if database.check.Status == StatusOK {
		operations, instances = evaluateInstances(database.instances, checkedAt)
	}

	checks := PlatformChecks{
		PostgreSQL: database.check,
		Kubernetes: kubernetes,
		Operations: operations,
		Instances:  instances,
	}

	return PlatformReport{Status: aggregateStatus(checks), CheckedAt: checkedAt, Checks: checks}
}

func (s *Service) checkDatabase(ctx context.Context) databaseResult {
	started := time.Now()
	if err := s.repository.Ping(ctx); err != nil {
		return databaseResult{check: errorCheck(err, "unavailable", started)}
	}

	instances, err := s.repository.ListValkeyInstancesForHealth(ctx)
	if err != nil {
		return databaseResult{check: errorCheck(err, "query_failed", started)}
	}

	return databaseResult{check: okCheck(time.Since(started)), instances: instances}
}

func (s *Service) checkKubernetes(ctx context.Context) Check {
	if !s.kubernetesEnabled {
		return checkWithIssue(StatusFail, "disabled", 0)
	}
	if s.kubernetes == nil {
		return checkWithIssue(StatusFail, "unavailable", 0)
	}

	started := time.Now()
	if err := s.kubernetes.Check(ctx); err != nil {
		return errorCheck(err, "unavailable", started)
	}

	return okCheck(time.Since(started))
}

func evaluateInstances(instances []store.ValkeyInstance, now time.Time) (Check, Check) {
	operations := Check{Status: StatusOK, Issues: []Issue{}}
	states := Check{Status: StatusOK, Issues: []Issue{}}

	for _, instance := range instances {
		if instance.DeletionRequestedAt != nil {
			operations.Issues = append(
				operations.Issues,
				operationIssue(instance, "delete", *instance.DeletionRequestedAt, now),
			)

			continue
		}

		configurationActive := instance.Phase == domain.ValkeyInstancePhaseProvisioning ||
			instance.Phase == domain.ValkeyInstancePhaseUpdating ||
			instance.DesiredGeneration != instance.ObservedGeneration ||
			instance.PasswordVersion != instance.AppliedPasswordVersion
		if configurationActive {
			operations.Issues = append(
				operations.Issues,
				operationIssue(instance, "configuration", instance.ConfigurationRequestedAt, now),
			)
		}

		switch instance.Phase {
		case domain.ValkeyInstancePhaseError:
			states.Issues = append(
				states.Issues,
				instanceIssue(instance, "instance_error", reason(instance.PhaseReason)),
			)
		case domain.ValkeyInstancePhaseUnavailable:
			states.Issues = append(
				states.Issues,
				instanceIssue(instance, "instance_unavailable", reason(instance.PhaseReason)),
			)
		case domain.ValkeyInstancePhaseDegraded:
			states.Issues = append(
				states.Issues,
				instanceIssue(instance, "instance_degraded", reason(instance.PhaseReason)),
			)
		}

		if instance.IsRecoveryRequired {
			states.Issues = append(
				states.Issues,
				instanceIssue(instance, "sync_recovery_required", reason(instance.SyncRecoveryReason)),
			)
		}
		if instance.IsOperatorRecoveryRequired {
			states.Issues = append(
				states.Issues,
				instanceIssue(instance, "operator_recovery_required", reason(instance.OperatorRecoveryReason)),
			)
		}
		if !configurationActive && (instance.ObservedAt == nil || now.Sub(*instance.ObservedAt) > ObservationMaxAge) {
			issue := instanceIssue(instance, "observation_stale", "")
			if instance.ObservedAt != nil {
				issue.AgeSeconds = ageSeconds(*instance.ObservedAt, now)
			}
			states.Issues = append(states.Issues, issue)
		}
	}

	sortIssues(operations.Issues)
	sortIssues(states.Issues)
	operations.Status = issueStatus(operations.Issues)
	states.Status = issueStatus(states.Issues)

	return operations, states
}

func operationIssue(instance store.ValkeyInstance, operation string, startedAt, now time.Time) Issue {
	code := "operation_in_progress"
	if now.Sub(startedAt) >= OperationDeadline {
		code = "operation_stalled"
	}

	return Issue{
		Code:       code,
		InstanceID: instance.ID.String(),
		Slug:       instance.Slug,
		Operation:  operation,
		AgeSeconds: ageSeconds(startedAt, now),
	}
}

func instanceIssue(instance store.ValkeyInstance, code, issueReason string) Issue {
	return Issue{
		Code:       code,
		InstanceID: instance.ID.String(),
		Slug:       instance.Slug,
		Phase:      string(instance.Phase),
		Reason:     issueReason,
	}
}

func reason(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}

func ageSeconds(startedAt, now time.Time) int64 {
	age := now.Sub(startedAt)
	if age < 0 {
		return 0
	}

	return int64(age / time.Second)
}

func sortIssues(issues []Issue) {
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Slug != issues[j].Slug {
			return issues[i].Slug < issues[j].Slug
		}

		return issues[i].Code < issues[j].Code
	})
}

func issueStatus(issues []Issue) Status {
	status := StatusOK
	for _, issue := range issues {
		switch issue.Code {
		case "operation_in_progress", "instance_degraded":
			if status == StatusOK {
				status = StatusWarning
			}
		default:
			return StatusFail
		}
	}

	return status
}

func aggregateStatus(checks PlatformChecks) Status {
	status := StatusOK
	for _, check := range []Check{checks.PostgreSQL, checks.Kubernetes, checks.Operations, checks.Instances} {
		if check.Status == StatusFail {
			return StatusFail
		}
		if check.Status == StatusWarning {
			status = StatusWarning
		}
	}

	return status
}

func okCheck(elapsed time.Duration) Check {
	return Check{Status: StatusOK, LatencyMS: elapsed.Milliseconds(), Issues: []Issue{}}
}

func unknownCheck() Check {
	return Check{Status: StatusUnknown, Issues: []Issue{}}
}

func failedCheck(code string, elapsed time.Duration) Check {
	return checkWithIssue(StatusFail, code, elapsed)
}

func errorCheck(err error, fallback string, started time.Time) Check {
	code := fallback
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		code = "timeout"
	}

	return checkWithIssue(StatusFail, code, time.Since(started))
}

func checkWithIssue(status Status, code string, elapsed time.Duration) Check {
	return Check{Status: status, LatencyMS: elapsed.Milliseconds(), Issues: []Issue{{Code: code}}}
}
