package operator

import (
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func Test_ReconcileProcess_WhenFirstObserved_SavesFinalizerBeforeProcessIdentity(t *testing.T) {
	ctx := context.Background()
	instance, pod, node, secret := processObservationObjects()
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, node, secret).
		Build()
	inspections := 0
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		Scheme: NewScheme(),
		InspectProcess: func(
			ctx context.Context,
			_ string,
			_ string,
			_ string,
			_ bool,
		) (operatorvalkey.ProcessState, error) {
			inspections++
			observedPod := &corev1.Pod{}
			if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), observedPod); err != nil {
				t.Fatalf("прочитать Pod во время наблюдения: %v", err)
			}
			if !slices.Contains(observedPod.Finalizers, processFinalizer) {
				t.Fatal("клиент Valkey вызван до сохранения finalizer Pod")
			}

			syncedAt := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
			return operatorvalkey.ProcessState{
				Role: "primary", RunID: "run-1", AppPasswordHashes: []string{strings.Repeat("ab", 32)},
				Replication: &operatorvalkey.ReplicationState{
					ReplicationID: "replication-1", Offset: 42, SyncedAt: &syncedAt,
				},
			}, nil
		},
	}

	result, err := reconciler.reconcileProcess(ctx, instance)
	if err != nil {
		t.Fatalf("первый reconcile процесса: %v", err)
	}
	if result.IsZero() || inspections != 0 {
		t.Fatalf("первый reconcile не ограничился finalizer: result=%+v inspections=%d", result, inspections)
	}

	observedPod := &corev1.Pod{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), observedPod); err != nil {
		t.Fatalf("прочитать Pod после первого reconcile: %v", err)
	}
	if !slices.Contains(observedPod.Finalizers, processFinalizer) {
		t.Fatal("finalizer процесса не сохранён")
	}

	observedInstance := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observedInstance); err != nil {
		t.Fatalf("прочитать ValkeyInstance: %v", err)
	}
	result, err = reconciler.reconcileProcess(ctx, observedInstance)
	if err != nil {
		t.Fatalf("второй reconcile процесса: %v", err)
	}
	if inspections != 1 {
		t.Fatalf("идентичность не была сохранена: result=%+v inspections=%d", result, inspections)
	}

	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observedInstance); err != nil {
		t.Fatalf("прочитать сохранённую идентичность: %v", err)
	}
	if len(observedInstance.Status.Nodes) != 1 {
		t.Fatalf("сохранено идентичностей: %d", len(observedInstance.Status.Nodes))
	}
	nodeStatus := observedInstance.Status.Nodes[0]
	if nodeStatus.PodUID != string(pod.UID) ||
		nodeStatus.ContainerID != pod.Status.ContainerStatuses[0].ContainerID ||
		nodeStatus.RunID != "run-1" ||
		nodeStatus.NodeName != node.Name ||
		nodeStatus.NodeUID != string(node.UID) ||
		nodeStatus.Role != valkeyv1alpha1.NodeRolePrimary ||
		!nodeStatus.Readiness || nodeStatus.Replication == nil ||
		nodeStatus.Replication.ReplicationID != "replication-1" ||
		nodeStatus.Replication.Offset != 42 || nodeStatus.Replication.SyncedAt == nil {
		t.Fatalf("сохранена неверная идентичность: %+v", nodeStatus)
	}
}

func Test_SameProcess_WhenAnyIdentityFieldChanges_ReturnsFalse(t *testing.T) {
	base := valkeyv1alpha1.NodeStatus{
		Ordinal: 0, PodUID: "pod-1", ContainerID: "containerd://1", RunID: "run-1",
		NodeName: "worker-1", NodeUID: "node-1",
	}

	tests := map[string]func(*valkeyv1alpha1.NodeStatus){
		"при изменении ordinal считает процесс заменённым":       func(status *valkeyv1alpha1.NodeStatus) { status.Ordinal = 1 },
		"при изменении UID Pod считает процесс заменённым":       func(status *valkeyv1alpha1.NodeStatus) { status.PodUID = "pod-2" },
		"при изменении ID контейнера считает процесс заменённым": func(status *valkeyv1alpha1.NodeStatus) { status.ContainerID = "containerd://2" },
		"при изменении run ID считает процесс заменённым":        func(status *valkeyv1alpha1.NodeStatus) { status.RunID = "run-2" },
		"при изменении имени ноды считает процесс заменённым":    func(status *valkeyv1alpha1.NodeStatus) { status.NodeName = "worker-2" },
		"при изменении UID ноды считает процесс заменённым":      func(status *valkeyv1alpha1.NodeStatus) { status.NodeUID = "node-2" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			observed := base
			mutate(&observed)
			if sameProcess(base, observed) {
				t.Fatalf("смена %s не обнаружена", name)
			}
		})
	}
	if !sameProcess(base, base) {
		t.Fatal("неизменный процесс распознан как замена")
	}
}

