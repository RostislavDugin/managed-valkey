package operator

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func Test_ReconcileAppAdmission_WithoutCurrentPrimaryEndpoint_WaitsThenConfirmsAppliedConfigurationAfterEndpointAppears(
	t *testing.T,
) {
	ctx := context.Background()
	instance, pod, _, secret := processObservationObjects()
	pod.Labels = workloadLabels(instance.Name)
	pod.Labels[applicationRoleLabel] = string(valkeyv1alpha1.NodeRolePrimary)
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, secret).
		Build()
	enableCalls := 0
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		UpdateAppAccess: func(
			_ context.Context,
			_ string,
			_ string,
			hash string,
			enabled bool,
		) (operatorvalkey.ProcessState, error) {
			enableCalls++
			if !enabled {
				t.Fatal("допуск попытался выключить app")
			}
			return operatorvalkey.ProcessState{
				Role: "primary", RunID: "run-1", AppEnabled: true,
				AppPasswordHashes: []string{hash},
			}, nil
		},
	}

	result, err := reconciler.reconcileAppAdmission(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("ожидание EndpointSlice: result=%+v error=%v", result, err)
	}
	if enableCalls != 0 || instance.Status.Initialized {
		t.Fatal("app включён до появления EndpointSlice")
	}

	endpointSlice := testPrimaryEndpointSlice(instance, pod)
	endpointSlice.Endpoints[0].TargetRef.UID = types.UID("old-pod")
	if err := k8s.Create(ctx, endpointSlice); err != nil {
		t.Fatalf("создать EndpointSlice прежнего Pod: %v", err)
	}
	result, err = reconciler.reconcileAppAdmission(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("ожидание актуального EndpointSlice: result=%+v error=%v", result, err)
	}
	if enableCalls != 0 {
		t.Fatal("app включён для EndpointSlice прежнего Pod")
	}

	endpointSlice.Endpoints[0].TargetRef.UID = pod.UID
	if err := k8s.Update(ctx, endpointSlice); err != nil {
		t.Fatalf("обновить EndpointSlice: %v", err)
	}
	result, err = reconciler.reconcileAppAdmission(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("допустить app: result=%+v error=%v", result, err)
	}
	if enableCalls != 1 {
		t.Fatalf("app включён %d раз", enableCalls)
	}
	if !instance.Status.Initialized || instance.Status.Phase != valkeyv1alpha1.InstancePhaseRunning ||
		instance.Status.PrimaryOrdinal == nil || *instance.Status.PrimaryOrdinal != 0 ||
		instance.Status.PrimaryPodUID != string(pod.UID) ||
		instance.Status.PrimaryContainerID != pod.Status.ContainerStatuses[0].ContainerID ||
		instance.Status.ObservedGeneration != instance.Status.AcceptedConfiguration.DesiredGeneration ||
		instance.Status.AppliedPasswordVersion != instance.Status.AcceptedConfiguration.PasswordVersion ||
		instance.Status.Applied == nil || instance.Status.ObservedAt == nil {
		t.Fatalf("неполный status после допуска: %+v", instance.Status)
	}
	result, err = reconciler.reconcileAppAdmission(ctx, instance)
	if err != nil || !result.IsZero() || enableCalls != 1 {
		t.Fatalf("готовый primary повторно принял управление: result=%+v calls=%d error=%v",
			result, enableCalls, err)
	}
}

func Test_ReconcileAppAdmission_WhenEnableResponseIsLost_RechecksACLStateBeforeConfirmingReadiness(t *testing.T) {
	ctx := context.Background()
	instance, pod, _, secret := processObservationObjects()
	pod.Labels = workloadLabels(instance.Name)
	pod.Labels[applicationRoleLabel] = string(valkeyv1alpha1.NodeRolePrimary)
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}
	endpointSlice := testPrimaryEndpointSlice(instance, pod)
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, secret, endpointSlice).
		Build()
	calls := 0
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		UpdateAppAccess: func(
			_ context.Context,
			_ string,
			_ string,
			hash string,
			enabled bool,
		) (operatorvalkey.ProcessState, error) {
			calls++
			if !enabled {
				t.Fatal("повтор допуска выключил app")
			}
			if calls == 1 {
				return operatorvalkey.ProcessState{}, errors.New("ответ ACL SETUSER потерян")
			}
			return operatorvalkey.ProcessState{
				Role: "primary", RunID: "run-1", AppEnabled: true,
				AppPasswordHashes: []string{hash},
			}, nil
		},
	}

	if _, err := reconciler.reconcileAppAdmission(ctx, instance); err == nil {
		t.Fatal("потерянный ответ ACL SETUSER принят как успех")
	}
	if instance.Status.Initialized {
		t.Fatal("готовность записана без подтверждения состояния ACL")
	}
	if _, err := reconciler.reconcileAppAdmission(ctx, instance); err != nil {
		t.Fatalf("повторно проверить состояние ACL: %v", err)
	}
	if calls != 2 || !instance.Status.Initialized {
		t.Fatalf("состояние ACL не подтверждено повторно: calls=%d status=%+v", calls, instance.Status)
	}
}

