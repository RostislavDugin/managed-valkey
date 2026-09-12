package operator

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func Test_DesiredPod_WithSingleMode_CreatesProcessWithSecurityAndResourceContract(t *testing.T) {
	instance := completeAcceptedInstance()
	pod := desiredPod(instance, 0, "cache-a1b2c3-config-digest", "valkey/valkey:8.1.9", nil)

	if pod.Name != "cache-a1b2c3-0" || pod.Spec.Hostname != pod.Name ||
		pod.Spec.Subdomain != "cache-a1b2c3-hl" ||
		pod.Spec.RestartPolicy != corev1.RestartPolicyNever ||
		!slices.Equal(pod.Finalizers, []string{processFinalizer}) {
		t.Fatalf("неверный Pod: %+v", pod)
	}

	container := pod.Spec.Containers[0]
	if container.RestartPolicy != nil || len(container.RestartPolicyRules) != 0 {
		t.Fatalf("restartPolicy контейнера: %v", container.RestartPolicy)
	}
	if !maps.Equal(container.Resources.Requests, container.Resources.Limits) {
		t.Fatalf("requests и limits различаются: %+v", container.Resources)
	}
	if container.ReadinessProbe == nil || container.LivenessProbe != nil {
		t.Fatalf(
			"неверные проверки контейнера: readiness=%v liveness=%v",
			container.ReadinessProbe,
			container.LivenessProbe,
		)
	}
	if strings.Contains(strings.Join(container.Command, " "), "password") {
		t.Fatalf("аргументы содержат секрет: %v", container.Command)
	}

	foundMemoryVolume := false
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "runtime" && volume.EmptyDir != nil && volume.EmptyDir.Medium == corev1.StorageMediumMemory {
			foundMemoryVolume = true
		}
	}
	if !foundMemoryVolume {
		t.Fatal("нет runtime emptyDir в памяти")
	}
	if !podOwnedByInstance(pod, instance) {
		t.Fatalf("неверный ownerReference: %v", pod.OwnerReferences)
	}
}

func Test_DesiredPod_WithRequestOverrides_PreservesConfiguredLimits(t *testing.T) {
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.VCPU = 4
	instance.Status.AcceptedConfiguration.RAMGB = 16
	requests := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	}

	pod := desiredPod(
		instance,
		0,
		"cache-a1b2c3-config-digest",
		"valkey/valkey:8.1.9",
		requests,
	)
	resources := pod.Spec.Containers[0].Resources
	expectedRequestCPU := requests[corev1.ResourceCPU]
	expectedRequestMemory := requests[corev1.ResourceMemory]
	expectedLimitCPU := resource.MustParse("4")
	expectedLimitMemory := resource.MustParse("16Gi")
	if resources.Requests.Cpu().Cmp(expectedRequestCPU) != 0 ||
		resources.Requests.Memory().Cmp(expectedRequestMemory) != 0 ||
		resources.Limits.Cpu().Cmp(expectedLimitCPU) != 0 ||
		resources.Limits.Memory().Cmp(expectedLimitMemory) != 0 {
		t.Fatalf("ресурсы Valkey не совпали: %+v", resources)
	}
}

