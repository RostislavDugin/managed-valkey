//go:build envtest

package operator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func TestEnvtestCT04AcceptedGenerationWaitsAndRevalidatesAfterConflict(t *testing.T) {
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
	hash1 := strings.Repeat("ab", 32)
	hash2 := strings.Repeat("cd", 32)
	hash3 := strings.Repeat("ef", 32)

	instance := &valkeyv1alpha1.ValkeyInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "cache-a1b2c3", Namespace: "default"},
		Spec: valkeyv1alpha1.ValkeyInstanceSpec{
			InstanceID:        "01991ad0-1234-7000-8000-000000000001",
			Slug:              "cache-a1b2c3",
			Mode:              valkeyv1alpha1.ValkeyModeSingle,
			VCPU:              1,
			RAMGB:             4,
			PublicPort:        41379,
			Whitelist:         &valkeyv1alpha1.WhitelistSpec{},
			PasswordVersion:   1,
			DesiredGeneration: 1,
		},
	}
	if err := k8s.Create(ctx, instance); err != nil {
		t.Fatalf("создать CR: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: valkeyv1alpha1.AuthSecretName(instance.Name), Namespace: "default"},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			valkeyv1alpha1.AppPasswordHashKeyPrefix + "1": []byte(hash1),
		},
	}
	if err := k8s.Create(ctx, secret); err != nil {
		t.Fatalf("создать Secret: %v", err)
	}
	instance.Status = valkeyv1alpha1.ValkeyInstanceStatus{
		Initialized: true,
		Phase:       valkeyv1alpha1.InstancePhaseRunning,
		AcceptedConfiguration: &valkeyv1alpha1.AcceptedConfiguration{
			InstanceID: instance.Spec.InstanceID, Slug: instance.Spec.Slug, Mode: instance.Spec.Mode,
			VCPU: instance.Spec.VCPU, RAMGB: instance.Spec.RAMGB, PublicPort: instance.Spec.PublicPort,
			Whitelist: valkeyv1alpha1.WhitelistSpec{}, PasswordVersion: 1, DesiredGeneration: 1,
		},
		ObservedGeneration:     1,
		AppliedPasswordVersion: 1,
		Applied: &valkeyv1alpha1.AppliedConfiguration{
			Mode: valkeyv1alpha1.ValkeyModeSingle, VCPU: 1, RAMGB: 4,
		},
	}
	if err := k8s.Status().Update(ctx, instance); err != nil {
		t.Fatalf("сохранить исходный status: %v", err)
	}

	instance.Spec.PasswordVersion = 2
	instance.Spec.DesiredGeneration = 2
	if err := k8s.Update(ctx, instance); err != nil {
		t.Fatalf("запросить второе поколение: %v", err)
	}

	conflicting := &statusConflictClient{Client: k8s}
	conflicting.onConflict = func() error {
		currentSecret := &corev1.Secret{}
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(secret), currentSecret); err != nil {
			return err
		}
		currentSecret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"2"] = []byte(hash2)
		if err := k8s.Update(ctx, currentSecret); err != nil {
			return err
		}

		current := &valkeyv1alpha1.ValkeyInstance{}
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), current); err != nil {
			return err
		}
		current.Spec.RAMGB = 8
		current.Spec.DesiredGeneration = 3
		if err := k8s.Update(ctx, current); err != nil {
			return err
		}
		current.Status.PrimaryPodUID = "concurrent-pod"
		return k8s.Status().Update(ctx, current)
	}

	requested := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), requested); err != nil {
		t.Fatalf("прочитать запрос: %v", err)
	}
	reconciler := &ValkeyInstanceReconciler{Client: conflicting, APIReader: k8s}
	changed, err := reconciler.reconcileAcceptedConfiguration(ctx, requested)
	if err != nil || !changed {
		t.Fatalf("принять поколение после конфликта: changed=%t error=%v", changed, err)
	}

	observed := readAcceptedConfigurationInstance(t, ctx, k8s, instance)
	if observed.Status.AcceptedConfiguration.DesiredGeneration != 3 ||
		observed.Status.AcceptedConfiguration.RAMGB != 8 ||
		observed.Status.AcceptedConfiguration.PasswordVersion != 2 ||
		observed.Status.PrimaryPodUID != "concurrent-pod" ||
		observed.Status.Rollout == nil || observed.Status.Rollout.DesiredGeneration != 3 ||
		observed.Status.Rollout.Stage != valkeyv1alpha1.RolloutStagePreparing ||
		observed.Status.CredentialRotation == nil ||
		observed.Status.CredentialRotation.TargetVersion != 2 ||
		observed.Status.Applied == nil || observed.Status.Applied.RAMGB != 4 ||
		observed.Status.AppliedPasswordVersion != 1 || observed.Status.ObservedGeneration != 1 {
		t.Fatalf("после конфликта принят устаревший снимок: %+v", observed.Status)
	}
	if apimeta.FindStatusCondition(observed.Status.Conditions, conditionTypeConfigurationPending) != nil {
		t.Fatal("ожидание хеша осталось после его конкурентной доставки")
	}

	observed.Status.ObservedGeneration = 3
	observed.Status.Phase = valkeyv1alpha1.InstancePhaseRunning
	observed.Status.Rollout = nil
	observed.Status.CredentialRotation = nil
	observed.Status.Applied = &valkeyv1alpha1.AppliedConfiguration{
		Mode: valkeyv1alpha1.ValkeyModeSingle, VCPU: 1, RAMGB: 8,
	}
	observed.Status.AppliedPasswordVersion = 2
	if err := k8s.Status().Update(ctx, observed); err != nil {
		t.Fatalf("завершить третье поколение: %v", err)
	}
	observed.Spec.PasswordVersion = 3
	observed.Spec.DesiredGeneration = 4
	if err := k8s.Update(ctx, observed); err != nil {
		t.Fatalf("запросить четвёртое поколение: %v", err)
	}
	changed, err = (&ValkeyInstanceReconciler{Client: k8s, APIReader: k8s}).
		reconcileAcceptedConfiguration(ctx, observed)
	if err != nil || !changed {
		t.Fatalf("сохранить ожидание хеша: changed=%t error=%v", changed, err)
	}
	observed = readAcceptedConfigurationInstance(t, ctx, k8s, instance)
	assertPendingGeneration(t, observed, 3, "AppPasswordHashMissing")

	currentSecret := &corev1.Secret{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(secret), currentSecret); err != nil {
		t.Fatalf("прочитать Secret: %v", err)
	}
	currentSecret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"3"] = []byte(strings.ToUpper(hash3))
	if err := k8s.Update(ctx, currentSecret); err != nil {
		t.Fatalf("доставить повреждённый хеш: %v", err)
	}
	if _, err := (&ValkeyInstanceReconciler{Client: k8s, APIReader: k8s}).
		reconcileAcceptedConfiguration(ctx, observed); err != nil {
		t.Fatalf("обработать повреждённый хеш: %v", err)
	}
	observed = readAcceptedConfigurationInstance(t, ctx, k8s, instance)
	assertPendingGeneration(t, observed, 3, "AppPasswordHashInvalid")

	if err := k8s.Get(ctx, client.ObjectKeyFromObject(secret), currentSecret); err != nil {
		t.Fatalf("повторно прочитать Secret: %v", err)
	}
	currentSecret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"3"] = []byte(hash3)
	if err := k8s.Update(ctx, currentSecret); err != nil {
		t.Fatalf("исправить хеш: %v", err)
	}
	if _, err := (&ValkeyInstanceReconciler{Client: k8s, APIReader: k8s}).
		reconcileAcceptedConfiguration(ctx, observed); err != nil {
		t.Fatalf("принять поколение с исправным хешем: %v", err)
	}
	observed = readAcceptedConfigurationInstance(t, ctx, k8s, instance)
	if observed.Status.AcceptedConfiguration.DesiredGeneration != 4 ||
		observed.Status.AcceptedConfiguration.PasswordVersion != 3 {
		t.Fatalf("четвёртое поколение не принято: %+v", observed.Status.AcceptedConfiguration)
	}

	observed.Spec.RAMGB = 16
	observed.Spec.DesiredGeneration = 5
	if err := k8s.Update(ctx, observed); err != nil {
		t.Fatalf("запросить поколение во время операции: %v", err)
	}
	if _, err := (&ValkeyInstanceReconciler{Client: k8s, APIReader: k8s}).
		reconcileAcceptedConfiguration(ctx, observed); err != nil {
		t.Fatalf("сохранить раннее поколение: %v", err)
	}
	observed = readAcceptedConfigurationInstance(t, ctx, k8s, instance)
	assertPendingGeneration(t, observed, 4, "CurrentGenerationIncomplete")
}

