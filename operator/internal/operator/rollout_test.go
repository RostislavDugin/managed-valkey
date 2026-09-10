package operator

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func Test_RZ09_ReconcileRolloutPreparing_WhenWorkloadStillUsesAppliedSize_KeepsAppliedTemplate(t *testing.T) {
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.VCPU = 4
	instance.Status.AcceptedConfiguration.RAMGB = 8
	instance.Status.Applied = &valkeyv1alpha1.AppliedConfiguration{
		Mode: valkeyv1alpha1.ValkeyModeSingle, VCPU: 2, RAMGB: 4,
	}
	instance.Status.Rollout = &valkeyv1alpha1.RolloutStatus{
		DesiredGeneration: 2, VCPU: 4, RAMGB: 8, Image: "valkey:fixed",
		Stage: valkeyv1alpha1.RolloutStagePreparing,
	}

	workload := rolloutWorkloadInstance(instance)
	if workload.Status.AcceptedConfiguration.VCPU != 2 ||
		workload.Status.AcceptedConfiguration.RAMGB != 4 {
		t.Fatalf("подготовка использует целевой размер: %+v", workload.Status.AcceptedConfiguration)
	}
	oldConfig := configMapData(*workload.Status.AcceptedConfiguration)
	if got := configMapData(
		*instance.Status.AcceptedConfiguration,
	); got[valkeyConfigKey] == oldConfig[valkeyConfigKey] {
		t.Fatal("тестовые размеры не изменили maxmemory")
	}

	instance.Status.Rollout.Stage = valkeyv1alpha1.RolloutStageUpdatingTemplate
	workload = rolloutWorkloadInstance(instance)
	if workload.Status.AcceptedConfiguration.VCPU != 4 ||
		workload.Status.AcceptedConfiguration.RAMGB != 8 {
		t.Fatalf("стадия обновления не использует цель: %+v", workload.Status.AcceptedConfiguration)
	}
}

func Test_RZ03_ReconcileFullStop_WhenClientAccessIsOpen_ScalesDownOnlyAfterAccessCloses(t *testing.T) {
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.RAMGB = 2
	instance.Status.Initialized = true
	instance.Status.Applied = &valkeyv1alpha1.AppliedConfiguration{
		Mode: valkeyv1alpha1.ValkeyModeHA, VCPU: 2, RAMGB: 4,
	}
	instance.Status.Rollout = &valkeyv1alpha1.RolloutStatus{
		DesiredGeneration: 2, VCPU: 2, RAMGB: 2,
		Stage: valkeyv1alpha1.RolloutStageStopping,
	}
	if replicas := rolloutWorkloadReplicas(instance, 3); replicas != 3 {
		t.Fatalf("StatefulSet остановлен до закрытия доступа: %d", replicas)
	}
	instance.Status.Rollout.AccessClosed = true
	if replicas := rolloutWorkloadReplicas(instance, 3); replicas != 0 {
		t.Fatalf("StatefulSet не остановлен после закрытия доступа: %d", replicas)
	}
	instance.Status.Rollout.Stage = valkeyv1alpha1.RolloutStageStarting
	if replicas := rolloutWorkloadReplicas(instance, 3); replicas != 3 {
		t.Fatalf("StatefulSet не запущен на целевом шаблоне: %d", replicas)
	}
}

func Test_RZ03_ReconcileFullStop_WhenFailureFailoverExists_ClearsFailoverBeforeScalingDown(t *testing.T) {
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.RAMGB = 2
	instance.Status.Applied = &valkeyv1alpha1.AppliedConfiguration{
		Mode: valkeyv1alpha1.ValkeyModeHA, VCPU: 2, RAMGB: 4,
	}
	instance.Status.Rollout = &valkeyv1alpha1.RolloutStatus{
		DesiredGeneration: 2, VCPU: 2, RAMGB: 2,
		Stage: valkeyv1alpha1.RolloutStageStopping,
	}
	instance.Status.Failover = &valkeyv1alpha1.FailoverStatus{
		Reason: valkeyv1alpha1.FailoverReasonFailure,
		Stage:  valkeyv1alpha1.FailoverStageChoosing,
	}
	instance.Status.Conditions = append(instance.Status.Conditions, metav1.Condition{
		Type: conditionTypeFailover, Status: metav1.ConditionFalse, Reason: "CandidateNotReady",
	})
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s}

	result, err := reconciler.reconcileFailover(context.Background(), instance)
	if err != nil || result.IsZero() {
		t.Fatalf("продолжить полный останов вместо failover: result=%+v error=%v", result, err)
	}
	if instance.Status.Failover != nil || instance.Status.Phase != valkeyv1alpha1.InstancePhaseUpdating ||
		instance.Status.Reason != "ROLLOUT_stopping" ||
		findCondition(instance.Status.Conditions, conditionTypeFailover) != nil {
		t.Fatalf("аварийный failover сохранился поверх полного останова: %+v", instance.Status)
	}
}

