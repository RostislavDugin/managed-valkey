//go:build envtest

package operator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func TestCredentialsUpdateRetriesWithoutLosingDeliveredHash(t *testing.T) {
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

	k8s, err := client.New(restConfig, client.Options{Scheme: NewScheme()})
	if err != nil {
		t.Fatalf("создать клиент: %v", err)
	}
	ctx := context.Background()

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

	firstHash := strings.Repeat("ab", 32)
	secondHash := strings.Repeat("cd", 32)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-a1b2c3-auth", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"app-password-hash.1": []byte(firstHash),
			"api-field":           []byte("preserved"),
		},
	}
	if err := k8s.Create(ctx, secret); err != nil {
		t.Fatalf("создать Secret: %v", err)
	}

	stale := &corev1.Secret{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(secret), stale); err != nil {
		t.Fatalf("прочитать Secret для оператора: %v", err)
	}
	credentials, err := newServiceCredentials(firstHash)
	if err != nil {
		t.Fatalf("сформировать служебные данные: %v", err)
	}
	if _, err := updateCredentialsSecret(instance, stale, credentials, NewScheme()); err != nil {
		t.Fatalf("подготовить обновление оператора: %v", err)
	}

	delivered := &corev1.Secret{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(secret), delivered); err != nil {
		t.Fatalf("прочитать Secret для API: %v", err)
	}
	delivered.Data["app-password-hash.2"] = []byte(secondHash)
	if err := k8s.Update(ctx, delivered); err != nil {
		t.Fatalf("доставить новый хеш: %v", err)
	}

	if err := k8s.Update(ctx, stale); !apierrors.IsConflict(err) {
		t.Fatalf("устаревшее обновление: получено %v, ожидался Conflict", err)
	}

	fresh := &corev1.Secret{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(secret), fresh); err != nil {
		t.Fatalf("повторно прочитать Secret: %v", err)
	}
	if _, err := updateCredentialsSecret(instance, fresh, credentials, NewScheme()); err != nil {
		t.Fatalf("повторно подготовить обновление: %v", err)
	}
	if err := k8s.Update(ctx, fresh); err != nil {
		t.Fatalf("повторить обновление: %v", err)
	}

	observed := &corev1.Secret{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(secret), observed); err != nil {
		t.Fatalf("прочитать итоговый Secret: %v", err)
	}
	if string(observed.Data["app-password-hash.2"]) != secondHash ||
		string(observed.Data["api-field"]) != "preserved" {
		t.Fatalf("потеряны чужие поля Secret: ключи=%v", secretKeys(observed.Data))
	}
	acl := string(observed.Data[valkeyv1alpha1.UsersACLKey])
	if !strings.Contains(acl, "#"+firstHash) || strings.Contains(acl, "#"+secondHash) {
		t.Fatal("ACL не сохранил принятую версию хеша")
	}
}

func secretKeys(data map[string][]byte) []string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}

	return keys
}
