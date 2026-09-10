package operator

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func Test_ValkeyMetricsReconcile_WithCurrentProcess_PublishesSnapshotAndUsesConfiguredInterval(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 123456000, time.UTC)
	instance, pod, secret := metricTestObjects(now)
	instance.Status.Reason = "UNCHANGED"
	k8s := metricTestClient(instance, pod, secret)
	reconciler := &ValkeyMetricsReconciler{
		Client:    k8s,
		APIReader: k8s,
		Clock:     clocktesting.NewFakeClock(now),
		Interval:  17 * time.Second,
		PodMetrics: podMetricsReaderFunc(
			func(_ context.Context, namespace, name string) (*metricsv1beta1.PodMetrics, error) {
				if namespace != pod.Namespace || name != pod.Name {
					t.Fatalf("запрошены метрики %s/%s", namespace, name)
				}

				return podCPU(now.Add(-time.Second), "250m"), nil
			},
		),
		OpenSession: func(_ context.Context, address, password string) (MetricsSession, error) {
			if address != net.JoinHostPort(pod.Status.PodIP, "6379") || password == "" {
				t.Fatalf("неверные параметры Valkey: address=%q password_empty=%t", address, password == "")
			}

			return successfulMetricsSession(), nil
		},
	}

	result, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
	if err != nil || result.RequeueAfter != 17*time.Second {
		t.Fatalf("согласовать метрики: result=%+v error=%v", result, err)
	}
	observed := getMetricInstance(t, k8s, instance)
	if len(observed.Status.Metrics) != 1 {
		t.Fatalf("записано снимков: %d", len(observed.Status.Metrics))
	}
	metric := observed.Status.Metrics[0]
	if metric.Ordinal != 0 || metric.PodUID != string(pod.UID) ||
		metric.ContainerID != pod.Status.ContainerStatuses[0].ContainerID || metric.RunID != "run-1" ||
		metric.Role != valkeyv1alpha1.NodeRolePrimary || metric.UsedMemoryBytes != 1024 ||
		metric.MaxmemoryBytes != 2048 || metric.ConnectedClients != 7 || metric.OpsPerSec != 8 ||
		metric.KeyspaceHits != 9 || metric.KeyspaceMisses != 10 || metric.EvictedKeys != 11 ||
		metric.CPUMillicores == nil || *metric.CPUMillicores != 250 || !metric.CollectedAt.Time.Equal(now) {
		t.Fatalf("неверный снимок: %+v", metric)
	}
	if observed.Status.Reason != "UNCHANGED" {
		t.Fatalf("соседнее поле status изменилось: %+v", observed.Status)
	}
}

func Test_ValkeyMetricsReconcile_WhenLeaseIsInactive_WaitsWithoutCollecting(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	instance, pod, secret := metricTestObjects(now)
	k8s := metricTestClient(instance, pod, secret)
	var calls atomic.Int32
	reconciler := &ValkeyMetricsReconciler{
		Client: k8s, APIReader: k8s, Lease: NewLeaseScope(), Interval: 23 * time.Second,
		OpenSession: func(context.Context, string, string) (MetricsSession, error) {
			calls.Add(1)

			return successfulMetricsSession(), nil
		},
	}

	result, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
	if err != nil || result.RequeueAfter != 23*time.Second || calls.Load() != 0 {
		t.Fatalf("неактивный LeaseScope: result=%+v calls=%d error=%v", result, calls.Load(), err)
	}
}