func Test_CT04_ReconcileAppAdmission_WithPendingMutations_DoesNotConfirmGeneration(t *testing.T) {
	ctx := context.Background()
	instance, pod, _, secret := processObservationObjects()
	pod.Labels = workloadLabels(instance.Name)
	pod.Labels[applicationRoleLabel] = string(valkeyv1alpha1.NodeRolePrimary)
	instance.Status.Initialized = true
	instance.Status.Phase = valkeyv1alpha1.InstancePhaseUpdating
	instance.Status.ObservedGeneration = 1
	instance.Status.AppliedPasswordVersion = 1
	instance.Status.Applied = &valkeyv1alpha1.AppliedConfiguration{
		Mode: valkeyv1alpha1.ValkeyModeSingle, VCPU: 1, RAMGB: 4,
	}
	instance.Status.AcceptedConfiguration.VCPU = 2
	instance.Status.AcceptedConfiguration.RAMGB = 8
	instance.Status.AcceptedConfiguration.PasswordVersion = 2
	instance.Status.AcceptedConfiguration.DesiredGeneration = 2
	instance.Status.Rollout = &valkeyv1alpha1.RolloutStatus{
		DesiredGeneration: 2, VCPU: 2, RAMGB: 8, Stage: valkeyv1alpha1.RolloutStagePreparing,
	}
	instance.Status.CredentialRotation = &valkeyv1alpha1.CredentialRotationStatus{
		TargetVersion: 2, PreviousVersion: 1,
		Stage: valkeyv1alpha1.CredentialRotationStagePreparing,
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}
	secret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"2"] = []byte(strings.Repeat("cd", 32))
	endpointSlice := testPrimaryEndpointSlice(instance, pod)
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, secret, endpointSlice).
		Build()
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		UpdateAppAccess: func(
			_ context.Context,
			_ string,
			_ string,
			hash string,
			enabled bool,
		) (operatorvalkey.ProcessState, error) {
			return operatorvalkey.ProcessState{
				Role: "primary", RunID: "run-1", AppEnabled: enabled,
				AppPasswordHashes: []string{hash},
			}, nil
		},
	}

	if _, err := reconciler.reconcileAppAdmission(ctx, instance); err != nil {
		t.Fatalf("повторно проверить публичный доступ: %v", err)
	}
	if instance.Status.Phase != valkeyv1alpha1.InstancePhaseUpdating ||
		instance.Status.ObservedGeneration != 1 || instance.Status.Applied == nil ||
		instance.Status.Applied.VCPU != 1 || instance.Status.Applied.RAMGB != 4 ||
		instance.Status.AppliedPasswordVersion != 1 || instance.Status.Rollout == nil ||
		instance.Status.CredentialRotation == nil {
		t.Fatalf("допуск преждевременно подтвердил поколение: %+v", instance.Status)
	}
}

