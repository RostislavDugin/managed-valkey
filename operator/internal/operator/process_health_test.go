package operator

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func Test_CT02_RecordProcessObservation_WhenResponseKindChanges_ResetsOrContinuesExpectedCounters(t *testing.T) {
	startedAt := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	observation := nextProcessObservation(
		nil,
		valkeyv1alpha1.ProcessObservationTransportError,
		startedAt,
	)
	observation = nextProcessObservation(
		observation,
		valkeyv1alpha1.ProcessObservationTransportError,
		startedAt.Add(time.Second),
	)
	if observation.ConsecutiveTransportErrors != 2 ||
		!observation.TransportErrorSince.Time.Equal(startedAt) {
		t.Fatalf("транспортная серия не продолжена: %+v", observation)
	}

	observation = nextProcessObservation(
		observation,
		valkeyv1alpha1.ProcessObservationBusy,
		startedAt.Add(2*time.Second),
	)
	if observation.ConsecutiveTransportErrors != 0 || observation.TransportErrorSince != nil ||
		observation.BusySince == nil ||
		!observation.BusySince.Time.Equal(startedAt.Add(2*time.Second)) {
		t.Fatalf("BUSY не сбросил отсутствие ответа: %+v", observation)
	}
	observation = nextProcessObservation(
		observation,
		valkeyv1alpha1.ProcessObservationBusy,
		startedAt.Add(3*time.Second),
	)
	if !observation.BusySince.Time.Equal(startedAt.Add(2 * time.Second)) {
		t.Fatalf("непрерывный BUSY начал новый отсчёт: %+v", observation)
	}

	for _, kind := range []valkeyv1alpha1.ProcessObservationKind{
		valkeyv1alpha1.ProcessObservationLoading,
		valkeyv1alpha1.ProcessObservationAuthError,
		valkeyv1alpha1.ProcessObservationServerError,
	} {
		observation = nextProcessObservation(observation, kind, startedAt.Add(4*time.Second))
		if observation.ConsecutiveTransportErrors != 0 || observation.TransportErrorSince != nil ||
			observation.BusySince != nil {
			t.Fatalf("ответ %q сохранил чужой таймер: %+v", kind, observation)
		}
	}

	observation = nextProcessObservation(
		observation,
		valkeyv1alpha1.ProcessObservationTransportError,
		startedAt.Add(5*time.Second),
	)
	if observation.ConsecutiveTransportErrors != 1 ||
		!observation.TransportErrorSince.Time.Equal(startedAt.Add(5*time.Second)) {
		t.Fatalf("новая транспортная серия продолжила старую: %+v", observation)
	}
}

func Test_ProcessObservationReason_WithEachObservationKind_ReturnsStatusContractReason(t *testing.T) {
	tests := []struct {
		kind   valkeyv1alpha1.ProcessObservationKind
		reason string
	}{
		{kind: valkeyv1alpha1.ProcessObservationTransportError, reason: "PROCESS_UNRESPONSIVE"},
		{kind: valkeyv1alpha1.ProcessObservationBusy, reason: "SCRIPT_BUSY"},
		{kind: valkeyv1alpha1.ProcessObservationLoading, reason: "DATA_LOADING"},
		{kind: valkeyv1alpha1.ProcessObservationAuthError, reason: "AUTH_FAILED"},
		{kind: valkeyv1alpha1.ProcessObservationServerError, reason: "PROCESS_RESPONSE_INVALID"},
	}
	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			if reason := processObservationReason(test.kind); reason != test.reason {
				t.Fatalf("причина %q: получено %q, ожидалось %q", test.kind, reason, test.reason)
			}
		})
	}
}