func Test_CollectNodeMetric_WhenProcessIdentityChanges_DiscardsSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		changeBefore func(*corev1.Pod)
		changeAfter  func(*corev1.Pod)
		metricsRunID string
		verification error
	}{
		{name: "Pod UID изменился до чтения", changeBefore: func(pod *corev1.Pod) { pod.UID = "pod-2" }},
		{name: "container ID изменился до чтения", changeBefore: func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses[0].ContainerID = "containerd://2"
		}},
		{name: "run_id изменился в полном INFO", metricsRunID: "run-2"},
		{name: "run_id изменился при повторной проверке", verification: operatorvalkey.ErrProcessChanged},
		{name: "Pod UID изменился после чтения", changeAfter: func(pod *corev1.Pod) { pod.UID = "pod-2" }},
		{name: "container ID изменился после чтения", changeAfter: func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses[0].ContainerID = "containerd://2"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance, pod, secret := metricTestObjects(now)
			node := instance.Status.Nodes[0]
			if test.changeBefore != nil {
				test.changeBefore(pod)
			}
			base := metricTestClient(instance, pod, secret)
			reader := client.Reader(base)
			if test.changeAfter != nil {
				reader = &secondPodReadMutation{Reader: base, mutate: test.changeAfter}
			}
			runID := test.metricsRunID
			if runID == "" {
				runID = "run-1"
			}
			reconciler := &ValkeyMetricsReconciler{
				Client: base, APIReader: reader, Clock: clocktesting.NewFakeClock(now),
				OpenSession: func(context.Context, string, string) (MetricsSession, error) {
					return &fakeMetricsSession{
						metrics: metricValues(runID), verification: test.verification,
					}, nil
				},
			}

			_, err := reconciler.collectNodeMetric(t.Context(), instance, node, "operator-password")
			if !errors.Is(err, operatorvalkey.ErrProcessChanged) {
				t.Fatalf("смена идентичности вернула %v", err)
			}
		})
	}
}

func Test_ValkeyMetricsReconcile_WithCollectionFailure_PreservesOnlyCurrentProcessSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		currentRunID  string
		wantSnapshots int
	}{
		{name: "ошибка INFO сохраняет снимок текущего процесса", currentRunID: "run-1", wantSnapshots: 1},
		{name: "ошибка INFO удаляет снимок прежнего процесса", currentRunID: "run-2", wantSnapshots: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance, pod, secret := metricTestObjects(now)
			instance.Status.Nodes[0].RunID = test.currentRunID
			instance.Status.Metrics = []valkeyv1alpha1.NodeMetricStatus{metricStatus(now.Add(-time.Minute), "run-1")}
			k8s := metricTestClient(instance, pod, secret)
			reconciler := &ValkeyMetricsReconciler{
				Client: k8s, APIReader: k8s,
				OpenSession: func(context.Context, string, string) (MetricsSession, error) {
					return &fakeMetricsSession{readError: errors.New("INFO недоступен")}, nil
				},
			}

			if _, err := reconciler.Reconcile(t.Context(), ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(instance),
			}); err != nil {
				t.Fatalf("согласовать ошибку INFO: %v", err)
			}
			observed := getMetricInstance(t, k8s, instance)
			if len(observed.Status.Metrics) != test.wantSnapshots {
				t.Fatalf("сохранены снимки: %+v", observed.Status.Metrics)
			}
			if test.wantSnapshots == 1 && !observed.Status.Metrics[0].CollectedAt.Time.Equal(now.Add(-time.Minute)) {
				t.Fatalf("время старого снимка изменилось: %+v", observed.Status.Metrics[0])
			}
		})
	}
}

func Test_ValkeyMetricsReconcile_WhenCPUIsUnavailable_PublishesSnapshotWithNilCPU(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	instance, pod, secret := metricTestObjects(now)
	k8s := metricTestClient(instance, pod, secret)
	reconciler := &ValkeyMetricsReconciler{
		Client: k8s, APIReader: k8s, Clock: clocktesting.NewFakeClock(now),
		PodMetrics: podMetricsReaderFunc(func(context.Context, string, string) (*metricsv1beta1.PodMetrics, error) {
			return nil, errors.New("metrics API недоступен")
		}),
		OpenSession: func(context.Context, string, string) (MetricsSession, error) {
			return successfulMetricsSession(), nil
		},
	}

	if _, err := reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(instance),
	}); err != nil {
		t.Fatalf("согласовать метрики без CPU: %v", err)
	}
	observed := getMetricInstance(t, k8s, instance)
	if len(observed.Status.Metrics) != 1 || observed.Status.Metrics[0].CPUMillicores != nil {
		t.Fatalf("ошибка CPU повредила снимок: %+v", observed.Status.Metrics)
	}
}

