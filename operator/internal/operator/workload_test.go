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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func TestSingleStatefulSetContract(t *testing.T) {
	instance := completeAcceptedInstance()
	statefulSet := desiredStatefulSet(instance, "cache-a1b2c3-config-digest", "valkey/valkey:8.1.9")

	if statefulSet.Spec.Replicas == nil || *statefulSet.Spec.Replicas != 1 ||
		statefulSet.Spec.PodManagementPolicy != appsv1.ParallelPodManagement ||
		statefulSet.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType {
		t.Fatalf("неверная стратегия StatefulSet: %+v", statefulSet.Spec)
	}
	if len(statefulSet.Spec.VolumeClaimTemplates) != 0 {
		t.Fatal("StatefulSet создаёт постоянное хранилище")
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

func TestSingleServicesAndNetworkPolicy(t *testing.T) {
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

func TestValkeyImageChangePreservesStatefulSetTemplate(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	scheme := NewScheme()
	reconciler := &ValkeyInstanceReconciler{Scheme: scheme}
	configMap, err := reconciler.desiredConfigMap(instance)
	if err != nil {
		t.Fatalf("сформировать ConfigMap: %v", err)
	}
	const originalImage = "valkey/valkey:8.1.9"
	statefulSet := desiredStatefulSet(instance, configMap.Name, originalImage)
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, statefulSet).
		Build()
	reconciler.Client = k8s
	reconciler.SystemNamespace = "valkey-system"
	reconciler.ValkeyImage = "valkey/valkey:9.0.0"

	if _, err := reconciler.reconcileSingleResources(ctx, instance); err != nil {
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
	if _, err := reconciler.reconcileSingleResources(ctx, observedInstance); err != nil {
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

func completeAcceptedInstance() *valkeyv1alpha1.ValkeyInstance {
	return &valkeyv1alpha1.ValkeyInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cache-a1b2c3", Namespace: "valkey-cache-a1b2c3", UID: "instance-uid",
		},
		Status: valkeyv1alpha1.ValkeyInstanceStatus{
			AcceptedConfiguration: &valkeyv1alpha1.AcceptedConfiguration{
				Slug: "cache-a1b2c3", Mode: valkeyv1alpha1.ValkeyModeSingle, VCPU: 2, RAMGB: 4,
			},
		},
	}
}