func Test_CT01CT02_ProcessRecoveryThresholds_WithPrimaryAndReplica_AreIndependent(t *testing.T) {
	startedAt := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	transportSince := metav1.NewTime(startedAt)
	transport := &valkeyv1alpha1.ProcessObservationStatus{
		Kind:                       valkeyv1alpha1.ProcessObservationTransportError,
		ConsecutiveTransportErrors: config.PrimaryFailureThreshold,
		TransportErrorSince:        &transportSince,
	}

	beforeTransport := processThresholds(
		transport,
		nil,
		startedAt.Add(config.PrimaryFailureMinDuration-time.Nanosecond),
	)
	if beforeTransport.TransportFailure || beforeTransport.Unresponsive {
		t.Fatalf("транспортный порог сработал раньше срока: %+v", beforeTransport)
	}
	atTransport := processThresholds(
		transport,
		nil,
		startedAt.Add(config.PrimaryFailureMinDuration),
	)
	if !atTransport.TransportFailure || atTransport.BusyIntervention ||
		atTransport.Unresponsive || atTransport.EmptyPrimary {
		t.Fatalf("десятисекундный порог смешан с другими: %+v", atTransport)
	}

	busySince := metav1.NewTime(startedAt)
	busy := &valkeyv1alpha1.ProcessObservationStatus{
		Kind: valkeyv1alpha1.ProcessObservationBusy, BusySince: &busySince,
	}
	beforeBusy := processThresholds(busy, nil, startedAt.Add(config.ScriptBusyTimeout-time.Nanosecond))
	if beforeBusy.BusyIntervention {
		t.Fatalf("BUSY разрешил вмешательство раньше срока: %+v", beforeBusy)
	}
	atBusy := processThresholds(busy, nil, startedAt.Add(config.ScriptBusyTimeout))
	if !atBusy.BusyIntervention || atBusy.TransportFailure ||
		atBusy.Unresponsive || atBusy.EmptyPrimary {
		t.Fatalf("тридцатисекундный порог смешан с другими: %+v", atBusy)
	}

	beforeUnresponsive := processThresholds(
		transport,
		nil,
		startedAt.Add(config.ProcessUnresponsiveTimeout-time.Nanosecond),
	)
	if beforeUnresponsive.Unresponsive {
		t.Fatalf("остановка разрешена раньше 60 секунд: %+v", beforeUnresponsive)
	}
	atUnresponsive := processThresholds(
		transport,
		nil,
		startedAt.Add(config.ProcessUnresponsiveTimeout),
	)
	if !atUnresponsive.TransportFailure || !atUnresponsive.Unresponsive ||
		atUnresponsive.BusyIntervention || atUnresponsive.EmptyPrimary {
		t.Fatalf("шестидесятисекундный порог смешан с другими: %+v", atUnresponsive)
	}

	emptySince := metav1.NewTime(startedAt)
	beforeEmpty := processThresholds(
		nil,
		&emptySince,
		startedAt.Add(config.EmptyPrimaryTimeout-time.Nanosecond),
	)
	if beforeEmpty.EmptyPrimary {
		t.Fatalf("пустой primary разрешён раньше двух минут: %+v", beforeEmpty)
	}
	atEmpty := processThresholds(nil, &emptySince, startedAt.Add(config.EmptyPrimaryTimeout))
	if !atEmpty.EmptyPrimary || atEmpty.TransportFailure ||
		atEmpty.BusyIntervention || atEmpty.Unresponsive {
		t.Fatalf("двухминутный порог смешан с другими: %+v", atEmpty)
	}

	transport.ConsecutiveTransportErrors = config.PrimaryFailureThreshold - 1
	if state := processThresholds(
		transport,
		nil,
		startedAt.Add(time.Hour),
	); state.TransportFailure ||
		state.Unresponsive {
		t.Fatalf("одного времени достаточно без трёх ошибок: %+v", state)
	}
}