func Test_ValkeyMetricsReconcile_WhenStatusChangesBeforeWrite_DiscardsCollectedProcess(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	instance, pod, secret := metricTestObjects(now)
	instance.Status.Metrics = []valkeyv1alpha1.NodeMetricStatus{metricStatus(now.Add(-time.Minute), "run-1")}
	base := metricTestClient(instance, pod, secret)
	reconciler := &ValkeyMetricsReconciler{
		Client: base, APIReader: base, Clock: clocktesting.NewFakeClock(now),
		OpenSession: func(context.Context, string, string) (MetricsSession, error) {
			return &fakeMetricsSession{
				metrics: metricValues("run-1"),
				verify: func() error {
					current := getMetricInstance(t, base, instance)
					current.Status.Nodes[0].RunID = "run-2"

					return base.Status().Update(t.Context(), current)
				},
			}, nil
		},
	}

	if _, err := reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(instance),
	}); err != nil {
		t.Fatalf("согласовать метрики после смены status.nodes: %v", err)
	}
	observed := getMetricInstance(t, base, instance)
	if len(observed.Status.Metrics) != 0 || observed.Status.Nodes[0].RunID != "run-2" {
		t.Fatalf("снимок прежнего процесса не удалён: %+v", observed.Status)
	}
}

func Test_ValkeyMetricsReconcile_WhenStatusWriteConflicts_RetriesAndPreservesConcurrentFields(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	instance, pod, secret := metricTestObjects(now)
	base := metricTestClient(instance, pod, secret)
	reconciler := &ValkeyMetricsReconciler{
		Client: &conflictingStatusClient{Client: base}, APIReader: base,
		Clock: clocktesting.NewFakeClock(now),
		OpenSession: func(context.Context, string, string) (MetricsSession, error) {
			return successfulMetricsSession(), nil
		},
	}

	if _, err := reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(instance),
	}); err != nil {
		t.Fatalf("повторить запись status.metrics: %v", err)
	}
	observed := getMetricInstance(t, base, instance)
	if len(observed.Status.Metrics) != 1 || observed.Status.Reason != "CONCURRENT_OBSERVATION" {
		t.Fatalf("повтор потерял данные: %+v", observed.Status)
	}
}

func Test_ValkeyMetricsPredicate_WhenOnlyMetricsChange_IgnoresUpdate(t *testing.T) {
	filter := valkeyMetricsPredicate()
	before := completeAcceptedInstance()
	after := before.DeepCopy()
	after.Status.Metrics = []valkeyv1alpha1.NodeMetricStatus{metricStatus(time.Now().UTC(), "run-1")}
	if filter.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("собственная запись status.metrics запустила немедленный повтор")
	}
}

func Test_ValkeyMetricsPredicate_WhenNodeObservationChangesBeforeScheduledRun_IgnoresUpdate(t *testing.T) {
	filter := valkeyMetricsPredicate()
	before := completeAcceptedInstance()
	before.Status.Nodes = []valkeyv1alpha1.NodeStatus{{
		Ordinal: 0, PodUID: "pod-1", ContainerID: "containerd://1", RunID: "run-1",
	}}
	after := before.DeepCopy()
	after.Generation++
	after.Status.Nodes[0].Readiness = true
	if filter.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("обновление наблюдения status.nodes запустило сбор до очередного периода")
	}
}