func Test_DesiredOperatorResources_WithOwnerMetadata_AddsDescriptionsWithoutChangingSelectors(t *testing.T) {
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.Whitelist = valkeyv1alpha1.WhitelistSpec{
		IsEnabled: true,
		CIDRs:     []string{"192.0.2.0/24"},
	}
	reconciler := &ValkeyInstanceReconciler{Scheme: NewScheme()}
	configMap, err := reconciler.desiredConfigMap(instance)
	if err != nil {
		t.Fatalf("сформировать ConfigMap: %v", err)
	}
	pod := desiredPod(instance, 0, configMap.Name, "valkey/valkey:8.1.9", nil)
	services := desiredServices(instance)
	networkPolicy := desiredNetworkPolicy(instance, "valkey-system", nil)
	pdb := desiredPodDisruptionBudget(instance)
	endpoint := networkEndpoints(instance)[0]
	route := desiredTCPRoute(instance, "valkey-system", endpoint)
	securityPolicy := desiredSecurityPolicy(instance, endpoint)
	objects := []metav1.Object{
		pod,
		configMap,
		networkPolicy,
		pdb,
		route,
		securityPolicy,
	}
	for _, service := range services {
		objects = append(objects, service)
	}
	for _, object := range objects {
		assertOwnerMetadata(t, object, instance)
	}

	selectorLabels := workloadLabels(instance.Name)
	if !maps.Equal(pdb.Spec.Selector.MatchLabels, selectorLabels) ||
		!maps.Equal(networkPolicy.Spec.PodSelector.MatchLabels, selectorLabels) {
		t.Fatalf(
			"описательные метки попали в селектор: pdb=%v networkPolicy=%v",
			pdb.Spec.Selector.MatchLabels,
			networkPolicy.Spec.PodSelector.MatchLabels,
		)
	}
	for _, service := range services {
		for key := range map[string]struct{}{
			valkeyv1alpha1.InstanceIDLabelKey: {},
			valkeyv1alpha1.UserIDLabelKey:     {},
		} {
			if _, found := service.Spec.Selector[key]; found {
				t.Fatalf("описательная метка %s попала в селектор Service %s", key, service.Name)
			}
		}
	}
}

func Test_DesiredServicesAndNetworkPolicy_WithSingleMode_ExposePrimaryAndRestrictTraffic(t *testing.T) {
	instance := completeAcceptedInstance()
	endpoints := networkEndpoints(instance)
	if len(endpoints) != 2 || endpoints[0].suffix != "" || endpoints[0].backendSuffix != "-primary" ||
		endpoints[1].suffix != "-ro" || endpoints[1].backendSuffix != "-primary" {
		t.Fatalf("single получил неверные публичные endpoints: %+v", endpoints)
	}
	readRoute := desiredTCPRoute(instance, "valkey-system", endpoints[1])
	if readRoute.Name != "cache-a1b2c3-ro" ||
		string(readRoute.Spec.Rules[0].BackendRefs[0].Name) != "cache-a1b2c3-primary" {
		t.Fatalf("маршрут чтения single не ведёт на primary: %+v", readRoute.Spec)
	}
	services := desiredServices(instance)
	if len(services) != 2 || services[0].Name != "cache-a1b2c3-hl" || services[1].Name != "cache-a1b2c3-primary" {
		t.Fatalf("созданы неверные Services: %v, %v", services[0].Name, services[1].Name)
	}
	if services[0].Spec.ClusterIP != corev1.ClusterIPNone || !services[0].Spec.PublishNotReadyAddresses {
		t.Fatalf("неверный headless Service: %+v", services[0].Spec)
	}
	if services[1].Spec.Selector[applicationRoleLabel] != string(valkeyv1alpha1.NodeRolePrimary) {
		t.Fatalf("primary Service не выбирает primary: %v", services[1].Spec.Selector)
	}

	policy := desiredNetworkPolicy(instance, "valkey-system", []string{"172.27.0.1/32"})
	if !maps.Equal(policy.Spec.PodSelector.MatchLabels, workloadLabels("cache-a1b2c3")) {
		t.Fatalf("NetworkPolicy выбрала чужие поды: %v", policy.Spec.PodSelector.MatchLabels)
	}
	if len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].From) != 4 ||
		policy.Spec.Ingress[0].From[3].IPBlock == nil ||
		policy.Spec.Ingress[0].From[3].IPBlock.CIDR != "172.27.0.1/32" ||
		len(policy.Spec.Egress) != 2 {
		t.Fatalf("неверные правила NetworkPolicy: %+v", policy.Spec)
	}
}