func Test_ReconcilePodMetadata_WithCurrentAndStaleProcessIdentity_SetsOwnerMetadataAndKeepsRoleLabelsOnlyForCurrentProcess(
	t *testing.T,
) {
	ctx := context.Background()
	instance, pod, _, _ := processObservationObjects()
	pod.Labels = workloadLabels(instance.Name)
	pod.Labels["other.example.com/value"] = "kept"
	pod.Annotations = map[string]string{"other.example.com/value": "kept"}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}
	k8s := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(instance, pod).Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s}

	result, err := reconciler.reconcilePodMetadata(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("назначить primary: result=%+v error=%v", result, err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatalf("прочитать Pod с ролью: %v", err)
	}
	if pod.Labels[applicationRoleLabel] != string(valkeyv1alpha1.NodeRolePrimary) ||
		pod.Labels[valkeyv1alpha1.RoleLabelKey] != string(valkeyv1alpha1.NodeRolePrimary) ||
		pod.Labels[valkeyv1alpha1.InstanceIDLabelKey] != instance.Status.AcceptedConfiguration.InstanceID ||
		pod.Labels[valkeyv1alpha1.UserIDLabelKey] != instance.Labels[valkeyv1alpha1.UserIDLabelKey] ||
		pod.Annotations[valkeyv1alpha1.UserEmailAnnotationKey] !=
			instance.Annotations[valkeyv1alpha1.UserEmailAnnotationKey] ||
		pod.Labels["other.example.com/value"] != "kept" || pod.Annotations["other.example.com/value"] != "kept" {
		t.Fatalf("текущий процесс получил неверные метаданные: labels=%v annotations=%v", pod.Labels, pod.Annotations)
	}

	instance.Status.Nodes[0].ContainerID = "containerd://old"
	result, err = reconciler.reconcilePodMetadata(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("снять устаревшую роль: result=%+v error=%v", result, err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatalf("прочитать Pod без роли: %v", err)
	}
	if _, exists := pod.Labels[applicationRoleLabel]; exists {
		t.Fatal("непрефиксная роль осталась у процесса без сохранённой идентичности")
	}
	if _, exists := pod.Labels[valkeyv1alpha1.RoleLabelKey]; exists {
		t.Fatal("префиксная роль осталась у процесса без сохранённой идентичности")
	}
}

func Test_ReconcileProcess_WhenAppIsEnabledBeforeAdmission_DisablesAppAccess(t *testing.T) {
	ctx := context.Background()
	instance, pod, node, secret := processObservationObjects()
	pod.Finalizers = []string{processFinalizer}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, pod, node, secret).
		Build()
	hash := strings.Repeat("ab", 32)
	disableCalls := 0
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		InspectProcess: func(context.Context, string, string, string, bool) (operatorvalkey.ProcessState, error) {
			return operatorvalkey.ProcessState{
				Role: "primary", RunID: "run-1", AppEnabled: true,
				AppPasswordHashes: []string{hash},
			}, nil
		},
		UpdateAppAccess: func(
			_ context.Context,
			_ string,
			_ string,
			observedHash string,
			enabled bool,
		) (operatorvalkey.ProcessState, error) {
			disableCalls++
			if enabled || observedHash != hash {
				t.Fatal("ранний доступ app отключён с неверными параметрами")
			}
			return operatorvalkey.ProcessState{
				Role: "primary", RunID: "run-1", AppPasswordHashes: []string{hash},
			}, nil
		},
	}

	result, err := reconciler.reconcileProcess(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("отключить ранний app: result=%+v error=%v", result, err)
	}
	if disableCalls != 1 || len(instance.Status.Nodes) != 0 {
		t.Fatalf("ранний app не отключён отдельным проходом: calls=%d nodes=%v", disableCalls, instance.Status.Nodes)
	}
}

func Test_EndpointSliceInstanceRequests_WithPrimaryServiceLabel_ReturnsInstanceRequest(t *testing.T) {
	endpointSlice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
		Namespace: "valkey-cache-a1b2c3",
		Labels:    map[string]string{discoveryv1.LabelServiceName: "cache-a1b2c3-primary"},
	}}
	requests := endpointSliceInstanceRequests(context.Background(), endpointSlice)
	if len(requests) != 1 || requests[0].Namespace != endpointSlice.Namespace ||
		requests[0].Name != "cache-a1b2c3" {
		t.Fatalf("неверная очередь EndpointSlice: %+v", requests)
	}
}

