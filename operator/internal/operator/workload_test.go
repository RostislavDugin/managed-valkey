package operator

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func Test_DesiredStatefulSet_WithSingleMode_CreatesOnePodWithSecurityAndResourceContract(t *testing.T) {
	instance := completeAcceptedInstance()
	statefulSet := desiredStatefulSet(instance, "cache-a1b2c3-config-digest", "valkey/valkey:8.1.9", nil)

	if statefulSet.Spec.Replicas == nil || *statefulSet.Spec.Replicas != 1 ||
		statefulSet.Spec.PodManagementPolicy != appsv1.ParallelPodManagement ||
		statefulSet.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType {
		t.Fatalf("неверная стратегия StatefulSet: %+v", statefulSet.Spec)
	}
	if len(statefulSet.Spec.VolumeClaimTemplates) != 0 {
		t.Fatal("StatefulSet создаёт постоянное хранилище")
	}
	if statefulSet.Spec.Selector == nil ||
		!maps.Equal(statefulSet.Spec.Selector.MatchLabels, workloadLabels(instance.Name)) ||
		!selectorMatchesLabels(statefulSet.Spec.Selector.MatchLabels, statefulSet.Spec.Template.Labels) {
		t.Fatalf("селектор single изменён: selector=%+v labels=%v",
			statefulSet.Spec.Selector, statefulSet.Spec.Template.Labels)
	}
	if statefulSet.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyAlways ||
		!slices.Equal(statefulSet.Spec.Template.Finalizers, []string{processFinalizer}) {
		t.Fatalf("неверная политика Pod: %+v", statefulSet.Spec.Template)
	}

	container := statefulSet.Spec.Template.Spec.Containers[0]
	if container.RestartPolicy == nil || *container.RestartPolicy != corev1.ContainerRestartPolicyNever {
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
	for _, volume := range statefulSet.Spec.Template.Spec.Volumes {
		if volume.Name == "runtime" && volume.EmptyDir != nil && volume.EmptyDir.Medium == corev1.StorageMediumMemory {
			foundMemoryVolume = true
		}
	}
	if !foundMemoryVolume {
		t.Fatal("нет runtime emptyDir в памяти")
	}
	if len(statefulSet.OwnerReferences) != 1 || statefulSet.OwnerReferences[0].UID != instance.UID {
		t.Fatalf("неверный ownerReference: %v", statefulSet.OwnerReferences)
	}
}

func Test_DesiredStatefulSet_WithRequestOverrides_PreservesConfiguredLimits(t *testing.T) {
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.VCPU = 4
	instance.Status.AcceptedConfiguration.RAMGB = 16
	requests := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	}

	statefulSet := desiredStatefulSet(
		instance,
		"cache-a1b2c3-config-digest",
		"valkey/valkey:8.1.9",
		requests,
	)
	resources := statefulSet.Spec.Template.Spec.Containers[0].Resources
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
	statefulSet := desiredStatefulSet(instance, configMap.Name, "valkey/valkey:8.1.9", nil)
	services := desiredServices(instance)
	networkPolicy := desiredNetworkPolicy(instance, "valkey-system", nil)
	pdb := desiredPodDisruptionBudget(instance)
	endpoint := networkEndpoints(instance)[0]
	route := desiredTCPRoute(instance, "valkey-system", endpoint)
	securityPolicy := desiredSecurityPolicy(instance, endpoint)
	objects := []metav1.Object{
		statefulSet,
		&statefulSet.Spec.Template.ObjectMeta,
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
	if !maps.Equal(statefulSet.Spec.Selector.MatchLabels, selectorLabels) ||
		!maps.Equal(pdb.Spec.Selector.MatchLabels, selectorLabels) ||
		!maps.Equal(networkPolicy.Spec.PodSelector.MatchLabels, selectorLabels) {
		t.Fatalf(
			"описательные метки попали в селектор: statefulSet=%v pdb=%v networkPolicy=%v",
			statefulSet.Spec.Selector.MatchLabels,
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

func Test_ReconcileStatefulSet_WhenValkeyImageChanges_PreservesExistingTemplate(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	scheme := NewScheme()
	reconciler := &ValkeyInstanceReconciler{Scheme: scheme}
	configMap, err := reconciler.desiredConfigMap(instance)
	if err != nil {
		t.Fatalf("сформировать ConfigMap: %v", err)
	}
	const originalImage = "valkey/valkey:8.1.9"
	statefulSet := desiredStatefulSet(instance, configMap.Name, originalImage, nil)
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, statefulSet).
		Build()
	reconciler.Client = k8s
	reconciler.SystemNamespace = "valkey-system"
	reconciler.ValkeyImage = "valkey/valkey:9.0.0"

	if _, err := reconciler.reconcileWorkloadResources(ctx, instance); err != nil {
		t.Fatalf("reconcile со сменившимся образом: %v", err)
	}
	observedStatefulSet := &appsv1.StatefulSet{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(statefulSet), observedStatefulSet); err != nil {
		t.Fatalf("прочитать StatefulSet: %v", err)
	}
	if got := observedStatefulSet.Spec.Template.Spec.Containers[0].Image; got != originalImage {
		t.Fatalf("образ StatefulSet изменён на %q", got)
	}
	observedInstance := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observedInstance); err != nil {
		t.Fatalf("прочитать ValkeyInstance: %v", err)
	}
	condition := apimeta.FindStatusCondition(observedInstance.Status.Conditions, conditionTypeRecoveryRequired)
	if condition == nil || condition.Reason != "ValkeyImageChanged" {
		t.Fatalf("нет RecoveryRequired для смены образа: %+v", condition)
	}

	reconciler.ValkeyImage = originalImage
	if _, err := reconciler.reconcileWorkloadResources(ctx, observedInstance); err != nil {
		t.Fatalf("reconcile после возврата образа: %v", err)
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

func Test_EnsureStatefulSet_WithServerDefaultsAndUnknownMetadata_DoesNotPatchRepeatedly(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	desired := desiredStatefulSet(instance, "cache-a1b2c3-config-digest", "valkey/valkey:8.1.9", nil)
	current := desired.DeepCopy()
	current.Spec.RevisionHistoryLimit = ptr.To[int32](10)
	current.Labels["example.com/unknown"] = "label"
	current.Annotations["example.com/unknown"] = "annotation"
	scheme := NewScheme()
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(current).Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, Scheme: scheme}

	changed, err := reconciler.ensureStatefulSet(ctx, desired)
	if err != nil || changed {
		t.Fatalf("серверное значение StatefulSet вызвало повторный PATCH: changed=%t error=%v", changed, err)
	}
}

func Test_DesiredStatefulSetAndDisruptionBudget_WithHAMode_CreateThreeAntiAffinePodsAndMinimumAvailability(
	t *testing.T,
) {
	instance := completeAcceptedInstance()
	instance.Spec.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	statefulSet := desiredStatefulSet(instance, "cache-a1b2c3-config-digest", "valkey/valkey:8.1.9", nil)

	container := statefulSet.Spec.Template.Spec.Containers[0]
	if statefulSet.Spec.Replicas == nil || *statefulSet.Spec.Replicas != 3 ||
		statefulSet.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType ||
		statefulSet.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyAlways ||
		container.RestartPolicy == nil || *container.RestartPolicy != corev1.ContainerRestartPolicyNever {
		t.Fatalf("неверная политика HA StatefulSet: %+v", statefulSet.Spec)
	}
	affinity := statefulSet.Spec.Template.Spec.Affinity
	if affinity == nil || affinity.PodAntiAffinity == nil ||
		len(affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatalf("HA не требует разнесения процессов: %+v", affinity)
	}
	term := affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
	if term.TopologyKey != corev1.LabelHostname || term.LabelSelector == nil ||
		!maps.Equal(term.LabelSelector.MatchLabels, workloadLabels(instance.Name)) {
		t.Fatalf("неверное правило anti-affinity: %+v", term)
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
			AcceptedConfiguration: &valkeyv1alpha1.AcceptedConfiguration{
				InstanceID: "01991ad0-1234-7000-8000-000000000001",
				Slug:       "cache-a1b2c3", Mode: valkeyv1alpha1.ValkeyModeSingle, VCPU: 2, RAMGB: 4,
			},
		},
	}
}

func selectorMatchesLabels(selector, labels map[string]string) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}

	return true
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