func Test_ReconcileValkeyImage_WhenEnvironmentChanges_PreservesSavedImage(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	scheme := NewScheme()
	const originalImage = "valkey/valkey:8.1.9"
	instance.Status.ValkeyImage = originalImage
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, ValkeyImage: "valkey/valkey:9.0.0"}

	if blocked, changed, err := reconciler.reconcileValkeyImage(ctx, instance); err != nil || blocked || !changed {
		t.Fatalf("сверить сменившийся образ: blocked=%t changed=%t error=%v", blocked, changed, err)
	}
	observedInstance := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observedInstance); err != nil {
		t.Fatalf("прочитать ValkeyInstance: %v", err)
	}
	condition := apimeta.FindStatusCondition(observedInstance.Status.Conditions, conditionTypeRecoveryRequired)
	if condition == nil || condition.Reason != "ValkeyImageChanged" {
		t.Fatalf("нет RecoveryRequired для смены образа: %+v", condition)
	}
	if observedInstance.Status.ValkeyImage != originalImage {
		t.Fatalf("сохранённый образ изменён на %q", observedInstance.Status.ValkeyImage)
	}

	reconciler.ValkeyImage = originalImage
	if _, _, err := reconciler.reconcileValkeyImage(ctx, observedInstance); err != nil {
		t.Fatalf("сверить восстановленный образ: %v", err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observedInstance); err != nil {
		t.Fatalf("прочитать восстановленный ValkeyInstance: %v", err)
	}
	if condition := apimeta.FindStatusCondition(
		observedInstance.Status.Conditions,
		conditionTypeRecoveryRequired,
	); condition != nil {
		t.Fatalf("RecoveryRequired сохранился после возврата образа: %+v", condition)
	}
}

func Test_ReconcileValkeyImage_WithoutProcessHistory_PersistsImageBeforeCreation(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Status.ValkeyImage = ""
	scheme := NewScheme()
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s, Scheme: scheme, ValkeyImage: "valkey:fixed"}

	blocked, changed, err := reconciler.reconcileValkeyImage(ctx, instance)
	if err != nil || blocked || !changed {
		t.Fatalf("сохранить первоначальный образ: blocked=%t changed=%t error=%v", blocked, changed, err)
	}
	observed := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observed); err != nil {
		t.Fatalf("прочитать сохранённый образ: %v", err)
	}
	if observed.Status.ValkeyImage != "valkey:fixed" {
		t.Fatalf("сохранён образ %q", observed.Status.ValkeyImage)
	}

	blocked, changed, err = (&ValkeyInstanceReconciler{
		Client: k8s, APIReader: k8s, Scheme: scheme, ValkeyImage: "valkey:fixed",
	}).reconcileValkeyImage(ctx, observed)
	if err != nil || blocked || changed {
		t.Fatalf("повтор изменил сохранённый образ: blocked=%t changed=%t error=%v", blocked, changed, err)
	}
}

func Test_ReconcileValkeyImage_WithProcessHistoryAndMissingImage_BlocksCreation(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Status.ValkeyImage = ""
	instance.Status.Initialized = true
	scheme := NewScheme()
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s, Scheme: scheme, ValkeyImage: "valkey:new"}

	blocked, changed, err := reconciler.reconcileValkeyImage(ctx, instance)
	if err != nil || !blocked || !changed {
		t.Fatalf("история без образа не заблокирована: blocked=%t changed=%t error=%v", blocked, changed, err)
	}
	condition := apimeta.FindStatusCondition(instance.Status.Conditions, conditionTypeRecoveryRequired)
	if condition == nil || condition.Reason != "ValkeyImageMissing" || instance.Status.ValkeyImage != "" {
		t.Fatalf(
			"неверная диагностика отсутствующего образа: condition=%+v image=%q",
			condition,
			instance.Status.ValkeyImage,
		)
	}
	pods := &corev1.PodList{}
	if err := k8s.List(ctx, pods, client.InNamespace(instance.Namespace)); err != nil || len(pods.Items) != 0 {
		t.Fatalf("до восстановления образа появился Pod: count=%d error=%v", len(pods.Items), err)
	}
}

func Test_EnsurePod_WhenOwnedPodExists_DoesNotCreateAgain(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	desired := desiredPod(instance, 0, "cache-a1b2c3-config-digest", "valkey/valkey:8.1.9", nil)
	current := desired.DeepCopy()
	current.Labels["example.com/unknown"] = "label"
	current.Annotations["example.com/unknown"] = "annotation"
	scheme := NewScheme()
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(current).Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, Scheme: scheme}

	changed, blocked, err := reconciler.ensurePod(ctx, instance, desired)
	if err != nil || changed || blocked {
		t.Fatalf("существующий Pod вызвал повторное создание: changed=%t blocked=%t error=%v", changed, blocked, err)
	}
}

