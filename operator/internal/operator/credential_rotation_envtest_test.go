//go:build envtest

package operator

import (
	"context"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func Test_Envtest_CT08_CleanPreviousPasswordHash_WhenPatchConflicts_RetriesAndPreservesConcurrentData(t *testing.T) {
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
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pwcleanup-auth", Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			valkeyv1alpha1.AppPasswordHashKeyPrefix + "1": []byte(strings.Repeat("ab", 32)),
			valkeyv1alpha1.AppPasswordHashKeyPrefix + "2": []byte(strings.Repeat("cd", 32)),
			valkeyv1alpha1.OperatorPasswordKey:            []byte("operator-password"),
			valkeyv1alpha1.ReplicaPasswordKey:             []byte("replica-password"),
			valkeyv1alpha1.HealthPasswordKey:              []byte("health-password"),
			valkeyv1alpha1.UsersACLKey:                    []byte("users-acl"),
			"external-field":                              []byte("original"),
		},
	}
	if err := k8s.Create(ctx, secret); err != nil {
		t.Fatalf("создать Secret: %v", err)
	}
	instance := &valkeyv1alpha1.ValkeyInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "pwcleanup", Namespace: "default"},
		Status: valkeyv1alpha1.ValkeyInstanceStatus{
			AcceptedConfiguration:  &valkeyv1alpha1.AcceptedConfiguration{Slug: "pwcleanup", PasswordVersion: 2},
			AppliedPasswordVersion: 1,
			CredentialRotation: &valkeyv1alpha1.CredentialRotationStatus{
				TargetVersion: 2, PreviousVersion: 1,
				Stage: valkeyv1alpha1.CredentialRotationStageCleaningSecret,
			},
		},
	}
	conflicting := &secretPatchConflictClient{Client: k8s}
	conflicting.onFirstPatch = func() error {
		current := &corev1.Secret{}
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(secret), current); err != nil {
			return err
		}
		current.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"3"] = []byte(strings.Repeat("ef", 32))
		current.Data["external-field"] = []byte("concurrent")
		return k8s.Update(ctx, current)
	}
	reconciler := &ValkeyInstanceReconciler{Client: conflicting, APIReader: k8s}

	result, err := reconciler.finishCredentialRotation(ctx, instance)
	if err != nil || result.IsZero() {
		t.Fatalf("очистить хеш после конфликта: result=%+v error=%v", result, err)
	}
	if conflicting.patchAttempts < 2 {
		t.Fatalf("очистка не повторила конфликтный Patch: %d", conflicting.patchAttempts)
	}
	observed := &corev1.Secret{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(secret), observed); err != nil {
		t.Fatalf("прочитать Secret после очистки: %v", err)
	}
	if _, found := observed.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"1"]; found {
		t.Fatal("предыдущий хеш сохранился после очистки")
	}
	for key, expected := range map[string]string{
		valkeyv1alpha1.AppPasswordHashKeyPrefix + "2": strings.Repeat("cd", 32),
		valkeyv1alpha1.AppPasswordHashKeyPrefix + "3": strings.Repeat("ef", 32),
		valkeyv1alpha1.OperatorPasswordKey:            "operator-password",
		valkeyv1alpha1.ReplicaPasswordKey:             "replica-password",
		valkeyv1alpha1.HealthPasswordKey:              "health-password",
		valkeyv1alpha1.UsersACLKey:                    "users-acl",
		"external-field":                              "concurrent",
	} {
		if string(observed.Data[key]) != expected {
			t.Fatalf("очистка повредила поле %s", key)
		}
	}
	if instance.Status.CredentialRotation == nil || instance.Status.AppliedPasswordVersion != 1 {
		t.Fatalf("очистка преждевременно подтвердила status: %+v", instance.Status)
	}
}

type secretPatchConflictClient struct {
	client.Client
	once          sync.Once
	onFirstPatch  func() error
	patchAttempts int
}

func (c *secretPatchConflictClient) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.PatchOption,
) error {
	if _, ok := object.(*corev1.Secret); ok {
		c.patchAttempts++
		var conflictErr error
		c.once.Do(func() { conflictErr = c.onFirstPatch() })
		if conflictErr != nil {
			return conflictErr
		}
	}
	return c.Client.Patch(ctx, object, patch, options...)
}
