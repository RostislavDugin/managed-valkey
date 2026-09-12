package platformhealth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time {
	return c.now
}

type repositoryStub struct {
	instances []store.ValkeyInstance
	ping      func(context.Context) error
	list      func(context.Context) ([]store.ValkeyInstance, error)
}

func (r repositoryStub) Ping(ctx context.Context) error {
	if r.ping != nil {
		return r.ping(ctx)
	}

	return nil
}

func (r repositoryStub) ListValkeyInstancesForHealth(ctx context.Context) ([]store.ValkeyInstance, error) {
	if r.list != nil {
		return r.list(ctx)
	}

	return r.instances, nil
}

type kubernetesProbeStub struct {
	check func(context.Context) error
}

func (p kubernetesProbeStub) Check(ctx context.Context) error {
	if p.check != nil {
		return p.check(ctx)
	}

	return nil
}

func Test_EvaluateInstances_WithOperationBoundaries_ReturnsExpectedStatus(t *testing.T) {
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name              string
		instance          store.ValkeyInstance
		operationsStatus  Status
		operationsCode    string
		instancesStatus   Status
		instancesCode     string
		instancesProblems int
	}{
		{
			name:             "создание длительностью 299 секунд остаётся предупреждением",
			instance:         instanceAt(now, domain.ValkeyInstancePhaseProvisioning, 299*time.Second),
			operationsStatus: StatusWarning, operationsCode: "operation_in_progress", instancesStatus: StatusOK,
		},
		{
			name:             "создание длительностью 300 секунд считается застрявшим",
			instance:         instanceAt(now, domain.ValkeyInstancePhaseProvisioning, OperationDeadline),
			operationsStatus: StatusFail, operationsCode: "operation_stalled", instancesStatus: StatusOK,
		},
		{
			name: "неприменённая версия пароля считается изменением конфигурации",
			instance: func() store.ValkeyInstance {
				instance := instanceAt(now, domain.ValkeyInstancePhaseRunning, 2*time.Minute)
				instance.DesiredGeneration = 2
				instance.ObservedGeneration = 2
				instance.PasswordVersion = 2
				instance.AppliedPasswordVersion = 1

				return instance
			}(),
			operationsStatus: StatusWarning, operationsCode: "operation_in_progress", instancesStatus: StatusOK,
		},
		{
			name: "удаление игнорирует прежнюю недоступность",
			instance: func() store.ValkeyInstance {
				instance := instanceAt(now, domain.ValkeyInstancePhaseUnavailable, 4*time.Minute)
				requestedAt := now.Add(-4 * time.Minute)
				instance.DeletionRequestedAt = &requestedAt

				return instance
			}(),
			operationsStatus: StatusWarning, operationsCode: "operation_in_progress", instancesStatus: StatusOK,
		},
		{
			name: "застрявшее удаление становится ошибкой без второй проблемы",
			instance: func() store.ValkeyInstance {
				instance := instanceAt(now, domain.ValkeyInstancePhaseError, OperationDeadline)
				requestedAt := now.Add(-OperationDeadline)
				instance.DeletionRequestedAt = &requestedAt

				return instance
			}(),
			operationsStatus: StatusFail, operationsCode: "operation_stalled", instancesStatus: StatusOK,
		},
		{
			name:              "degraded остаётся предупреждением",
			instance:          stableInstance(now, domain.ValkeyInstancePhaseDegraded, ObservationMaxAge),
			operationsStatus:  StatusOK,
			instancesStatus:   StatusWarning,
			instancesCode:     "instance_degraded",
			instancesProblems: 1,
		},
		{
			name:              "error становится ошибкой",
			instance:          stableInstance(now, domain.ValkeyInstancePhaseError, ObservationMaxAge),
			operationsStatus:  StatusOK,
			instancesStatus:   StatusFail,
			instancesCode:     "instance_error",
			instancesProblems: 1,
		},
		{
			name:              "unavailable становится ошибкой",
			instance:          stableInstance(now, domain.ValkeyInstancePhaseUnavailable, ObservationMaxAge),
			operationsStatus:  StatusOK,
			instancesStatus:   StatusFail,
			instancesCode:     "instance_unavailable",
			instancesProblems: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operations, instances := evaluateInstances([]store.ValkeyInstance{test.instance}, now)

			if operations.Status != test.operationsStatus {
				t.Errorf("статус операций %q, ожидался %q", operations.Status, test.operationsStatus)
			}
			if test.operationsCode != "" &&
				(len(operations.Issues) != 1 || operations.Issues[0].Code != test.operationsCode) {
				t.Errorf("проблемы операций %+v, ожидался код %q", operations.Issues, test.operationsCode)
			}
			if instances.Status != test.instancesStatus {
				t.Errorf("статус инстансов %q, ожидался %q", instances.Status, test.instancesStatus)
			}
			if len(instances.Issues) != test.instancesProblems {
				t.Fatalf(
					"проблем инстансов %d, ожидалось %d: %+v",
					len(instances.Issues),
					test.instancesProblems,
					instances.Issues,
				)
			}
			if test.instancesCode != "" && instances.Issues[0].Code != test.instancesCode {
				t.Errorf("код проблемы %q, ожидался %q", instances.Issues[0].Code, test.instancesCode)
			}
		})
	}
}