func Test_EnsurePod_WhenNameBelongsToAnotherOwner_ReportsConflictWithoutMutation(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	desired := desiredPod(instance, 0, "cache-a1b2c3-config-digest", "valkey/valkey:8.1.9", nil)
	foreign := desired.DeepCopy()
	foreign.OwnerReferences = nil
	foreign.Labels = map[string]string{"foreign.example/name": "kept"}
	scheme := NewScheme()
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, foreign).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s, Scheme: scheme}

	changed, blocked, err := reconciler.ensurePod(ctx, instance, desired)
	if err != nil || !changed || !blocked {
		t.Fatalf("конфликт владельца не остановил создание: changed=%t blocked=%t error=%v", changed, blocked, err)
	}
	observed := &corev1.Pod{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(foreign), observed); err != nil {
		t.Fatalf("прочитать чужой Pod: %v", err)
	}
	if !maps.Equal(observed.Labels, foreign.Labels) || len(observed.OwnerReferences) != 0 {
		t.Fatalf("чужой Pod изменён: labels=%v owners=%v", observed.Labels, observed.OwnerReferences)
	}
	condition := apimeta.FindStatusCondition(instance.Status.Conditions, conditionTypeRecoveryRequired)
	if condition == nil || condition.Reason != "PodOwnershipConflict" {
		t.Fatalf("нет диагностики конфликта владельца: %+v", condition)
	}
}

func Test_EnsurePod_AfterLostCreateResponse_ReusesCreatedObject(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	desired := desiredPod(instance, 0, "cache-a1b2c3-config-digest", "valkey/valkey:8.1.9", nil)
	scheme := NewScheme()
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	lost := errors.New("ответ создания потерян")
	reconciler := &ValkeyInstanceReconciler{
		Client: &createThenErrorClient{Client: base, err: lost}, APIReader: base, Scheme: scheme,
	}

	if _, _, err := reconciler.ensurePod(ctx, instance, desired.DeepCopy()); !errors.Is(err, lost) {
		t.Fatalf("потерянный ответ не возвращён: %v", err)
	}
	changed, blocked, err := reconciler.ensurePod(ctx, instance, desired.DeepCopy())
	if err != nil || changed || blocked {
		t.Fatalf("повтор не переиспользовал Pod: changed=%t blocked=%t error=%v", changed, blocked, err)
	}
	pods := &corev1.PodList{}
	if err := base.List(ctx, pods, client.InNamespace(instance.Namespace)); err != nil || len(pods.Items) != 1 {
		t.Fatalf("после повтора создано Pod: count=%d error=%v", len(pods.Items), err)
	}
}

func Test_EnsurePod_WhenCachedReadMissesExistingObject_HandlesAlreadyExists(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	desired := desiredPod(instance, 0, "cache-a1b2c3-config-digest", "valkey/valkey:8.1.9", nil)
	scheme := NewScheme()
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, desired.DeepCopy()).
		Build()
	reconciler := &ValkeyInstanceReconciler{
		Client: &firstPodReadMissClient{Client: base}, APIReader: base, Scheme: scheme,
	}

	changed, blocked, err := reconciler.ensurePod(ctx, instance, desired.DeepCopy())
	if err != nil || changed || blocked {
		t.Fatalf("AlreadyExists не разрешён перечитыванием: changed=%t blocked=%t error=%v", changed, blocked, err)
	}
}