func Test_ValkeyMetricsPredicate_WhenProcessIdentityChanges_AllowsImmediateUpdate(t *testing.T) {
	filter := valkeyMetricsPredicate()
	changes := []struct {
		name   string
		change func(*valkeyv1alpha1.NodeStatus)
	}{
		{name: "сменился podUID", change: func(node *valkeyv1alpha1.NodeStatus) { node.PodUID = "pod-2" }},
		{name: "сменился containerID", change: func(node *valkeyv1alpha1.NodeStatus) {
			node.ContainerID = "containerd://2"
		}},
		{name: "сменился runId", change: func(node *valkeyv1alpha1.NodeStatus) { node.RunID = "run-2" }},
	}
	for _, test := range changes {
		t.Run(test.name, func(t *testing.T) {
			before := completeAcceptedInstance()
			before.Status.Nodes = []valkeyv1alpha1.NodeStatus{{
				Ordinal: 0, PodUID: "pod-1", ContainerID: "containerd://1", RunID: "run-1",
			}}
			after := before.DeepCopy()
			test.change(&after.Status.Nodes[0])
			if !filter.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
				t.Fatal("смена идентичности процесса не запустила ближайший сбор")
			}
		})
	}
}

func metricTestObjects(now time.Time) (
	*valkeyv1alpha1.ValkeyInstance,
	*corev1.Pod,
	*corev1.Secret,
) {
	instance, pod, _, secret := processObservationObjects()
	startedAt := metav1.NewTime(now.Add(-2 * time.Minute))
	pod.Status.ContainerStatuses[0].State.Running.StartedAt = startedAt
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}

	return instance, pod, secret
}

func metricTestClient(objects ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(objects...).
		Build()
}

func getMetricInstance(
	t *testing.T,
	k8s client.Client,
	instance *valkeyv1alpha1.ValkeyInstance,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()
	observed := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(t.Context(), client.ObjectKeyFromObject(instance), observed); err != nil {
		t.Fatalf("прочитать ValkeyInstance: %v", err)
	}

	return observed
}

func successfulMetricsSession() MetricsSession {
	return &fakeMetricsSession{metrics: metricValues("run-1")}
}

func metricValues(runID string) operatorvalkey.Metrics {
	return operatorvalkey.Metrics{
		Role: "primary", RunID: runID, UsedMemoryBytes: 1024, MaxmemoryBytes: 2048,
		ConnectedClients: 7, OpsPerSec: 8, KeyspaceHits: 9, KeyspaceMisses: 10, EvictedKeys: 11,
	}
}

func metricStatus(collectedAt time.Time, runID string) valkeyv1alpha1.NodeMetricStatus {
	return valkeyv1alpha1.NodeMetricStatus{
		Ordinal: 0, PodUID: "pod-1", ContainerID: "containerd://1", RunID: runID,
		CollectedAt: metav1.NewMicroTime(collectedAt), Role: valkeyv1alpha1.NodeRolePrimary,
		UsedMemoryBytes: 1, MaxmemoryBytes: 2, ConnectedClients: 3, OpsPerSec: 4,
		KeyspaceHits: 5, KeyspaceMisses: 6, EvictedKeys: 7,
	}
}

type fakeMetricsSession struct {
	metrics      operatorvalkey.Metrics
	readError    error
	verification error
	verify       func() error
}

func (s *fakeMetricsSession) ReadMetrics(context.Context, string) (operatorvalkey.Metrics, error) {
	return s.metrics, s.readError
}

func (s *fakeMetricsSession) VerifyRunID(context.Context, string) error {
	if s.verify != nil {
		return s.verify()
	}

	return s.verification
}

func (s *fakeMetricsSession) Close() {}

type secondPodReadMutation struct {
	client.Reader
	reads  int
	mutate func(*corev1.Pod)
}

func (r *secondPodReadMutation) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if err := r.Reader.Get(ctx, key, object, options...); err != nil {
		return err
	}
	pod, ok := object.(*corev1.Pod)
	if !ok {
		return nil
	}
	r.reads++
	if r.reads == 2 {
		r.mutate(pod)
	}

	return nil
}

var _ client.Reader = (*secondPodReadMutation)(nil)
