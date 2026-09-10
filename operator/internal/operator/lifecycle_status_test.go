package operator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func Test_ReconcileProvisioningDeadline_AfterTimeoutAcrossRestart_UsesCreationTimestampAndReportsError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	instance := completeAcceptedInstance()
	instance.CreationTimestamp = metav1.NewTime(now.Add(-config.ProvisionTimeout))
	instance.Status.CredentialsInitialized = true
	instance.Status.Phase = valkeyv1alpha1.InstancePhaseProvisioning
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, Clock: clocktesting.NewFakeClock(now)}

	changed, err := reconciler.reconcileProvisioningDeadline(ctx, instance)
	if err != nil || !changed {
		t.Fatalf("таймаут создания: changed=%t error=%v", changed, err)
	}
	if instance.Status.Phase != valkeyv1alpha1.InstancePhaseError ||
		instance.Status.Reason != "PROVISIONING_TIMEOUT" || instance.Status.Initialized {
		t.Fatalf("неверный status таймаута: %+v", instance.Status)
	}

	restarted := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), restarted); err != nil {
		t.Fatalf("прочитать CR после рестарта: %v", err)
	}
	reconciler = &ValkeyInstanceReconciler{Client: k8s, Clock: clocktesting.NewFakeClock(now.Add(time.Minute))}
	changed, err = reconciler.reconcileProvisioningDeadline(ctx, restarted)
	if err != nil || changed || restarted.Status.Phase != valkeyv1alpha1.InstancePhaseError {
		t.Fatalf("рестарт изменил терминальный таймаут: changed=%t status=%+v error=%v", changed, restarted.Status, err)
	}
}

func Test_UpdateLifecycleStatus_WhenEnvoyVerificationIsUnknown_AdvancesHeartbeatWithoutChangingOperationalState(
	t *testing.T,
) {
	ctx := context.Background()
	instance, pod, node, secret := processObservationObjects()
	pod.Finalizers = []string{processFinalizer}
	observedAt := metav1.NewTime(time.Date(2026, 9, 8, 11, 59, 0, 0, time.UTC))
	verifiedAt := metav1.NewTime(observedAt.Add(-time.Minute))
	instance.Status.Initialized = true
	instance.Status.Phase = valkeyv1alpha1.InstancePhaseRunning
	instance.Status.ObservedAt = &observedAt
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}
	instance.Status.Network = &valkeyv1alpha1.NetworkStatus{
		VerificationStatus:  valkeyv1alpha1.NetworkVerificationUnknown,
		VerifiedAt:          &verifiedAt,
		VerifiedFingerprint: "network",
	}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, node, secret).
		Build()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		Clock:  clocktesting.NewFakeClock(now),
		InspectProcess: func(context.Context, string, string, string, bool) (operatorvalkey.ProcessState, error) {
			return operatorvalkey.ProcessState{
				Role: "primary", RunID: "run-1", AppEnabled: true,
				AppPasswordHashes: []string{strings.Repeat("ab", 32)},
			}, nil
		},
	}

	result, err := reconciler.reconcileProcess(ctx, instance)
	if err != nil {
		t.Fatalf("обновить heartbeat: result=%+v error=%v", result, err)
	}
	if instance.Status.ObservedAt == nil || !instance.Status.ObservedAt.Time.Equal(now) {
		t.Fatalf("heartbeat не обновлён: %v", instance.Status.ObservedAt)
	}
	if instance.Status.Network.VerifiedAt == nil ||
		!instance.Status.Network.VerifiedAt.Equal(&verifiedAt) ||
		instance.Status.Phase != valkeyv1alpha1.InstancePhaseRunning {
		t.Fatalf("unknown Envoy повредил рабочий status: %+v", instance.Status)
	}
}

func Test_CT01_ReconcileSchedule_AfterFirstObservation_UsesSecondObservationInterval(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	clock := clocktesting.NewFakeClock(now)
	observedAt := metav1.NewTime(clock.Now())
	if heartbeatDue(&observedAt, clock.Now()) {
		t.Fatal("heartbeat повторён без нового секундного наблюдения")
	}

	clock.Step(config.HealthCheckInterval - time.Millisecond)
	if heartbeatDue(&observedAt, clock.Now()) {
		t.Fatal("heartbeat обновлён раньше секунды")
	}
	clock.Step(time.Millisecond)
	if !heartbeatDue(&observedAt, clock.Now()) {
		t.Fatal("heartbeat не готов через секунду")
	}

	if result := requeueForObservation(ctrl.Result{}); result.RequeueAfter != config.HealthCheckInterval {
		t.Fatalf("пустое ожидание назначено через %s", result.RequeueAfter)
	}
	if result := requeueForObservation(ctrl.Result{
		RequeueAfter: config.NetworkVerifyInterval,
	}); result.RequeueAfter != config.HealthCheckInterval {
		t.Fatalf("Envoy задержал наблюдение до %s", result.RequeueAfter)
	}
	if result := requeueForObservation(ctrl.Result{
		RequeueAfter: time.Nanosecond,
	}); result.RequeueAfter != time.Nanosecond {
		t.Fatalf("немедленный переход задержан до %s", result.RequeueAfter)
	}

	clock.Step(10 * time.Second)
	if result := requeueForObservation(ctrl.Result{}); result.RequeueAfter != config.HealthCheckInterval {
		t.Fatalf("пропущенные тики накопились: %+v", result)
	}
}