func Test_ProcessNeedsControl_AfterManagerStart_ReturnsTrueOnlyForUncontrolledProcess(t *testing.T) {
	startedAt := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	before := metav1.NewTime(startedAt.Add(-time.Second))
	after := metav1.NewTime(startedAt.Add(time.Second))
	process := valkeyv1alpha1.NodeStatus{Replication: &valkeyv1alpha1.ReplicationStatus{ObservedAt: before}}
	if !processNeedsControl(process, true, startedAt) {
		t.Fatal("первое наблюдение после запуска manager не приняло управление")
	}
	process.Replication.ObservedAt = after
	if processNeedsControl(process, true, startedAt) {
		t.Fatal("повторное наблюдение снова закрыло административные соединения")
	}
	if !processNeedsControl(valkeyv1alpha1.NodeStatus{}, false, startedAt) {
		t.Fatal("новый процесс не потребовал приёма управления")
	}
}

func Test_CT06_ReconcileProcess_WhenIdentityChanges_PreservesUnconfirmedPreviousIncarnations(t *testing.T) {
	process := func(id string) valkeyv1alpha1.NodeStatus {
		return valkeyv1alpha1.NodeStatus{
			Ordinal:     0,
			PodUID:      "pod-1",
			ContainerID: "containerd://" + id,
			RunID:       "run-" + id,
			NodeName:    "worker-1",
			NodeUID:     "node-1",
		}
	}
	status := valkeyv1alpha1.ValkeyInstanceStatus{Nodes: []valkeyv1alpha1.NodeStatus{process("a")}}

	recordCurrentProcess(&status, process("b"), nil)
	proofB := &valkeyv1alpha1.ProcessTermination{
		Reason: "Completed", Evidence: "container_status", FinishedAt: metav1.Now(),
	}
	recordCurrentProcess(&status, process("c"), proofB)

	if len(status.Nodes) != 1 || status.Nodes[0].RunID != "run-c" {
		t.Fatalf("текущий ordinal не указывает на C: %+v", status.Nodes)
	}
	if len(status.PreviousProcesses) != 2 || status.PreviousProcesses[0].RunID != "run-a" ||
		status.PreviousProcesses[0].Termination != nil ||
		status.PreviousProcesses[1].RunID != "run-b" ||
		status.PreviousProcesses[1].Termination == nil {
		t.Fatalf("история A/B повреждена: %+v", status.PreviousProcesses)
	}
	if !hasUnterminatedPreviousProcess(status.PreviousProcesses) {
		t.Fatal("доказательство остановки B ошибочно закрыло обязательство A")
	}
}

func Test_CT07_ReconcileProcess_WhenRunIDChanges_PreservesReplicationHistory(t *testing.T) {
	now := metav1.Now()
	old := valkeyv1alpha1.NodeStatus{
		Ordinal: 0, PodUID: "pod-1", ContainerID: "containerd://1", RunID: "run-a",
		NodeName: "worker-1", NodeUID: "node-1",
		Replication: &valkeyv1alpha1.ReplicationStatus{
			ReplicationID: "history-a", Offset: 75, ObservedAt: now,
		},
	}
	current := *old.DeepCopy()
	current.RunID = "run-b"
	current.Replication = &valkeyv1alpha1.ReplicationStatus{
		ReplicationID: "history-b", Offset: 1, ObservedAt: now,
	}
	status := valkeyv1alpha1.ValkeyInstanceStatus{Nodes: []valkeyv1alpha1.NodeStatus{old}}

	recordCurrentProcess(&status, current, nil)

	if len(status.Nodes) != 1 || status.Nodes[0].RunID != "run-b" ||
		status.Nodes[0].Replication == nil || status.Nodes[0].Replication.ReplicationID != "history-b" {
		t.Fatalf("новая история репликации не сохранена: %+v", status.Nodes)
	}
	if len(status.PreviousProcesses) != 1 || status.PreviousProcesses[0].RunID != "run-a" ||
		status.PreviousProcesses[0].Replication == nil ||
		status.PreviousProcesses[0].Replication.ReplicationID != "history-a" ||
		status.PreviousProcesses[0].Termination != nil {
		t.Fatalf("история прежнего run ID потеряна: %+v", status.PreviousProcesses)
	}
}