func Test_EvaluateInstances_WithObservationBoundaries_ReturnsExpectedStatus(t *testing.T) {
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		age    time.Duration
		status Status
		code   string
	}{
		{name: "наблюдение возрастом 60 секунд остаётся свежим", age: ObservationMaxAge, status: StatusOK},
		{
			name:   "наблюдение старше 60 секунд считается устаревшим",
			age:    ObservationMaxAge + time.Second,
			status: StatusFail,
			code:   "observation_stale",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, instances := evaluateInstances(
				[]store.ValkeyInstance{stableInstance(now, domain.ValkeyInstancePhaseRunning, test.age)},
				now,
			)
			if instances.Status != test.status {
				t.Errorf("статус %q, ожидался %q", instances.Status, test.status)
			}
			if test.code != "" && (len(instances.Issues) != 1 || instances.Issues[0].Code != test.code) {
				t.Errorf("проблемы %+v, ожидался код %q", instances.Issues, test.code)
			}
		})
	}
}

func Test_EvaluateInstances_WithoutObservation_ReturnsStaleFailure(t *testing.T) {
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	instance := stableInstance(now, domain.ValkeyInstancePhaseRunning, time.Second)
	instance.ObservedAt = nil

	_, instances := evaluateInstances([]store.ValkeyInstance{instance}, now)

	if instances.Status != StatusFail || len(instances.Issues) != 1 ||
		instances.Issues[0].Code != "observation_stale" {
		t.Errorf("неверный результат отсутствующего наблюдения: %+v", instances)
	}
}

func Test_AggregateStatus_WithWarningsAndFailures_UsesHighestSeverity(t *testing.T) {
	checks := PlatformChecks{
		PostgreSQL: Check{Status: StatusOK},
		Kubernetes: Check{Status: StatusOK},
		Operations: Check{Status: StatusWarning},
		Instances:  Check{Status: StatusOK},
	}
	if status := aggregateStatus(checks); status != StatusWarning {
		t.Errorf("статус %q, ожидался warning", status)
	}

	checks.Instances.Status = StatusFail
	if status := aggregateStatus(checks); status != StatusFail {
		t.Errorf("статус %q, ожидался fail", status)
	}
}

func Test_EvaluateInstances_WithRecoveryFlags_ReturnsSeparateFailures(t *testing.T) {
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	instance := stableInstance(now, domain.ValkeyInstancePhaseRunning, time.Second)
	instance.IsRecoveryRequired = true
	instance.IsOperatorRecoveryRequired = true
	syncReason := "NamespaceIdentityMismatch"
	operatorReason := "ManualRecoveryRequired"
	instance.SyncRecoveryReason = &syncReason
	instance.OperatorRecoveryReason = &operatorReason

	_, instances := evaluateInstances([]store.ValkeyInstance{instance}, now)

	if instances.Status != StatusFail || len(instances.Issues) != 2 {
		t.Fatalf("неверный результат восстановления: %+v", instances)
	}
	if instances.Issues[0].Code != "operator_recovery_required" ||
		instances.Issues[1].Code != "sync_recovery_required" {
		t.Errorf("проблемы не отсортированы или имеют неверные коды: %+v", instances.Issues)
	}
}