func Test_ReconcileLegacyStatefulSet_WithUserWorkload_BlocksDirectPods(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	legacy := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: instance.Name, Namespace: instance.Namespace, Labels: workloadLabels(instance.Name),
	}}
	operatorWorkload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "managed-valkey-operator", Namespace: "valkey-system",
	}}
	scheme := NewScheme()
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, legacy, operatorWorkload).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s, Scheme: scheme}

	blocked, changed, err := reconciler.reconcileLegacyStatefulSet(ctx, instance)
	if err != nil || !blocked || !changed {
		t.Fatalf("прежний StatefulSet не заблокирован: blocked=%t changed=%t error=%v", blocked, changed, err)
	}
	condition := apimeta.FindStatusCondition(instance.Status.Conditions, conditionTypeRecoveryRequired)
	if condition == nil || condition.Reason != "LegacyStatefulSet" {
		t.Fatalf("нет диагностики прежнего StatefulSet: %+v", condition)
	}
	if requests := statefulSetInstanceRequests(ctx, operatorWorkload); len(requests) != 0 {
		t.Fatalf("StatefulSet оператора поставил пользовательский инстанс в очередь: %+v", requests)
	}
}

func Test_DesiredPodAndDisruptionBudget_WithHAMode_CreateSoftTopologySpreadAndMinimumAvailability(
	t *testing.T,
) {
	instance := completeAcceptedInstance()
	instance.Spec.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	pod := desiredPod(instance, 2, "cache-a1b2c3-config-digest", "valkey/valkey:8.1.9", nil)

	container := pod.Spec.Containers[0]
	if pod.Name != "cache-a1b2c3-2" || pod.Spec.RestartPolicy != corev1.RestartPolicyNever ||
		container.RestartPolicy != nil || len(container.RestartPolicyRules) != 0 {
		t.Fatalf("неверная политика HA Pod: %+v", pod.Spec)
	}
	if pod.Spec.Affinity != nil {
		t.Fatalf("HA получил обязательное affinity: %+v", pod.Spec.Affinity)
	}
	if len(pod.Spec.TopologySpreadConstraints) != 1 {
		t.Fatalf("HA не получил мягкое распределение: %+v", pod.Spec.TopologySpreadConstraints)
	}
	constraint := pod.Spec.TopologySpreadConstraints[0]
	if constraint.MaxSkew != 1 || constraint.TopologyKey != corev1.LabelHostname ||
		constraint.WhenUnsatisfiable != corev1.ScheduleAnyway || constraint.MinDomains != nil ||
		constraint.LabelSelector == nil ||
		!maps.Equal(constraint.LabelSelector.MatchLabels, workloadLabels(instance.Name)) {
		t.Fatalf("неверное мягкое распределение: %+v", constraint)
	}

	pdb := desiredPodDisruptionBudget(instance)
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntVal != 2 ||
		pdb.Spec.Selector == nil ||
		!maps.Equal(pdb.Spec.Selector.MatchLabels, workloadLabels(instance.Name)) {
		t.Fatalf("неверный PDB HA: %+v", pdb.Spec)
	}
	services := desiredServices(instance)
	if len(services) != 3 || services[2].Name != instance.Name+"-replicas" ||
		services[2].Spec.Selector[applicationRoleLabel] != string(valkeyv1alpha1.NodeRoleReplica) {
		t.Fatalf("неверный Service реплик: %+v", services)
	}
}

func Test_DesiredPod_WithSingleMode_HasNoTopologySpreadConstraint(t *testing.T) {
	instance := completeAcceptedInstance()
	pod := desiredPod(instance, 0, "cache-a1b2c3-config-digest", "valkey/valkey:8.1.9", nil)

	if pod.Spec.Affinity != nil || len(pod.Spec.TopologySpreadConstraints) != 0 {
		t.Fatalf("single получил ограничение распределения: %+v", pod.Spec)
	}
}

