//go:build envtest

package operator

import (
	"context"
	"maps"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/event"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func Test_Envtest_ReconcileOwnerMetadata_WithExistingWorkload_UpdatesWithoutReplacingProcesses(t *testing.T) {
	restConfig := startOwnerMetadataEnvironment(t)
	k8s, err := client.New(restConfig, client.Options{Scheme: NewScheme()})
	if err != nil {
		t.Fatalf("создать клиент envtest: %v", err)
	}
	ctx := context.Background()
	legacy := completeAcceptedInstance()
	legacy.Namespace = "default"
	legacy.Labels = nil
	legacy.Annotations = nil
	legacy.Status.AcceptedConfiguration.InstanceID = ""
	legacyStatefulSet := desiredStatefulSet(legacy, "cache-a1b2c3-config", "valkey/valkey:8.1.9")
	if err := k8s.Create(ctx, legacyStatefulSet); err != nil {
		t.Fatalf("создать прежний StatefulSet: %v", err)
	}
	legacyService := desiredServices(legacy)[1]
	if err := k8s.Create(ctx, legacyService); err != nil {
		t.Fatalf("создать прежний Service: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: legacy.Name + "-0", Namespace: legacy.Namespace,
			Labels: workloadLabels(legacy.Name),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "valkey", Image: "valkey/valkey:8.1.9"}}},
	}
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatalf("создать работающий Pod: %v", err)
	}
	podUID := pod.UID
	serviceIP := legacyService.Spec.ClusterIP

	instance := completeAcceptedInstance()
	instance.Namespace = "default"
	desiredStatefulSet := desiredStatefulSet(instance, "cache-a1b2c3-config", "valkey/valkey:8.1.9")
	reconciler := &ValkeyInstanceReconciler{Client: k8s, Scheme: NewScheme()}
	if changed, err := reconciler.ensureStatefulSet(ctx, desiredStatefulSet); err != nil || !changed {
		t.Fatalf("дополнить StatefulSet: changed=%t error=%v", changed, err)
	}
	desiredService := desiredServices(instance)[1]
	if changed, err := reconciler.ensureService(ctx, desiredService); err != nil || !changed {
		t.Fatalf("дополнить Service: changed=%t error=%v", changed, err)
	}

	currentStatefulSet := &appsv1.StatefulSet{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(legacyStatefulSet), currentStatefulSet); err != nil {
		t.Fatalf("перечитать StatefulSet: %v", err)
	}
	currentPod := &corev1.Pod{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), currentPod); err != nil {
		t.Fatalf("перечитать Pod: %v", err)
	}
	currentService := &corev1.Service{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(legacyService), currentService); err != nil {
		t.Fatalf("перечитать Service: %v", err)
	}
	if !maps.Equal(currentStatefulSet.Spec.Selector.MatchLabels, workloadLabels(instance.Name)) ||
		currentPod.UID != podUID || currentService.Spec.ClusterIP != serviceIP ||
		!maps.Equal(currentService.Spec.Selector, legacyService.Spec.Selector) {
		t.Fatalf("описательные метаданные изменили процессы или маршрут: selector=%v podUID=%s service=%+v",
			currentStatefulSet.Spec.Selector.MatchLabels, currentPod.UID, currentService.Spec)
	}
	assertOwnerMetadata(t, currentStatefulSet, instance)
	assertOwnerMetadata(t, &currentStatefulSet.Spec.Template.ObjectMeta, instance)
	assertOwnerMetadata(t, currentService, instance)
}

func Test_Envtest_ReconcilePodMetadata_WhenOwnerEmailAppears_UpdatesPodAndLeavesNodeUntouched(t *testing.T) {
	restConfig := startOwnerMetadataEnvironment(t)
	k8s, err := client.New(restConfig, client.Options{Scheme: NewScheme()})
	if err != nil {
		t.Fatalf("создать клиент envtest: %v", err)
	}
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "owner-metadata-node", Labels: map[string]string{"other.example.com/value": "kept"},
	}}
	if err := k8s.Create(ctx, node); err != nil {
		t.Fatalf("создать Node: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cache-a1b2c3-0", Namespace: "default", Labels: workloadLabels("cache-a1b2c3"),
		},
		Spec: corev1.PodSpec{
			NodeName:   node.Name,
			Containers: []corev1.Container{{Name: "valkey", Image: "valkey/valkey:8.1.9"}},
		},
	}
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatalf("создать Pod: %v", err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "valkey", ContainerID: "containerd://owner-metadata", Ready: true,
	}}
	if err := k8s.Status().Update(ctx, pod); err != nil {
		t.Fatalf("подготовить status Pod: %v", err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
		t.Fatalf("перечитать Node: %v", err)
	}
	nodeVersion := node.ResourceVersion
	nodeUID := node.UID

	instance := completeAcceptedInstance()
	instance.Namespace = "default"
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{{
		Ordinal: 0, PodUID: string(pod.UID), ContainerID: "containerd://owner-metadata",
		Role: valkeyv1alpha1.NodeRolePrimary,
	}}
	primaryOrdinal := int32(0)
	instance.Status.PrimaryOrdinal = &primaryOrdinal
	instance.Status.PrimaryPodUID = string(pod.UID)
	instance.Status.PrimaryContainerID = "containerd://owner-metadata"
	before := instance.DeepCopy()
	delete(before.Annotations, valkeyv1alpha1.UserEmailAnnotationKey)
	if !valkeyInstancePredicate().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: instance}) {
		t.Fatal("появление email-аннотации не поставило ValkeyInstance в очередь")
	}
	reconciler := &ValkeyInstanceReconciler{Client: k8s}
	if result, err := reconciler.reconcilePodMetadata(ctx, instance); err != nil || result.IsZero() {
		t.Fatalf("дополнить работающий Pod: result=%+v error=%v", result, err)
	}
	currentPod := &corev1.Pod{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(pod), currentPod); err != nil {
		t.Fatalf("перечитать Pod: %v", err)
	}
	if currentPod.UID != pod.UID ||
		currentPod.Annotations[valkeyv1alpha1.UserEmailAnnotationKey] !=
			instance.Annotations[valkeyv1alpha1.UserEmailAnnotationKey] {
		t.Fatalf("Pod заменён или не получил email: uid=%s annotations=%v", currentPod.UID, currentPod.Annotations)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
		t.Fatalf("перечитать Node после сверки: %v", err)
	}
	if node.UID != types.UID(nodeUID) || node.ResourceVersion != nodeVersion ||
		node.Labels["other.example.com/value"] != "kept" {
		t.Fatalf("сверка Pod изменила Node: %+v", node.ObjectMeta)
	}
}

func startOwnerMetadataEnvironment(t *testing.T) *rest.Config {
	t.Helper()
	environment := &envtest.Environment{}
	restConfig, err := environment.Start()
	if err != nil {
		t.Fatalf("запустить envtest метаданных владельца: %v", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("остановить envtest метаданных владельца: %v", err)
		}
	})

	return restConfig
}
