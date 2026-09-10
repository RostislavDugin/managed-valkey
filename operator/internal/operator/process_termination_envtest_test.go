//go:build envtest

package operator

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func TestEnvtestCT05NodeDeletionRequiresFreshUIDEvidence(t *testing.T) {
	environment := &envtest.Environment{}
	restConfig, err := environment.Start()
	if err != nil {
		t.Fatalf("запустить envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("остановить envtest: %v", err)
		}
	})

	k8s, err := client.New(restConfig, client.Options{Scheme: NewScheme()})
	if err != nil {
		t.Fatalf("создать клиент: %v", err)
	}
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "worker-ct05", Finalizers: []string{"test.valkey/finalizer"},
	}}
	if err := k8s.Create(ctx, node); err != nil {
		t.Fatalf("создать Node: %v", err)
	}
	process := valkeyv1alpha1.NodeStatus{
		PodUID: "pod-ct05", ContainerID: "containerd://ct05", RunID: "run-ct05",
		NodeName: node.Name, NodeUID: string(node.UID),
	}
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "valkey-ct05", Namespace: "default",
	}, Spec: corev1.PodSpec{
		NodeName:   node.Name,
		Containers: []corev1.Container{{Name: "valkey", Image: "valkey/valkey:8.1.9"}},
	}}
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatalf("создать Pod: %v", err)
	}
	if err := k8s.Delete(ctx, pod); err != nil {
		t.Fatalf("удалить только Pod: %v", err)
	}
	assertNodeNotDeleted(t, ctx, reconciler, process, "удаление только Pod")

	node.Status.Conditions = []corev1.NodeCondition{{
		Type: corev1.NodeReady, Status: corev1.ConditionFalse, Reason: "CT05",
	}}
	if err := k8s.Status().Update(ctx, node); err != nil {
		t.Fatalf("сохранить NotReady: %v", err)
	}
	assertNodeNotDeleted(t, ctx, reconciler, process, "NotReady старого UID")

	if err := k8s.Delete(ctx, node); err != nil {
		t.Fatalf("запросить удаление Node: %v", err)
	}
	deleting := &corev1.Node{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(node), deleting); err != nil {
		t.Fatalf("прочитать удаляемый Node: %v", err)
	}
	if deleting.DeletionTimestamp.IsZero() {
		t.Fatal("Node не получил deletionTimestamp")
	}
	assertNodeNotDeleted(t, ctx, reconciler, process, "deletionTimestamp старого UID")

	before := deleting.DeepCopy()
	deleting.Finalizers = nil
	if err := k8s.Patch(ctx, deleting, client.MergeFrom(before)); err != nil {
		t.Fatalf("снять тестовый finalizer Node: %v", err)
	}
	deleted, err := reconciler.previousNodeDeleted(ctx, process)
	if err != nil || !deleted {
		t.Fatalf("NotFound старого UID не стал доказательством: deleted=%t error=%v", deleted, err)
	}

	replacement := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name}}
	if err := k8s.Create(ctx, replacement); err != nil {
		t.Fatalf("создать Node с прежним именем: %v", err)
	}
	if string(replacement.UID) == process.NodeUID {
		t.Fatalf("Kubernetes повторно выдал UID %s", replacement.UID)
	}
	deleted, err = reconciler.previousNodeDeleted(ctx, process)
	if err != nil || !deleted {
		t.Fatalf("новый UID не доказал исчезновение старого: deleted=%t error=%v", deleted, err)
	}

	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "Forbidden", err: apierrors.NewForbidden(
			schema.GroupResource{Resource: "nodes"}, node.Name, errors.New("доступ запрещён"),
		)},
		{name: "timeout", err: apierrors.NewTimeoutError("таймаут чтения Node", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &ct05NodeErrorReader{Reader: k8s, err: test.err}
			failed := &ValkeyInstanceReconciler{Client: k8s, APIReader: reader}
			deleted, err := failed.previousNodeDeleted(ctx, process)
			if err == nil || deleted {
				t.Fatalf("ошибка Node принята как доказательство: deleted=%t error=%v", deleted, err)
			}
		})
	}
}

type ct05NodeErrorReader struct {
	client.Reader
	err error
}

func (r *ct05NodeErrorReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if _, ok := object.(*corev1.Node); ok {
		return r.err
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func assertNodeNotDeleted(
	t *testing.T,
	ctx context.Context,
	reconciler *ValkeyInstanceReconciler,
	process valkeyv1alpha1.NodeStatus,
	scenario string,
) {
	t.Helper()
	deleted, err := reconciler.previousNodeDeleted(ctx, process)
	if err != nil || deleted {
		t.Fatalf("%s принято как node_deleted: deleted=%t error=%v", scenario, deleted, err)
	}
}