func Test_CT10_Manager_WithIndependentInstances_UsesBoundedConcurrentReconciliation(t *testing.T) {
	options := valkeyInstanceControllerOptions()
	if options.MaxConcurrentReconciles != config.MaxConcurrentReconciles ||
		options.MaxConcurrentReconciles <= 1 {
		t.Fatalf("неверный параллелизм контроллера: %d", options.MaxConcurrentReconciles)
	}
}

func Test_UpdateStatus_WithUnchangedOrConflictingState_SkipsWriteAndPreservesConcurrentFields(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Status.Phase = valkeyv1alpha1.InstancePhaseProvisioning
	base := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	if err := base.Get(ctx, client.ObjectKeyFromObject(instance), instance); err != nil {
		t.Fatalf("прочитать исходный CR: %v", err)
	}
	reconciler := &ValkeyInstanceReconciler{Client: base}
	beforeVersion := instance.ResourceVersion
	changed, err := reconciler.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		status.Phase = valkeyv1alpha1.InstancePhaseProvisioning
	})
	if err != nil || changed {
		t.Fatalf("неизменный status записан: changed=%t error=%v", changed, err)
	}
	if instance.ResourceVersion != beforeVersion {
		t.Fatalf("resourceVersion изменился без изменения status: %s -> %s", beforeVersion, instance.ResourceVersion)
	}

	conflicting := &conflictingStatusClient{Client: base}
	reconciler.Client = conflicting
	changed, err = reconciler.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		status.Phase = valkeyv1alpha1.InstancePhaseRunning
	})
	if err != nil || !changed {
		t.Fatalf("повтор status после конфликта: changed=%t error=%v", changed, err)
	}
	observed := &valkeyv1alpha1.ValkeyInstance{}
	if err := base.Get(ctx, client.ObjectKeyFromObject(instance), observed); err != nil {
		t.Fatalf("прочитать status после конфликта: %v", err)
	}
	if observed.Status.Phase != valkeyv1alpha1.InstancePhaseRunning ||
		observed.Status.Reason != "CONCURRENT_OBSERVATION" {
		t.Fatalf("конкурентное поле потеряно: %+v", observed.Status)
	}
}

func Test_UpdateRecoveryCondition_WhenConditionIsUnchanged_DoesNotDuplicateEvent(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	recorder := events.NewFakeRecorder(2)
	reconciler := &ValkeyInstanceReconciler{Client: k8s, Recorder: recorder}
	for range 2 {
		if _, err := reconciler.credentialsFailure(ctx, instance, "SecretNotFound", true); err != nil {
			t.Fatalf("сохранить состояние восстановления: %v", err)
		}
	}
	if count := len(recorder.Events); count != 1 {
		t.Fatalf("одно состояние создало %d Events", count)
	}
}

type conflictingStatusClient struct {
	client.Client
	once sync.Once
}

func (c *conflictingStatusClient) Status() client.SubResourceWriter {
	return &conflictingStatusWriter{SubResourceWriter: c.Client.Status(), owner: c}
}

type conflictingStatusWriter struct {
	client.SubResourceWriter
	owner *conflictingStatusClient
}

func (w *conflictingStatusWriter) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.SubResourcePatchOption,
) error {
	conflict := false
	w.owner.once.Do(func() {
		conflict = true
	})
	if !conflict {
		return w.SubResourceWriter.Patch(ctx, object, patch, options...)
	}

	current := &valkeyv1alpha1.ValkeyInstance{}
	if err := w.owner.Client.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
		return err
	}
	current.Status.Reason = "CONCURRENT_OBSERVATION"
	if err := w.owner.Client.Status().Update(ctx, current); err != nil {
		return err
	}

	return apierrors.NewConflict(
		schema.GroupResource{Group: valkeyv1alpha1.GroupVersion.Group, Resource: "valkeyinstances"},
		object.GetName(),
		errors.New("тестовый конфликт status"),
	)
}

var (
	_ client.Client            = (*conflictingStatusClient)(nil)
	_ client.SubResourceWriter = (*conflictingStatusWriter)(nil)
)

func Test_UpdateLifecycleStatus_WhenInitializedProcessIsMissing_MovesInstanceToUnavailable(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Status.Initialized = true
	instance.Status.Phase = valkeyv1alpha1.InstancePhaseRunning
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s}

	result, err := reconciler.reconcileProcess(ctx, instance)
	if err != nil || result.RequeueAfter != config.HealthCheckInterval {
		t.Fatalf("потеря primary: result=%+v error=%v", result, err)
	}
	if instance.Status.Phase != valkeyv1alpha1.InstancePhaseUnavailable ||
		instance.Status.Reason != "PRIMARY_NOT_READY" {
		t.Fatalf("потеря primary не отражена: %+v", instance.Status)
	}
}

func Test_ValkeyInstancePredicate_WhenOnlyStatusChanges_IgnoresUpdate(t *testing.T) {
	filter := valkeyInstancePredicate()
	before := completeAcceptedInstance()
	after := before.DeepCopy()
	after.Status.Reason = "OBSERVED"
	if filter.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("собственная запись status запустила второй reconcile")
	}
	after.Generation++
	if !filter.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("новое поколение spec отфильтровано")
	}
	after = before.DeepCopy()
	after.Annotations = map[string]string{manualFencingAnnotation: "{}"}
	if !filter.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("ручное fencing отфильтровано")
	}
	after = before.DeepCopy()
	deletedAt := metav1.Now()
	after.DeletionTimestamp = &deletedAt
	if !filter.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("начало удаления отфильтровано")
	}
}