func Test_ReconcileProcessHistory_AfterConfirmedProcessObligationsClose_RemovesPreviousProcess(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	stale := valkeyv1alpha1.NodeStatus{
		Ordinal: 0, PodUID: "pod-stale", ContainerID: "containerd://stale", RunID: "run-stale",
		NodeName: "worker-1", NodeUID: "node-1",
		Termination: &valkeyv1alpha1.ProcessTermination{
			Reason: "Completed", Evidence: "container_status", FinishedAt: metav1.Now(),
		},
	}
	primary := *stale.DeepCopy()
	primary.PodUID = "pod-primary"
	primary.ContainerID = "containerd://primary"
	primary.RunID = "run-primary"
	unknown := *stale.DeepCopy()
	unknown.PodUID = "pod-unknown"
	unknown.ContainerID = "containerd://unknown"
	unknown.RunID = "run-unknown"
	unknown.Termination = nil
	instance.Status.PreviousProcesses = []valkeyv1alpha1.NodeStatus{stale, primary, unknown}
	setPrimaryIdentity(&instance.Status, primary)
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s}

	result, err := reconciler.reconcileProcessHistory(ctx, instance)
	if err != nil || result.IsZero() || len(instance.Status.PreviousProcesses) != 2 {
		t.Fatalf("закрытая история не очищена: result=%+v history=%+v error=%v",
			result, instance.Status.PreviousProcesses, err)
	}
	if instance.Status.PreviousProcesses[0].RunID != primary.RunID ||
		instance.Status.PreviousProcesses[1].RunID != unknown.RunID {
		t.Fatalf("нужная история удалена: %+v", instance.Status.PreviousProcesses)
	}
}

func Test_ReplicaSynchronization_WhenReplicationHistoryChanges_BelongsOnlyToCurrentHistory(t *testing.T) {
	firstTime := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	first := mergeObservedReplication(nil, &valkeyv1alpha1.ReplicationStatus{
		ReplicationID: "history-a", UpstreamHost: "10.42.0.1", UpstreamPort: 6379, LinkUp: true,
	}, valkeyv1alpha1.NodeRoleReplica, firstTime)
	if first.SyncedAt == nil || !first.SyncedAt.Time.Equal(firstTime) {
		t.Fatalf("первая синхронизация не подтверждена: %+v", first)
	}

	disconnected := mergeObservedReplication(first, &valkeyv1alpha1.ReplicationStatus{
		ReplicationID: "history-a", UpstreamHost: "10.42.0.1", UpstreamPort: 6379,
	}, valkeyv1alpha1.NodeRoleReplica, firstTime.Add(time.Minute))
	if disconnected.SyncedAt == nil || !disconnected.SyncedAt.Equal(first.SyncedAt) {
		t.Fatalf("подтверждение текущей истории потеряно после разрыва: %+v", disconnected)
	}

	foreign := mergeObservedReplication(disconnected, &valkeyv1alpha1.ReplicationStatus{
		ReplicationID: "history-b", UpstreamHost: "10.42.0.2", UpstreamPort: 6379,
	}, valkeyv1alpha1.NodeRoleReplica, firstTime.Add(2*time.Minute))
	if foreign.SyncedAt != nil {
		t.Fatalf("подтверждение перенесено на чужую историю: %+v", foreign)
	}
	foreign.LinkUp = true
	resynced := mergeObservedReplication(
		foreign,
		foreign.DeepCopy(),
		valkeyv1alpha1.NodeRoleReplica,
		firstTime.Add(3*time.Minute),
	)
	if resynced.SyncedAt == nil || !resynced.SyncedAt.Time.Equal(firstTime.Add(3*time.Minute)) {
		t.Fatalf("новая история не получила своё подтверждение: %+v", resynced)
	}
}

