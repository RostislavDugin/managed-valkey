//go:build envtest

package operator_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	"github.com/RostislavDugin/managed-valkey/operator/internal/operator"
)

func TestManagerStartsAndStopsOnContext(t *testing.T) {
	restConfig := startEnvironment(t)

	probeAddr := freeAddress(t)

	mgr, err := operator.NewManager(restConfig, config.Config{
		SystemNamespace: "default",
	}, discardLogger(),
		operator.WithSkipControllerNameValidation(),
		operator.WithProbeAddr(probeAddr),
		operator.WithLeaderElection(false),
	)
	if err != nil {
		t.Fatalf("создать manager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	stopped := make(chan error, 1)
	go func() { stopped <- operator.Run(ctx, mgr) }()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache manager не синхронизировался")
	}

	for _, path := range []string{"/healthz", "/readyz"} {
		waitForOK(t, "http://"+probeAddr+path)
	}

	cancel()

	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("остановка manager: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("manager не остановился по отмене контекста")
	}
}

func TestReconcileLeavesResourceUntouched(t *testing.T) {
	restConfig := startEnvironment(t)

	mgr, err := operator.NewManager(restConfig, config.Config{
		SystemNamespace: "default",
	}, discardLogger(),
		operator.WithSkipControllerNameValidation(),
		operator.WithProbeAddr(freeAddress(t)),
		operator.WithLeaderElection(false),
	)
	if err != nil {
		t.Fatalf("создать manager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = operator.Run(ctx, mgr) }()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache manager не синхронизировался")
	}

	k8s, err := client.New(restConfig, client.Options{Scheme: operator.NewScheme()})
	if err != nil {
		t.Fatalf("создать клиент: %v", err)
	}

	instance := &valkeyv1alpha1.ValkeyInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-a1b2c3", Namespace: "default"},
		Spec: valkeyv1alpha1.ValkeyInstanceSpec{
			InstanceID: "01991ad0-1234-7000-8000-000000000001",
			Slug:       "shop-a1b2c3",
		},
	}

	if err := k8s.Create(ctx, instance); err != nil {
		t.Fatalf("создать ValkeyInstance: %v", err)
	}

	created := instance.DeepCopy()

	// Пауза даёт контроллеру обработать событие: без неё проверка прошла бы и
	// для оператора, который просто не успел получить ресурс.
	time.Sleep(3 * time.Second)

	observed := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observed); err != nil {
		t.Fatalf("прочитать ValkeyInstance: %v", err)
	}

	if observed.ResourceVersion != created.ResourceVersion {
		t.Errorf("resourceVersion изменился с %s на %s", created.ResourceVersion, observed.ResourceVersion)
	}

	if len(observed.Finalizers) != 0 {
		t.Errorf("оператор добавил finalizers %v", observed.Finalizers)
	}

	if observed.Status.ObservedGeneration != 0 || len(observed.Status.Conditions) != 0 {
		t.Errorf("оператор записал status %+v", observed.Status)
	}

	assertNoDependents(ctx, t, k8s)
}

func assertNoDependents(ctx context.Context, t *testing.T, k8s client.Client) {
	t.Helper()

	statefulSets := &appsv1.StatefulSetList{}
	if err := k8s.List(ctx, statefulSets, client.InNamespace("default")); err != nil {
		t.Fatalf("прочитать StatefulSet: %v", err)
	}

	if len(statefulSets.Items) != 0 {
		t.Errorf("создано StatefulSet: %d", len(statefulSets.Items))
	}

	secrets := &corev1.SecretList{}
	if err := k8s.List(ctx, secrets, client.InNamespace("default")); err != nil {
		t.Fatalf("прочитать Secret: %v", err)
	}

	for _, secret := range secrets.Items {
		if secret.Type != corev1.SecretTypeServiceAccountToken {
			t.Errorf("создан Secret %s", secret.Name)
		}
	}

	services := &corev1.ServiceList{}
	if err := k8s.List(ctx, services, client.InNamespace("default")); err != nil {
		t.Fatalf("прочитать Service: %v", err)
	}

	for _, service := range services.Items {
		if service.Name != "kubernetes" {
			t.Errorf("создан Service %s", service.Name)
		}
	}
}

func startEnvironment(t *testing.T) *rest.Config {
	t.Helper()

	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}

	restConfig, err := environment.Start()
	if err != nil {
		t.Fatalf("запустить envtest: %v", err)
	}

	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("остановить envtest: %v", err)
		}
	})

	return restConfig
}

func waitForOK(t *testing.T, url string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		response, err := http.Get(url)
		if err == nil {
			_ = response.Body.Close()

			if response.StatusCode == http.StatusOK {
				return
			}
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("проверка %s не ответила успешно", url)
}

func freeAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("занять порт: %v", err)
	}

	address := listener.Addr().String()

	if err := listener.Close(); err != nil {
		t.Fatalf("освободить порт: %v", err)
	}

	return address
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