func Test_RZ05_ReconcileFullStopPreparing_WhenPrimaryFails_DoesNotSuppressFailover(t *testing.T) {
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.RAMGB = 2
	instance.Status.Initialized = true
	instance.Status.Applied = &valkeyv1alpha1.AppliedConfiguration{
		Mode: valkeyv1alpha1.ValkeyModeHA, VCPU: 2, RAMGB: 4,
	}
	instance.Status.Rollout = &valkeyv1alpha1.RolloutStatus{
		DesiredGeneration: 2, VCPU: 2, RAMGB: 2,
		Stage: valkeyv1alpha1.RolloutStagePreparing,
	}
	primary := failoverNode(0, valkeyv1alpha1.NodeRolePrimary, "history-a", 100, nil)
	primary.Termination = &valkeyv1alpha1.ProcessTermination{Evidence: "container_status"}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{primary}
	setPrimaryIdentity(&instance.Status, primary)
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()

	result, err := (&ValkeyInstanceReconciler{Client: k8s}).reconcileFailover(context.Background(), instance)
	if err != nil || result.IsZero() || instance.Status.Failover == nil {
		t.Fatalf("аварийный failover не начался до полного останова: result=%+v status=%+v error=%v",
			result, instance.Status, err)
	}
}

func Test_RZ05PW10_ReconcileFullStopRecovery_WhenStageIsStartingOrVerifying_FailsOverToSynchronizedReplica(
	t *testing.T,
) {
	for _, stage := range []valkeyv1alpha1.RolloutStage{
		valkeyv1alpha1.RolloutStageStarting,
		valkeyv1alpha1.RolloutStageVerifying,
	} {
		t.Run(string(stage), func(t *testing.T) {
			assertFullStopFailoverCandidate(t, stage)
		})
	}
}

func assertFullStopFailoverCandidate(t *testing.T, stage valkeyv1alpha1.RolloutStage) {
	t.Helper()
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.RAMGB = 2
	instance.Status.AcceptedConfiguration.PasswordVersion = 2
	instance.Status.Applied = &valkeyv1alpha1.AppliedConfiguration{
		Mode: valkeyv1alpha1.ValkeyModeHA, VCPU: 2, RAMGB: 4,
	}
	instance.Status.AppliedPasswordVersion = 1
	instance.Status.Initialized = true
	instance.Status.Rollout = &valkeyv1alpha1.RolloutStatus{
		DesiredGeneration: 2, VCPU: 2, RAMGB: 2,
		Stage: stage,
	}
	instance.Status.CredentialRotation = &valkeyv1alpha1.CredentialRotationStatus{
		TargetVersion: 2, PreviousVersion: 1,
		Stage: valkeyv1alpha1.CredentialRotationStageUpdatingReplicas,
	}
	syncedAt := metav1.Now()
	primary := failoverNode(0, valkeyv1alpha1.NodeRolePrimary, "history-a", 100, nil)
	primary.Termination = &valkeyv1alpha1.ProcessTermination{Evidence: "container_status"}
	replica := failoverNode(1, valkeyv1alpha1.NodeRoleReplica, "history-a", 100, &syncedAt)
	replica.AppPasswordVersion = 2
	empty := failoverNode(2, valkeyv1alpha1.NodeRoleReplica, "", emptyReplicaInitialOffset, nil)
	empty.AppEnabled = false
	empty.AppPasswordVersion = 2
	empty.Replication.LinkUp = false
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{primary, replica, empty}
	setPrimaryIdentity(&instance.Status, primary)
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s}

	for step := range 3 {
		result, err := reconciler.reconcileFailover(ctx, instance)
		if err != nil || result.IsZero() {
			t.Fatalf("шаг %d аварийного переключения: result=%+v status=%+v error=%v",
				step, result, instance.Status, err)
		}
	}
	if instance.Status.Failover == nil || instance.Status.Failover.Candidate == nil ||
		instance.Status.Failover.Candidate.RunID != replica.RunID ||
		instance.Status.Failover.Candidate.RunID == empty.RunID || instance.Status.Reason != "FAILOVER" {
		t.Fatalf("вместо синхронизированной реплики выбран пустой процесс: %+v", instance.Status.Failover)
	}
	if instance.Status.Rollout == nil || instance.Status.Rollout.Stage != stage ||
		instance.Status.CredentialRotation == nil ||
		instance.Status.CredentialRotation.Stage != valkeyv1alpha1.CredentialRotationStageUpdatingReplicas {
		t.Fatalf("failover потерял совмещённые операции: %+v", instance.Status)
	}
}