func Test_CT10_ReconcileProcesses_WithHAMode_ObservesOtherProcessesWhileOneCallBlocks(t *testing.T) {
	ctx := context.Background()
	instance, basePod, _, secret := processObservationObjects()
	instance.Spec.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.Initialized = true
	primaryOrdinal := int32(0)
	instance.Status.PrimaryOrdinal = &primaryOrdinal
	objects := []client.Object{instance, secret}
	for ordinal := range int32(3) {
		pod := basePod.DeepCopy()
		pod.Name = instance.Name + "-" + string(rune('0'+ordinal))
		pod.UID = types.UID("pod-" + string(rune('0'+ordinal)))
		pod.Spec.NodeName = "worker-" + string(rune('0'+ordinal))
		pod.Status.PodIP = "10.42.0." + string(rune('1'+ordinal))
		pod.Status.ContainerStatuses[0].ContainerID = "containerd://" + string(rune('0'+ordinal))
		pod.Finalizers = []string{processFinalizer}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: pod.Spec.NodeName, UID: types.UID("node-" + string(rune('0'+ordinal))),
		}}
		role := valkeyv1alpha1.NodeRoleReplica
		if ordinal == 0 {
			role = valkeyv1alpha1.NodeRolePrimary
		}
		instance.Status.Nodes = append(instance.Status.Nodes, valkeyv1alpha1.NodeStatus{
			Ordinal: ordinal, PodUID: string(pod.UID),
			ContainerID: pod.Status.ContainerStatuses[0].ContainerID,
			RunID:       "run-" + string(rune('0'+ordinal)),
			NodeName:    node.Name, NodeUID: string(node.UID), Role: role, Readiness: true,
		})
		objects = append(objects, pod, node)
	}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(objects...).
		Build()
	blocked := make(chan struct{})
	observed := make(chan string, 2)
	var calls atomic.Int32
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s, APIReader: k8s,
		InspectProcess: func(_ context.Context, address, _, _ string, _ bool) (operatorvalkey.ProcessState, error) {
			calls.Add(1)
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return operatorvalkey.ProcessState{}, err
			}
			if host == "10.42.0.3" {
				<-blocked
				return operatorvalkey.ProcessState{}, operatorvalkey.ErrTransportFailure
			}
			observed <- host
			ordinal := host[len(host)-1] - '1'
			role := "replica"
			if ordinal == 0 {
				role = "primary"
			}
			return operatorvalkey.ProcessState{
				Role: role, RunID: "run-" + string(rune('0'+ordinal)),
			}, nil
		},
		Clock: clocktesting.NewFakeClock(now),
	}
	done := make(chan error, 1)
	go func() {
		_, err := reconciler.reconcileProcess(ctx, instance)
		done <- err
	}()

	seen := map[string]bool{}
	for range 2 {
		select {
		case host := <-observed:
			seen[host] = true
		case <-time.After(time.Second):
			t.Fatal("исправный процесс не наблюдался из-за зависшей реплики")
		}
	}
	if !seen["10.42.0.1"] || !seen["10.42.0.2"] {
		t.Fatalf("наблюдались не все исправные процессы: %v", seen)
	}
	deadline := time.Now().Add(time.Second)
	heartbeatSaved := false
	for time.Now().Before(deadline) {
		current := &valkeyv1alpha1.ValkeyInstance{}
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), current); err != nil {
			t.Fatalf("прочитать heartbeat HA: %v", err)
		}
		if current.Status.ObservedAt != nil && current.Status.ObservedAt.Time.Equal(now) {
			heartbeatSaved = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !heartbeatSaved {
		t.Fatal("исправные процессы не сохранили heartbeat до ответа зависшей реплики")
	}
	close(blocked)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("завершить наблюдение HA: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("наблюдение HA не завершилось после снятия блокировки")
	}
	if calls.Load() != 3 {
		t.Fatalf("выполнено %d наблюдений вместо 3", calls.Load())
	}
}

func Test_PodInstanceRequests_WithAndWithoutInstanceLabel_ReturnsRequestOnlyForLabeledPod(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "cache-a1b2c3-0", Namespace: "valkey-cache-a1b2c3",
		Labels: map[string]string{instanceLabelKey: "cache-a1b2c3"},
	}}
	requests := podInstanceRequests(context.Background(), pod)
	if len(requests) != 1 || requests[0].Name != "cache-a1b2c3" ||
		requests[0].Namespace != "valkey-cache-a1b2c3" {
		t.Fatalf("неверный запрос reconcile для Pod: %+v", requests)
	}

	pod.Labels = nil
	if requests := podInstanceRequests(context.Background(), pod); len(requests) != 0 {
		t.Fatalf("Pod без метки поставлен в очередь: %+v", requests)
	}
}

func processObservationObjects() (
	*valkeyv1alpha1.ValkeyInstance,
	*corev1.Pod,
	*corev1.Node,
	*corev1.Secret,
) {
	instance := completeAcceptedInstance()
	instance.Spec.Slug = instance.Name
	instance.Status.AcceptedConfiguration.PasswordVersion = 1
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: instance.Name + "-0", Namespace: instance.Namespace, UID: types.UID("pod-1"),
		},
		Spec: corev1.PodSpec{NodeName: "worker-1"},
		Status: corev1.PodStatus{
			PodIP: "10.42.0.10",
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "valkey", ContainerID: "containerd://1",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", UID: types.UID("node-1")}}
	password := []byte(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 24)))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: valkeyv1alpha1.AuthSecretName(instance.Name), Namespace: instance.Namespace,
		},
		Data: map[string][]byte{
			valkeyv1alpha1.OperatorPasswordKey:            password,
			valkeyv1alpha1.ReplicaPasswordKey:             password,
			valkeyv1alpha1.HealthPasswordKey:              password,
			valkeyv1alpha1.UsersACLKey:                    []byte("acl"),
			valkeyv1alpha1.AppPasswordHashKeyPrefix + "1": []byte(strings.Repeat("ab", 32)),
		},
	}

	return instance, pod, node, secret
}