func Test_CT02_RecordProcessObservation_WhenBusyThenSuccessful_PersistsBusyStateUntilSuccessClearsIt(t *testing.T) {
	ctx := context.Background()
	instance, pod, node, secret := processObservationObjects()
	pod.Finalizers = []string{processFinalizer}
	instance.Status.Initialized = true
	instance.Status.Phase = valkeyv1alpha1.InstancePhaseRunning
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, node, secret).
		Build()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		Clock:  clocktesting.NewFakeClock(now),
		InspectProcess: func(context.Context, string, string, string, bool) (operatorvalkey.ProcessState, error) {
			return operatorvalkey.ProcessState{}, operatorvalkey.ErrBusy
		},
	}

	result, err := reconciler.reconcileProcess(ctx, instance)
	if err != nil || result.RequeueAfter != config.HealthCheckInterval {
		t.Fatalf("сохранить BUSY: result=%+v error=%v", result, err)
	}
	observed := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observed); err != nil {
		t.Fatalf("прочитать BUSY из status: %v", err)
	}
	if len(observed.Status.Nodes) != 1 || observed.Status.Nodes[0].Observation == nil ||
		observed.Status.Nodes[0].Observation.Kind != valkeyv1alpha1.ProcessObservationBusy ||
		observed.Status.Nodes[0].Observation.BusySince == nil ||
		!observed.Status.Nodes[0].Observation.BusySince.Time.Equal(now) ||
		observed.Status.Phase != valkeyv1alpha1.InstancePhaseUnavailable ||
		observed.Status.Reason != "SCRIPT_BUSY" {
		t.Fatalf("BUSY сохранён неверно: %+v", observed.Status)
	}

	reconciler.InspectProcess = func(context.Context, string, string, string, bool) (operatorvalkey.ProcessState, error) {
		return operatorvalkey.ProcessState{Role: "primary", RunID: "run-1"}, nil
	}
	result, err = reconciler.reconcileProcess(ctx, observed)
	if err != nil || !result.IsZero() {
		t.Fatalf("сохранить успешный ответ: result=%+v error=%v", result, err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observed); err != nil {
		t.Fatalf("прочитать исправный status: %v", err)
	}
	if observed.Status.Nodes[0].Observation != nil {
		t.Fatalf("успешный ответ не сбросил BUSY: %+v", observed.Status.Nodes[0].Observation)
	}
}

func Test_OP08_RecordProcessObservation_AfterManagerRestart_StartsNewTransportFailureSeries(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	instance, pod, node, _ := processObservationObjects()
	instance.Status.Initialized = true
	instance.Status.Phase = valkeyv1alpha1.InstancePhaseRunning
	process := testObservedNode(pod)
	oldSince := metav1.NewTime(now.Add(-time.Hour))
	process.Observation = &valkeyv1alpha1.ProcessObservationStatus{
		Kind:                       valkeyv1alpha1.ProcessObservationTransportError,
		ConsecutiveTransportErrors: config.PrimaryFailureThreshold,
		TransportErrorSince:        &oldSince, ObservedAt: oldSince,
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{process}
	ordinal := int32(0)
	instance.Status.PrimaryOrdinal = &ordinal
	setPrimaryIdentity(&instance.Status, process)
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s, Clock: clocktesting.NewFakeClock(now), ObservationStartedAt: now,
	}

	container := &pod.Status.ContainerStatuses[0]
	result, err := reconciler.recordProcessObservationError(
		ctx,
		instance,
		pod,
		node,
		container,
		0,
		operatorvalkey.ErrTransportFailure,
	)
	if err != nil || result.RequeueAfter != config.HealthCheckInterval {
		t.Fatalf("сохранить первую ошибку нового процесса оператора: result=%+v error=%v", result, err)
	}
	observation := instance.Status.Nodes[0].Observation
	if observation == nil || observation.ConsecutiveTransportErrors != 1 ||
		observation.TransportErrorSince == nil || !observation.TransportErrorSince.Time.Equal(now) {
		t.Fatalf("старая серия продолжилась после рестарта: %+v", observation)
	}
	if instance.Status.Phase != valkeyv1alpha1.InstancePhaseRunning {
		t.Fatalf("первая транспортная ошибка изменила фазу на %q", instance.Status.Phase)
	}
}

func Test_RecordProcessObservation_WhenHealthyIdentityReplacesFailedProcess_ClearsObservationState(t *testing.T) {
	process := valkeyv1alpha1.NodeStatus{
		Ordinal: 0, PodUID: "pod-1", ContainerID: "containerd://1", RunID: "run-1",
		NodeName: "worker-1", NodeUID: "node-1",
		Observation: &valkeyv1alpha1.ProcessObservationStatus{
			Kind: valkeyv1alpha1.ProcessObservationTransportError,
		},
	}
	status := valkeyv1alpha1.ValkeyInstanceStatus{Nodes: []valkeyv1alpha1.NodeStatus{process}}
	healthy := process
	healthy.Observation = nil
	recordCurrentProcess(&status, healthy, nil)
	if status.Nodes[0].Observation != nil {
		t.Fatalf("успешное наблюдение сохранило ошибку: %+v", status.Nodes[0].Observation)
	}
}