func Test_ReconcileWorkloadResources_DuringHARollout_CreatesOnlyMissingTargetPod(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.VCPU = 4
	instance.Status.AcceptedConfiguration.RAMGB = 8
	instance.Status.Applied = &valkeyv1alpha1.AppliedConfiguration{
		Mode: valkeyv1alpha1.ValkeyModeHA, VCPU: 2, RAMGB: 4,
	}
	instance.Status.Rollout = &valkeyv1alpha1.RolloutStatus{
		DesiredGeneration: 2, VCPU: 4, RAMGB: 8, Image: instance.Status.ValkeyImage,
		Stage: valkeyv1alpha1.RolloutStageReplacingReplicas,
	}
	oldInstance := instance.DeepCopy()
	oldInstance.Status.AcceptedConfiguration.VCPU = 2
	oldInstance.Status.AcceptedConfiguration.RAMGB = 4
	oldInstance.Status.Rollout = nil
	oldConfig, err := (&ValkeyInstanceReconciler{}).desiredConfigMap(oldInstance)
	if err != nil {
		t.Fatalf("сформировать прежнюю ConfigMap: %v", err)
	}
	oldZero := desiredPod(oldInstance, 0, oldConfig.Name, instance.Status.ValkeyImage, nil)
	oldTwo := desiredPod(oldInstance, 2, oldConfig.Name, instance.Status.ValkeyImage, nil)
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{
		{Ordinal: 0, PodUID: "old-0"},
		{Ordinal: 1, PodUID: "old-1", Termination: &valkeyv1alpha1.ProcessTermination{Evidence: "container_status"}},
		{Ordinal: 2, PodUID: "old-2"},
	}
	scheme := NewScheme()
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, oldZero, oldTwo).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s, Scheme: scheme}

	if _, err := reconciler.reconcileWorkloadResources(ctx, instance); err != nil {
		t.Fatalf("создать недостающий Pod: %v", err)
	}
	for ordinal, expectedRAM := range []int64{4, 8, 4} {
		pod := &corev1.Pod{}
		key := client.ObjectKey{Namespace: instance.Namespace, Name: fmt.Sprintf("%s-%d", instance.Name, ordinal)}
		if err := k8s.Get(ctx, key, pod); err != nil {
			t.Fatalf("прочитать Pod %d: %v", ordinal, err)
		}
		if got := pod.Spec.Containers[0].Resources.Limits.Memory(); got == nil ||
			got.CmpInt64(expectedRAM*gibibyte) != 0 {
			t.Fatalf("Pod %d получил RAM %v вместо %d GiB", ordinal, got, expectedRAM)
		}
	}
	if configName := podConfigMapReference(
		t,
		readPod(t, ctx, k8s, instance.Namespace, instance.Name+"-1"),
	); configName != rolloutConfigMapName(
		instance,
	) {
		t.Fatalf("замена использует ConfigMap %q вместо %q", configName, rolloutConfigMapName(instance))
	}
	if configName := podConfigMapReference(
		t,
		readPod(t, ctx, k8s, instance.Namespace, instance.Name+"-0"),
	); configName != oldConfig.Name {
		t.Fatalf("прежний Pod изменил ConfigMap на %q", configName)
	}
}

func Test_ReconcileWorkloadResources_AfterConfirmedFullStop_RecreatesAllPodsFromSavedTarget(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.VCPU = 4
	instance.Status.AcceptedConfiguration.RAMGB = 8
	instance.Status.Applied = &valkeyv1alpha1.AppliedConfiguration{
		Mode: valkeyv1alpha1.ValkeyModeHA, VCPU: 2, RAMGB: 16,
	}
	instance.Status.Rollout = &valkeyv1alpha1.RolloutStatus{
		DesiredGeneration: 2, VCPU: 4, RAMGB: 8, Image: instance.Status.ValkeyImage,
		Stage: valkeyv1alpha1.RolloutStageStarting, AccessClosed: true,
	}
	for ordinal := range int32(3) {
		instance.Status.Nodes = append(instance.Status.Nodes, valkeyv1alpha1.NodeStatus{
			Ordinal: ordinal, PodUID: fmt.Sprintf("old-%d", ordinal),
			Termination: &valkeyv1alpha1.ProcessTermination{Evidence: "container_status"},
		})
	}
	scheme := NewScheme()
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s, Scheme: scheme}

	if _, err := reconciler.reconcileWorkloadResources(ctx, instance); err != nil {
		t.Fatalf("восстановить состав после полной остановки: %v", err)
	}
	pods := &corev1.PodList{}
	if err := k8s.List(ctx, pods, client.InNamespace(instance.Namespace)); err != nil || len(pods.Items) != 3 {
		t.Fatalf("восстановлен неверный состав: count=%d error=%v", len(pods.Items), err)
	}
	for index := range pods.Items {
		container := pods.Items[index].Spec.Containers[0]
		if container.Image != instance.Status.ValkeyImage ||
			container.Resources.Limits.Cpu().CmpInt64(4) != 0 ||
			container.Resources.Limits.Memory().CmpInt64(8*gibibyte) != 0 {
			t.Fatalf("Pod восстановлен с неверной конфигурацией: %+v", container)
		}
	}
}