func Test_RZ02_ReconcileHAGrowth_WhenResourcesIncrease_UsesRollingTemplate(t *testing.T) {
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.RAMGB = 8
	instance.Status.Applied = &valkeyv1alpha1.AppliedConfiguration{
		Mode: valkeyv1alpha1.ValkeyModeHA, VCPU: 2, RAMGB: 4,
	}
	instance.Status.Rollout = &valkeyv1alpha1.RolloutStatus{
		DesiredGeneration: 2, VCPU: 2, RAMGB: 8,
		Stage: valkeyv1alpha1.RolloutStageUpdatingTemplate,
	}
	if rolloutRequiresFullStop(instance) {
		t.Fatal("рост RAM HA ошибочно требует полной остановки")
	}
	if replicas := rolloutWorkloadReplicas(instance, 3); replicas != 3 {
		t.Fatalf("rolling resize уменьшил состав до %d", replicas)
	}
	instance.Status.Rollout.RAMGB = 2
	if !rolloutRequiresFullStop(instance) {
		t.Fatal("уменьшение RAM HA не требует полной остановки")
	}
}

func Test_RZ05_ReconcileReplicaAdmission_WhenRolloutIsActive_DoesNotBlockTargetReplica(t *testing.T) {
	ctx := context.Background()
	instance, pod, node, _ := processObservationObjects()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.VCPU = 2
	instance.Status.AcceptedConfiguration.RAMGB = 2
	instance.Status.Rollout = &valkeyv1alpha1.RolloutStatus{
		DesiredGeneration: 2,
		VCPU:              2,
		RAMGB:             2,
		Image:             "valkey:fixed",
		Stage:             valkeyv1alpha1.RolloutStageReplacingReplicas,
	}
	pod.Spec.Volumes = []corev1.Volume{{
		Name: "config",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: rolloutConfigMapName(instance)},
		}},
	}}
	pod.Spec.Containers = []corev1.Container{{
		Name:  "valkey",
		Image: instance.Status.Rollout.Image,
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		}},
	}}
	replica := testObservedNode(pod)
	replica.Role = valkeyv1alpha1.NodeRoleReplica
	replica.AppPasswordVersion = 1
	replica.Replication = &valkeyv1alpha1.ReplicationStatus{
		LinkUp: true,
		SyncedAt: func() *metav1.Time {
			now := metav1.Now()
			return &now
		}(),
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{replica}
	primaryOrdinal := int32(1)
	instance.Status.PrimaryOrdinal = &primaryOrdinal

	k8s := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(instance, pod, node).Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s}
	result, err := reconciler.reconcileRollingReplacement(
		ctx,
		instance,
		valkeyv1alpha1.RolloutStageSwitchingPrimary,
	)
	if err != nil || !result.IsZero() || instance.Status.Rollout.Process != nil {
		t.Fatalf("rollout не передал управление допуску целевой реплики: result=%+v rollout=%+v error=%v",
			result, instance.Status.Rollout, err)
	}
}

func Test_RZ11_ReconcileRolloutDeletion_WhenStatusChangesConcurrently_UsesFreshInstance(t *testing.T) {
	current := completeAcceptedInstance()
	current.Finalizers = []string{instanceFinalizer}
	now := metav1.Now()
	current.DeletionTimestamp = &now
	stale := current.DeepCopy()
	stale.DeletionTimestamp = nil

	k8s := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(current).Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s}
	deleting, err := reconciler.rolloutDeletionRequested(context.Background(), stale)
	if err != nil || !deleting {
		t.Fatalf("свежий deletionTimestamp не остановил rollout: deleting=%t error=%v", deleting, err)
	}
}

func Test_RZ07_ReconcileRolloutReplacement_WhenPodReadFails_StopsReplacement(t *testing.T) {
	ctx := context.Background()
	instance, pod, node, _ := processObservationObjects()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.Rollout = &valkeyv1alpha1.RolloutStatus{
		DesiredGeneration: 2,
		VCPU:              2,
		RAMGB:             4,
		Image:             "valkey:fixed",
		Stage:             valkeyv1alpha1.RolloutStageReplacingReplicas,
	}
	process := testObservedNode(pod)
	process.Role = valkeyv1alpha1.NodeRoleReplica
	process.AppPasswordVersion = 1
	process.Replication = &valkeyv1alpha1.ReplicationStatus{
		LinkUp:   true,
		SyncedAt: &metav1.Time{Time: metav1.Now().Time},
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{process}
	primaryOrdinal := int32(1)
	instance.Status.PrimaryOrdinal = &primaryOrdinal

	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, node).
		Build()
	readErr := errors.New("временная ошибка API")
	reconciler := &ValkeyInstanceReconciler{
		Client:    k8s,
		APIReader: &podReadErrorReader{Reader: k8s, err: readErr},
	}
	result, err := reconciler.reconcileRollingReplacement(
		ctx,
		instance,
		valkeyv1alpha1.RolloutStageSwitchingPrimary,
	)
	if !errors.Is(err, readErr) || !result.IsZero() || instance.Status.Rollout.Process != nil {
		t.Fatalf("ошибка чтения Pod не остановила замену: result=%+v rollout=%+v error=%v",
			result, instance.Status.Rollout, err)
	}
}

type podReadErrorReader struct {
	client.Reader
	err error
}

func (r *podReadErrorReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if _, ok := object.(*corev1.Pod); ok {
		return r.err
	}
	return r.Reader.Get(ctx, key, object, options...)
}