func Test_CurrentProcessRoleLabel_WithHAReplica_ReturnsReplicaOnlyForCurrentSynchronizedProcess(t *testing.T) {
	instance, pod, _, _ := processObservationObjects()
	instance.Spec.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	syncedAt := metav1.Now()
	replica := testObservedNode(pod)
	replica.Role = valkeyv1alpha1.NodeRoleReplica
	replica.AppEnabled = true
	replica.AppPasswordVersion = instance.Status.AcceptedConfiguration.PasswordVersion
	replica.Replication = &valkeyv1alpha1.ReplicationStatus{
		LinkUp: true, SyncedAt: &syncedAt, UpstreamHost: "10.42.0.20", UpstreamPort: valkeyPort,
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{replica}
	if role := currentProcessRoleLabel(instance, pod, 0); role != string(valkeyv1alpha1.NodeRoleReplica) {
		t.Fatalf("синхронизированная реплика получила метку %q", role)
	}
	if !setPodMetadata(instance, pod, 0) ||
		pod.Labels[applicationRoleLabel] != string(valkeyv1alpha1.NodeRoleReplica) ||
		pod.Labels[valkeyv1alpha1.RoleLabelKey] != string(valkeyv1alpha1.NodeRoleReplica) {
		t.Fatalf("допущенная реплика получила несогласованные метки: %v", pod.Labels)
	}

	checks := []struct {
		name   string
		mutate func(*valkeyv1alpha1.NodeStatus)
	}{
		{
			name: "при незавершённой синхронизации не назначает реплике роль",
			mutate: func(node *valkeyv1alpha1.NodeStatus) {
				node.Replication.SyncInProgress = true
			},
		},
		{
			name: "при разорванной связи с primary не назначает реплике роль",
			mutate: func(node *valkeyv1alpha1.NodeStatus) {
				node.Replication.LinkUp = false
			},
		},
		{
			name: "без подтверждения синхронизации не назначает реплике роль",
			mutate: func(node *valkeyv1alpha1.NodeStatus) {
				node.Replication.SyncedAt = nil
			},
		},
		{
			name: "до допуска приложения не назначает реплике роль",
			mutate: func(node *valkeyv1alpha1.NodeStatus) {
				node.AppEnabled = false
			},
		},
		{name: "при устаревшем пароле не назначает реплике роль", mutate: func(node *valkeyv1alpha1.NodeStatus) {
			node.AppPasswordVersion = 0
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			changed := *replica.DeepCopy()
			check.mutate(&changed)
			instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{changed}
			if role := currentProcessRoleLabel(instance, pod, 0); role != "" {
				t.Fatalf("непригодная реплика получила метку %q", role)
			}
		})
	}
}

func Test_SetPodMetadata_WithoutOwnerEmail_DoesNotCreateEmptyAnnotation(t *testing.T) {
	instance, pod, _, _ := processObservationObjects()
	delete(instance.Annotations, valkeyv1alpha1.UserEmailAnnotationKey)
	pod.Annotations = map[string]string{"other.example.com/value": "kept"}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}

	if !setPodMetadata(instance, pod, 0) {
		t.Fatal("описательные метки Pod не добавлены")
	}
	if _, found := pod.Annotations[valkeyv1alpha1.UserEmailAnnotationKey]; found {
		t.Fatalf("оператор записал пустую email-аннотацию: %v", pod.Annotations)
	}
	if pod.Annotations["other.example.com/value"] != "kept" {
		t.Fatalf("посторонняя аннотация потеряна: %v", pod.Annotations)
	}
}