func Test_ReconcileWorkloadResources_WhenPreviousProcessHasNoTerminationProof_DoesNotCreateReplacement(
	t *testing.T,
) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Status.Initialized = true
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{{
		Ordinal: 0, PodUID: "old-pod", ContainerID: "containerd://old", RunID: "old-run",
		NodeName: "worker-1", NodeUID: "node-1",
	}}
	scheme := NewScheme()
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s, Scheme: scheme}

	if _, err := reconciler.reconcileWorkloadResources(ctx, instance); err != nil {
		t.Fatalf("проверить отсутствие доказательства остановки: %v", err)
	}
	pods := &corev1.PodList{}
	if err := k8s.List(ctx, pods, client.InNamespace(instance.Namespace)); err != nil {
		t.Fatalf("прочитать Pod: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("без доказательства остановки создано Pod: %+v", pods.Items)
	}

	instance.Status.Nodes[0].Termination = &valkeyv1alpha1.ProcessTermination{Evidence: "container_status"}
	if _, err := reconciler.reconcileWorkloadResources(ctx, instance); err != nil {
		t.Fatalf("создать замену после доказательства: %v", err)
	}
	if err := k8s.List(ctx, pods, client.InNamespace(instance.Namespace)); err != nil || len(pods.Items) != 1 {
		t.Fatalf("после доказательства создано Pod=%d: %v", len(pods.Items), err)
	}
}

func Test_ReconcileWorkloadResources_WhenTerminatedPodStillExists_DoesNotCreateAnotherIncarnation(
	t *testing.T,
) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Status.Initialized = true
	oldPod := desiredPod(instance, 0, "old-config", instance.Status.ValkeyImage, nil)
	oldPod.UID = "old-pod"
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{{
		Ordinal: 0, PodUID: string(oldPod.UID), ContainerID: "containerd://old", RunID: "old-run",
		NodeName: "worker-1", NodeUID: "node-1",
		Termination: &valkeyv1alpha1.ProcessTermination{Evidence: "container_status"},
	}}
	scheme := NewScheme()
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, oldPod).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s, Scheme: scheme}

	if _, err := reconciler.reconcileWorkloadResources(ctx, instance); err != nil {
		t.Fatalf("сверить удержанный Pod: %v", err)
	}
	current := readPod(t, ctx, k8s, instance.Namespace, oldPod.Name)
	if current.UID != oldPod.UID || podConfigMapReference(t, current) != "old-config" {
		t.Fatalf("удержанный Pod заменён: uid=%s config=%s", current.UID, podConfigMapReference(t, current))
	}

	current.Finalizers = nil
	if err := k8s.Update(ctx, current); err != nil {
		t.Fatalf("снять finalizer в подготовке теста: %v", err)
	}
	if err := k8s.Delete(ctx, current); err != nil {
		t.Fatalf("удалить доказанно завершённый Pod: %v", err)
	}
	if _, err := reconciler.reconcileWorkloadResources(ctx, instance); err != nil {
		t.Fatalf("создать следующую инкарнацию: %v", err)
	}
	replacement := readPod(t, ctx, k8s, instance.Namespace, oldPod.Name)
	if replacement.UID == oldPod.UID || podConfigMapReference(t, replacement) == "old-config" {
		t.Fatalf("не создана целевая инкарнация: uid=%s config=%s",
			replacement.UID, podConfigMapReference(t, replacement))
	}
}