func readAcceptedConfigurationInstance(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	instance *valkeyv1alpha1.ValkeyInstance,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()

	observed := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observed); err != nil {
		t.Fatalf("прочитать CR: %v", err)
	}
	if observed.Status.AcceptedConfiguration == nil {
		t.Fatal("принятая конфигурация исчезла")
	}

	return observed
}

func assertPendingGeneration(
	t *testing.T,
	instance *valkeyv1alpha1.ValkeyInstance,
	wantAccepted int64,
	wantReason string,
) {
	t.Helper()

	condition := apimeta.FindStatusCondition(instance.Status.Conditions, conditionTypeConfigurationPending)
	if instance.Status.AcceptedConfiguration.DesiredGeneration != wantAccepted ||
		condition == nil || condition.Reason != wantReason {
		t.Fatalf("неверное ожидание поколения: accepted=%+v condition=%+v",
			instance.Status.AcceptedConfiguration, condition)
	}
}

type statusConflictClient struct {
	client.Client
	once       sync.Once
	onConflict func() error
}

func (c *statusConflictClient) Status() client.SubResourceWriter {
	return &statusConflictWriter{
		SubResourceWriter: c.Client.Status(),
		once:              &c.once,
		onConflict:        c.onConflict,
	}
}

type statusConflictWriter struct {
	client.SubResourceWriter
	once       *sync.Once
	onConflict func() error
}

func (w *statusConflictWriter) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.SubResourcePatchOption,
) error {
	conflict := false
	w.once.Do(func() { conflict = true })
	if conflict {
		if err := w.onConflict(); err != nil {
			return err
		}
		return apierrors.NewConflict(
			schema.GroupResource{Group: valkeyv1alpha1.GroupVersion.Group, Resource: "valkeyinstances"},
			object.GetName(),
			errors.New("конкурентное обновление status"),
		)
	}

	return w.SubResourceWriter.Patch(ctx, object, patch, options...)
}