func Test_HA01_ReconcileReplicaAdmission_WithInitializedHAAndDisabledPrimaryApp_EnablesSynchronizedReplicaBeforeAssigningRoleMetadata(
	t *testing.T,
) {
	ctx := context.Background()
	instance, primaryPod, _, secret := processObservationObjects()
	instance.Spec.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.Initialized = true
	primary := testObservedNode(primaryPod)
	primary.AppEnabled = false
	primary.AppPasswordVersion = 0
	primaryOrdinal := int32(0)
	instance.Status.PrimaryOrdinal = &primaryOrdinal
	instance.Status.PrimaryPodUID = primary.PodUID
	instance.Status.PrimaryContainerID = primary.ContainerID
	instance.Status.PrimaryRunID = primary.RunID
	instance.Status.PrimaryNodeName = primary.NodeName
	instance.Status.PrimaryNodeUID = primary.NodeUID

	replicaPod := primaryPod.DeepCopy()
	replicaPod.Name = instance.Name + "-1"
	replicaPod.UID = "pod-2"
	replicaPod.Status.PodIP = "10.42.0.11"
	replicaPod.Status.ContainerStatuses[0].ContainerID = "containerd://2"
	replicaPod.Labels = workloadLabels(instance.Name)
	syncedAt := metav1.Now()
	replica := valkeyv1alpha1.NodeStatus{
		Ordinal: 1, PodUID: string(replicaPod.UID),
		ContainerID: replicaPod.Status.ContainerStatuses[0].ContainerID,
		RunID:       "run-2", NodeName: "worker-2", NodeUID: "node-2",
		Role: valkeyv1alpha1.NodeRoleReplica, Readiness: true, AppPasswordVersion: 1,
		Replication: &valkeyv1alpha1.ReplicationStatus{
			UpstreamHost: primaryPod.Status.PodIP, UpstreamPort: valkeyPort,
			LinkUp: true, SyncedAt: &syncedAt,
		},
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{primary, replica}
	if !setPodMetadata(instance, primaryPod, primary.Ordinal) {
		t.Fatal("не удалось подготовить метаданные primary")
	}
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, primaryPod, replicaPod, secret).
		Build()
	updates := 0
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s,
		UpdateAppAccess: func(
			_ context.Context,
			address string,
			_ string,
			hash string,
			enabled bool,
		) (operatorvalkey.ProcessState, error) {
			updates++
			if address != "10.42.0.11:6379" || !enabled {
				t.Fatalf("неверная цель допуска реплики: %s enabled=%t", address, enabled)
			}
			return operatorvalkey.ProcessState{
				Role: "replica", RunID: "run-2", AppEnabled: true,
				AppPasswordHashes: []string{hash},
				Replication: &operatorvalkey.ReplicationState{
					UpstreamHost: primaryPod.Status.PodIP, UpstreamPort: valkeyPort, LinkUp: true,
				},
			}, nil
		},
	}
	instance.Status.Initialized = false
	result, err := reconciler.reconcileReplicaAdmission(ctx, instance)
	if err != nil || !result.IsZero() || updates != 0 {
		t.Fatalf("реплика допущена до инициализации HA: result=%+v updates=%d error=%v", result, updates, err)
	}
	instance.Status.Initialized = true
	instance.Status.CredentialRotation = &valkeyv1alpha1.CredentialRotationStatus{
		TargetVersion: 2, PreviousVersion: 1,
		Stage: valkeyv1alpha1.CredentialRotationStageUpdatingReplicas,
	}
	result, err = reconciler.reconcileReplicaAdmission(ctx, instance)
	if err != nil || !result.IsZero() || updates != 0 {
		t.Fatalf("реплика допущена посреди ротации: result=%+v updates=%d error=%v", result, updates, err)
	}
	instance.Status.CredentialRotation = nil

	result, err = reconciler.reconcileReplicaAdmission(ctx, instance)
	if err != nil || result.IsZero() || updates != 1 {
		t.Fatalf("допустить реплику: result=%+v updates=%d error=%v", result, updates, err)
	}
	observed := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observed); err != nil {
		t.Fatalf("прочитать подтверждение реплики: %v", err)
	}
	node, found := nodeStatusAtOrdinal(observed.Status.Nodes, 1)
	if !found || !node.AppEnabled || node.AppPasswordVersion != 1 {
		t.Fatalf("допуск реплики не сохранён: %+v", observed.Status.Nodes)
	}
	result, err = reconciler.reconcilePodMetadata(ctx, observed)
	if err != nil || result.IsZero() {
		t.Fatalf("назначить роль допущенной реплике: result=%+v error=%v", result, err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(replicaPod), replicaPod); err != nil {
		t.Fatalf("прочитать Pod допущенной реплики: %v", err)
	}
	if replicaPod.Labels[applicationRoleLabel] != string(valkeyv1alpha1.NodeRoleReplica) ||
		replicaPod.Labels[valkeyv1alpha1.RoleLabelKey] != string(valkeyv1alpha1.NodeRoleReplica) {
		t.Fatalf("допущенная реплика получила неверные метки роли: %v", replicaPod.Labels)
	}
}

func testObservedNode(pod *corev1.Pod) valkeyv1alpha1.NodeStatus {
	return valkeyv1alpha1.NodeStatus{
		Ordinal: 0, PodUID: string(pod.UID), ContainerID: pod.Status.ContainerStatuses[0].ContainerID,
		RunID: "run-1", NodeName: pod.Spec.NodeName, NodeUID: "node-1",
		Role: valkeyv1alpha1.NodeRolePrimary, Readiness: true,
	}
}

func testPrimaryEndpointSlice(
	instance *valkeyv1alpha1.ValkeyInstance,
	pod *corev1.Pod,
) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name + "-primary-1",
			Namespace: instance.Namespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: instance.Name + "-primary",
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports: []discoveryv1.EndpointPort{{
			Name: ptr.To("valkey"), Port: ptr.To[int32](valkeyPort), Protocol: ptr.To(corev1.ProtocolTCP),
		}},
		Endpoints: []discoveryv1.Endpoint{{
			Addresses: []string{pod.Status.PodIP},
			Conditions: discoveryv1.EndpointConditions{
				Ready: ptr.To(true),
			},
			TargetRef: &corev1.ObjectReference{
				APIVersion: "v1", Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID,
			},
		}},
	}
}