func Test_RequestWorkloadStop_WhenDeleteResponseIsLost_RetriesWithExactPodPreconditions(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	pod := desiredPod(instance, 0, "config", instance.Status.ValkeyImage, nil)
	pod.UID = "pod-uid"
	pod.ResourceVersion = "17"
	scheme := NewScheme()
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	lost := &lostDeleteResponseClient{Client: k8s}
	reconciler := &ValkeyInstanceReconciler{Client: lost, APIReader: k8s, Scheme: scheme}

	if _, err := reconciler.requestWorkloadStop(ctx, instance); err == nil {
		t.Fatal("потерянный ответ Delete не возвращён")
	}
	options := lost.options
	if options == nil || options.Preconditions == nil || options.Preconditions.UID == nil ||
		*options.Preconditions.UID != pod.UID || options.Preconditions.ResourceVersion == nil ||
		*options.Preconditions.ResourceVersion != pod.ResourceVersion {
		t.Fatalf("DELETE потерял предусловия Pod: %+v", options)
	}
	changed, err := reconciler.requestWorkloadStop(ctx, instance)
	if err != nil || changed {
		t.Fatalf("повтор DELETE не распознал удаляемый Pod: changed=%t error=%v", changed, err)
	}
}

func completeAcceptedInstance() *valkeyv1alpha1.ValkeyInstance {
	return &valkeyv1alpha1.ValkeyInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cache-a1b2c3", Namespace: "valkey-cache-a1b2c3", UID: "instance-uid",
			Labels: map[string]string{
				valkeyv1alpha1.UserIDLabelKey: "01991ad0-1234-7000-8000-000000000010",
			},
			Annotations: map[string]string{
				valkeyv1alpha1.UserEmailAnnotationKey: "owner@example.com",
			},
		},
		Status: valkeyv1alpha1.ValkeyInstanceStatus{
			ValkeyImage: "valkey/valkey:8.1.9",
			AcceptedConfiguration: &valkeyv1alpha1.AcceptedConfiguration{
				InstanceID: "01991ad0-1234-7000-8000-000000000001",
				Slug:       "cache-a1b2c3", Mode: valkeyv1alpha1.ValkeyModeSingle, VCPU: 2, RAMGB: 4,
			},
		},
	}
}

func assertOwnerMetadata(
	t *testing.T,
	object metav1.Object,
	instance *valkeyv1alpha1.ValkeyInstance,
) {
	t.Helper()
	labels := object.GetLabels()
	if labels[instanceLabelKey] != instance.Name ||
		labels[valkeyv1alpha1.InstanceIDLabelKey] != instance.Status.AcceptedConfiguration.InstanceID ||
		labels[valkeyv1alpha1.UserIDLabelKey] != instance.Labels[valkeyv1alpha1.UserIDLabelKey] ||
		object.GetAnnotations()[valkeyv1alpha1.UserEmailAnnotationKey] !=
			instance.Annotations[valkeyv1alpha1.UserEmailAnnotationKey] {
		t.Fatalf("объект %s получил неверные метаданные: labels=%v annotations=%v",
			object.GetName(), labels, object.GetAnnotations())
	}
}

func readPod(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	namespace string,
	name string,
) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pod); err != nil {
		t.Fatalf("прочитать Pod %s: %v", name, err)
	}

	return pod
}

func podConfigMapReference(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "config" && volume.ConfigMap != nil {
			return volume.ConfigMap.Name
		}
	}
	t.Fatalf("Pod %s не содержит ConfigMap конфигурации", pod.Name)
	return ""
}

type createThenErrorClient struct {
	client.Client
	err error
}

func (c *createThenErrorClient) Create(
	ctx context.Context,
	object client.Object,
	options ...client.CreateOption,
) error {
	if err := c.Client.Create(ctx, object, options...); err != nil {
		return err
	}
	if _, ok := object.(*corev1.Pod); ok && c.err != nil {
		err := c.err
		c.err = nil
		return err
	}

	return nil
}

type firstPodReadMissClient struct {
	client.Client
	missed bool
}

func (c *firstPodReadMissClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if _, ok := object.(*corev1.Pod); ok && !c.missed {
		c.missed = true
		return apierrors.NewNotFound(corev1.Resource("pods"), key.Name)
	}

	return c.Client.Get(ctx, key, object, options...)
}