func Test_CheckPlatformHealth_WithConcurrentDependencies_WaitsForBoth(t *testing.T) {
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	databaseStarted := make(chan struct{})
	kubernetesStarted := make(chan struct{})
	release := make(chan struct{})
	repository := repositoryStub{ping: func(context.Context) error {
		close(databaseStarted)
		<-release

		return nil
	}}
	kubernetes := kubernetesProbeStub{check: func(context.Context) error {
		close(kubernetesStarted)
		<-release

		return nil
	}}
	service := NewService(repository, kubernetes, true, fixedClock{now: now})
	reports := make(chan PlatformReport, 1)
	go func() {
		reports <- service.Check(context.Background())
	}()

	waitStarted(t, databaseStarted)
	waitStarted(t, kubernetesStarted)
	close(release)
	report := <-reports

	if report.Status != StatusOK || report.Checks.PostgreSQL.Status != StatusOK ||
		report.Checks.Kubernetes.Status != StatusOK {
		t.Errorf("неверный успешный результат: %+v", report)
	}
}

func Test_CheckPlatformHealth_WhenPostgreSqlFails_StillChecksKubernetes(t *testing.T) {
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	kubernetesChecked := false
	service := NewService(
		repositoryStub{ping: func(context.Context) error { return errors.New("database secret") }},
		kubernetesProbeStub{check: func(context.Context) error {
			kubernetesChecked = true

			return nil
		}},
		true,
		fixedClock{now: now},
	)

	report := service.Check(context.Background())

	if !kubernetesChecked || report.Checks.Kubernetes.Status != StatusOK {
		t.Errorf("Kubernetes не проверен: %+v", report)
	}
	if report.Status != StatusFail || report.Checks.Operations.Status != StatusUnknown ||
		report.Checks.Instances.Status != StatusUnknown {
		t.Errorf("неверная зависимость результатов от PostgreSQL: %+v", report)
	}
}

func Test_CheckPlatformHealth_WhenKubernetesIsDisabled_ReturnsFailureWithoutChangingDatabaseResult(t *testing.T) {
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	service := NewService(repositoryStub{}, nil, false, fixedClock{now: now})

	report := service.Check(context.Background())

	if report.Status != StatusFail || report.Checks.Kubernetes.Status != StatusFail ||
		len(report.Checks.Kubernetes.Issues) != 1 || report.Checks.Kubernetes.Issues[0].Code != "disabled" {
		t.Errorf("неверный результат выключенного Kubernetes: %+v", report)
	}
	if report.Checks.PostgreSQL.Status != StatusOK || report.Checks.Operations.Status != StatusOK {
		t.Errorf("успешные результаты базы потеряны: %+v", report)
	}
}

func Test_CheckPlatformHealth_WhenRequestExpires_CancelsDependenciesAndReturnsTimeouts(t *testing.T) {
	now := time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC)
	blocked := func(ctx context.Context) error {
		<-ctx.Done()

		return ctx.Err()
	}
	service := NewService(
		repositoryStub{ping: blocked},
		kubernetesProbeStub{check: blocked},
		true,
		fixedClock{now: now},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	report := service.Check(ctx)

	if report.Status != StatusFail || report.Checks.PostgreSQL.Issues[0].Code != "timeout" ||
		report.Checks.Kubernetes.Issues[0].Code != "timeout" {
		t.Errorf("неверный результат таймаута: %+v", report)
	}
}

func instanceAt(now time.Time, phase domain.ValkeyInstancePhase, operationAge time.Duration) store.ValkeyInstance {
	observedAt := now

	return store.ValkeyInstance{
		ID:                       uuid.MustParse("01994180-0000-7000-8000-000000000001"),
		Slug:                     "cache-abc123",
		Phase:                    phase,
		DesiredGeneration:        1,
		ObservedGeneration:       0,
		PasswordVersion:          1,
		AppliedPasswordVersion:   1,
		ConfigurationRequestedAt: now.Add(-operationAge),
		ObservedAt:               &observedAt,
	}
}

func stableInstance(
	now time.Time,
	phase domain.ValkeyInstancePhase,
	observationAge time.Duration,
) store.ValkeyInstance {
	instance := instanceAt(now, phase, time.Second)
	observedAt := now.Add(-observationAge)
	instance.DesiredGeneration = 1
	instance.ObservedGeneration = 1
	instance.ObservedAt = &observedAt

	return instance
}

func waitStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("зависимость не начала проверку")
	}
}
