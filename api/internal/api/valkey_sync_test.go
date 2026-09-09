package api_test

import (
	"context"
	"encoding/base64"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeysync "github.com/RostislavDugin/managed-valkey/api/internal/sync"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

const syncWaitTimeout = 20 * time.Second

type interceptUpdateClient struct {
	client.Client
	once      stdsync.Once
	intercept func(context.Context, client.Object, ...client.UpdateOption) error
}

type interceptCreateClient struct {
	client.Client
	once      stdsync.Once
	intercept func(context.Context, client.Object, ...client.CreateOption) error
}

func (c *interceptCreateClient) Create(
	ctx context.Context,
	object client.Object,
	options ...client.CreateOption,
) error {
	if _, ok := object.(*valkeyv1alpha1.ValkeyInstance); ok {
		intercepted := false
		var err error
		c.once.Do(func() {
			intercepted = true
			err = c.intercept(ctx, object, options...)
		})
		if intercepted {
			return err
		}
	}

	return c.Client.Create(ctx, object, options...)
}

type blockingUpdateClient struct {
	client.Client
	once    stdsync.Once
	entered chan struct{}
	release chan struct{}
}

func (c *blockingUpdateClient) Update(
	ctx context.Context,
	object client.Object,
	options ...client.UpdateOption,
) error {
	if _, ok := object.(*valkeyv1alpha1.ValkeyInstance); ok {
		c.once.Do(func() {
			close(c.entered)
			select {
			case <-ctx.Done():
			case <-c.release:
			}
		})
	}

	return c.Client.Update(ctx, object, options...)
}

type blockingImportRepository struct {
	*store.Store
	once    stdsync.Once
	entered chan struct{}
	release chan struct{}
}

func (r *blockingImportRepository) ImportValkeyObservation(
	ctx context.Context,
	instanceID uuid.UUID,
	observation store.ValkeyObservation,
) error {
	r.once.Do(func() {
		close(r.entered)
		select {
		case <-ctx.Done():
		case <-r.release:
		}
	})

	return r.Store.ImportValkeyObservation(ctx, instanceID, observation)
}

type transportFailureClient struct {
	client.Client
	err error
}

func (c transportFailureClient) Get(
	context.Context,
	client.ObjectKey,
	client.Object,
	...client.GetOption,
) error {
	return c.err
}

func (c transportFailureClient) List(
	context.Context,
	client.ObjectList,
	...client.ListOption,
) error {
	return c.err
}

type failingFindRepository struct {
	*store.Store
	instanceID uuid.UUID
	err        error
}

func (r failingFindRepository) FindValkeyInstanceForSync(
	ctx context.Context,
	instanceID uuid.UUID,
) (store.ValkeyInstance, error) {
	if instanceID == r.instanceID {
		return store.ValkeyInstance{}, r.err
	}

	return r.Store.FindValkeyInstanceForSync(ctx, instanceID)
}

func (c *interceptUpdateClient) Update(
	ctx context.Context,
	object client.Object,
	options ...client.UpdateOption,
) error {
	if _, ok := object.(*valkeyv1alpha1.ValkeyInstance); ok {
		intercepted := false
		var err error
		c.once.Do(func() {
			intercepted = true
			err = c.intercept(ctx, object, options...)
		})
		if intercepted {
			return err
		}
	}

	return c.Client.Update(ctx, object, options...)
}

func TestValkeyStateSyncThroughK3S(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	setUserQuota(t, app, account.ID, 32, 128)
	app.startSync(t)

	created := createValkey(t, app, account, map[string]any{
		"name": "synchronized-cache", "prefix": "state", "vcpu": 1, "ram_gb": 1,
	})
	namespaceName := "valkey-" + created.Slug
	t.Cleanup(func() {
		app.stopSync()
		cleanupSyncNamespace(t, app, namespaceName)
	})

	resource := waitForValkeyInstance(t, app, created.Slug, func(resource *valkeyv1alpha1.ValkeyInstance) bool {
		return resource.Spec.DesiredGeneration == 1 && resource.Spec.PasswordVersion == 1
	})
	assertDeliveredIdentity(t, app, account.ID, created, resource)
	unchangedVersion := resource.ResourceVersion
	if err := app.syncService.RunDelivery(context.Background()); err != nil {
		t.Fatalf("повторить доставку совпадающего состояния: %v", err)
	}
	resource = waitForValkeyInstance(t, app, created.Slug, func(resource *valkeyv1alpha1.ValkeyInstance) bool {
		return resource.ResourceVersion == unchangedVersion
	})
	assertApplicationRBAC(t, app, resource)
	confirmValkeyStatus(t, app, resource)
	waitForHTTPInstance(t, app, account, created.ID, func(instance valkeydomain.Instance) bool {
		return instance.Status == "running" && instance.ObservedGeneration == 1 &&
			instance.AppliedPasswordVersion == 1 && instance.AppliedVCPU == 1 &&
			instance.NetworkVerificationStatus == "verified" && !instance.IsStale
	})
	assertImportedRows(t, app, created.ID, 1, 1)
	auditCount := databaseCount(t, app.database.DB().Model(&store.AuditLog{}).Where("user_id = ?", account.ID))
	billingCount := databaseCount(
		t,
		app.database.DB().Model(&store.BillingPeriod{}).Where("resource_id = ?", created.ID),
	)
	if err := app.syncService.RunImport(context.Background()); err != nil {
		t.Fatalf("повторить импорт совпадающего status: %v", err)
	}
	if databaseCount(t, app.database.DB().Model(&store.AuditLog{}).Where("user_id = ?", account.ID)) != auditCount ||
		databaseCount(
			t,
			app.database.DB().Model(&store.BillingPeriod{}).Where("resource_id = ?", created.ID),
		) != billingCount {
		t.Fatal("повторный импорт создал аудит или биллинговый период")
	}

	resize := resizeValkey(t, app, account, created.ID, 2, 2)
	assertStatus(t, resize, http.StatusAccepted)
	resource = waitForValkeyInstance(t, app, created.Slug, func(resource *valkeyv1alpha1.ValkeyInstance) bool {
		return resource.Spec.DesiredGeneration == 2 && resource.Spec.VCPU == 2 && resource.Spec.RAMGB == 2
	})
	setValkeyCRPhase(t, app, resource, valkeyv1alpha1.InstancePhaseUpdating)
	waitForHTTPInstance(t, app, account, created.ID, func(instance valkeydomain.Instance) bool {
		return instance.Status == "updating" && instance.ObservedGeneration == 1
	})
	confirmValkeyStatus(t, app, resource)
	waitForHTTPInstance(t, app, account, created.ID, func(instance valkeydomain.Instance) bool {
		return instance.ObservedGeneration == 2 && instance.AppliedVCPU == 2 && instance.AppliedRAMGB == 2
	})

	resize = resizeValkey(t, app, account, created.ID, 1, 1)
	assertStatus(t, resize, http.StatusAccepted)
	waitForQuotaUsage(t, app, account, 2, 2)
	resource = waitForValkeyInstance(t, app, created.Slug, func(resource *valkeyv1alpha1.ValkeyInstance) bool {
		return resource.Spec.DesiredGeneration == 3 && resource.Spec.VCPU == 1 && resource.Spec.RAMGB == 1
	})
	confirmValkeyStatus(t, app, resource)
	waitForQuotaUsage(t, app, account, 1, 1)

	whitelist := app.requestJSON(
		t,
		http.MethodPut,
		instancePath(created.ID)+"/whitelist",
		map[string]any{"is_whitelist_enabled": true, "whitelist_cidrs": []string{"192.0.2.0/24"}},
		bearer(account.Token),
	)
	assertStatus(t, whitelist, http.StatusAccepted)
	resource = waitForValkeyInstance(t, app, created.Slug, func(resource *valkeyv1alpha1.ValkeyInstance) bool {
		return resource.Spec.DesiredGeneration == 4 && resource.Spec.Whitelist != nil &&
			resource.Spec.Whitelist.IsEnabled && slices.Equal(resource.Spec.Whitelist.CIDRs, []string{"192.0.2.0/24"})
	})
	confirmValkeyStatus(t, app, resource)
	waitForHTTPInstance(t, app, account, created.ID, func(instance valkeydomain.Instance) bool {
		return instance.ObservedGeneration == 4
	})

	rotated := app.requestJSON(
		t,
		http.MethodPost,
		instancePath(created.ID)+"/credentials/rotate",
		map[string]any{"password": rotatedValkeyPassword, "expected_password_version": 1},
		mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": uuid.NewString()}),
	)
	assertStatus(t, rotated, http.StatusAccepted)
	resource = waitForValkeyInstance(t, app, created.Slug, func(resource *valkeyv1alpha1.ValkeyInstance) bool {
		return resource.Spec.DesiredGeneration == 5 && resource.Spec.PasswordVersion == 2
	})
	assertSecretVersionPresent(t, app, namespaceName, created.Slug, 2)
	confirmValkeyStatus(t, app, resource)
	waitForHTTPInstance(t, app, account, created.ID, func(instance valkeydomain.Instance) bool {
		return instance.ObservedGeneration == 5 && instance.AppliedPasswordVersion == 2
	})
	removeOldPasswordHash(t, app, namespaceName, created.Slug)
	if err := app.syncService.RunDelivery(context.Background()); err != nil {
		t.Fatalf("сверить Secret после очистки старого хеша: %v", err)
	}
	assertOldPasswordHashAbsent(t, app, namespaceName, created.Slug)

	assertStatus(
		t,
		app.requestJSON(t, http.MethodDelete, instancePath(created.ID), nil, bearer(account.Token)),
		http.StatusAccepted,
	)
	resource = waitForValkeyInstance(t, app, created.Slug, func(resource *valkeyv1alpha1.ValkeyInstance) bool {
		return resource.DeletionTimestamp != nil
	})
	assertNamespaceAndSecretExist(t, app, namespaceName, created.Slug)
	removeInstanceFinalizer(t, app, resource)
	waitForKubernetesDeletion(t, app, namespaceName, created.Slug)
	waitForHTTPNotFound(t, app, account, created.ID)
	assertImportedRows(t, app, created.ID, 0, 4)
}

func TestValkeySyncStorePreservesIntentAndRollsBackObservation(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	setUserQuota(t, app, account.ID, 32, 128)
	created := createValkey(t, app, account, map[string]any{"name": "transactional-sync"})
	now := time.Now().UTC().Truncate(time.Microsecond)
	setValkeyState(t, app, created.ID, "running", 1, now)
	legacyReason := "legacy_recovery_required"
	if err := app.database.DB().Model(&store.ValkeyInstance{}).Where("id = ?", created.ID).
		UpdateColumns(map[string]any{
			"is_recovery_required": true,
			"sync_recovery_reason": legacyReason,
		}).Error; err != nil {
		t.Fatalf("подготовить признак восстановления: %v", err)
	}

	assertStatus(t, resizeValkey(t, app, account, created.ID, 2, 2), http.StatusAccepted)
	intent := loadValkey(t, app, created.ID)
	node := store.ValkeyNodeObservation{
		Ordinal: 0, Role: "primary", PodName: created.Slug + "-0", PodUID: "pod-a",
		ContainerID: "container-a", RunID: "run-a", NodeName: "node-a", NodeUID: "node-uid-a",
		IsReady: true, ObservedAt: now.Add(time.Second),
	}
	broken := store.ValkeyObservation{
		Phase: "degraded", ObservedGeneration: 1, ObservedAt: now.Add(time.Second),
		AppliedPasswordVersion: 1, HasAppliedConfiguration: true, AppliedMode: "single",
		AppliedVCPU: 1, AppliedRAMGB: 1, Nodes: []store.ValkeyNodeObservation{node, node},
	}
	if err := app.database.ImportValkeyObservation(context.Background(), created.ID, broken); err == nil {
		t.Fatal("наблюдение с повторяющимся ordinal записано")
	}
	afterFailure := loadValkey(t, app, created.ID)
	if afterFailure.Phase != "running" || afterFailure.ObservedAt == nil || !afterFailure.ObservedAt.Equal(now) ||
		afterFailure.VCPU != 2 || afterFailure.DesiredGeneration != 2 ||
		!afterFailure.UpdatedAt.Equal(intent.UpdatedAt) {
		t.Fatalf("неудачная транзакция изменила строку: %+v", afterFailure)
	}
	assertImportedRows(t, app, created.ID, 0, 0)

	valid := broken
	valid.Nodes = []store.ValkeyNodeObservation{node}
	if err := app.database.ImportValkeyObservation(context.Background(), created.ID, valid); err != nil {
		t.Fatalf("импортировать допустимое наблюдение: %v", err)
	}
	afterImport := loadValkey(t, app, created.ID)
	if afterImport.VCPU != 2 || afterImport.RAMGB != 2 || afterImport.DesiredGeneration != 2 ||
		!afterImport.UpdatedAt.Equal(intent.UpdatedAt) || !afterImport.IsRecoveryRequired ||
		afterImport.SyncRecoveryReason == nil || *afterImport.SyncRecoveryReason != legacyReason {
		t.Fatalf("импорт перезаписал намерение или причину восстановления: %+v", afterImport)
	}
	assertImportedRows(t, app, created.ID, 1, 1)

	stale := valid
	stale.Phase = "error"
	stale.ObservedGeneration = 0
	stale.ObservedAt = now.Add(-time.Second)
	if err := app.database.ImportValkeyObservation(
		context.Background(),
		created.ID,
		stale,
	); !errors.Is(
		err,
		store.ErrValkeyObservationStale,
	) {
		t.Fatalf("устаревшее наблюдение вернуло ошибку %v", err)
	}
	afterStale := loadValkey(t, app, created.ID)
	if afterStale.Phase != "degraded" || afterStale.ObservedGeneration != 1 {
		t.Fatalf("устаревшее наблюдение изменило строку: %+v", afterStale)
	}

	future := valid
	future.ObservedGeneration = 3
	if err := app.database.ImportValkeyObservation(
		context.Background(),
		created.ID,
		future,
	); !errors.Is(
		err,
		store.ErrValkeyObservationInvalid,
	) {
		t.Fatalf("будущее наблюдение вернуло ошибку %v", err)
	}

	sameTime := valid
	sameTime.Nodes = nil
	sameTime.HasNetwork = true
	sameTime.NetworkVerificationStatus = "verified"
	sameTime.NetworkVerifiedAt = nil
	if err := app.database.ImportValkeyObservation(context.Background(), created.ID, sameTime); err != nil {
		t.Fatalf("импортировать изменение с тем же observedAt: %v", err)
	}
	afterSameTime := loadValkey(t, app, created.ID)
	if afterSameTime.NetworkVerificationStatus != "verified" || afterSameTime.NetworkVerifiedAt != nil {
		t.Fatalf("сеть с тем же observedAt сохранена неверно: %+v", afterSameTime)
	}
	assertImportedRows(t, app, created.ID, 0, 1)

	falseReadiness := valid
	falseReadiness.ObservedAt = now.Add(2 * time.Second)
	falseReadiness.Nodes[0].IsReady = false
	falseReadiness.Nodes[0].ObservedAt = falseReadiness.ObservedAt
	if err := app.database.ImportValkeyObservation(context.Background(), created.ID, falseReadiness); err != nil {
		t.Fatalf("импортировать readiness=false: %v", err)
	}
	var savedNode store.ValkeyInstanceNode
	if err := app.database.DB().Where("instance_id = ?", created.ID).First(&savedNode).Error; err != nil {
		t.Fatalf("прочитать ноду: %v", err)
	}
	if savedNode.IsReady {
		t.Fatal("readiness=false потерян при импорте")
	}

	if err := app.database.BindValkeyNamespaceUID(context.Background(), created.ID, "namespace-a"); err != nil {
		t.Fatalf("сохранить UID Namespace: %v", err)
	}
	if err := app.database.BindValkeyNamespaceUID(context.Background(), created.ID, "namespace-a"); err != nil {
		t.Fatalf("повторно сохранить UID Namespace: %v", err)
	}
	if err := app.database.BindValkeyNamespaceUID(
		context.Background(),
		created.ID,
		"namespace-b",
	); !errors.Is(
		err,
		store.ErrKubernetesIdentityConflict,
	) {
		t.Fatalf("замена UID вернула ошибку %v", err)
	}
	afterIdentity := loadValkey(t, app, created.ID)
	if afterIdentity.KubernetesNamespaceUID == nil || *afterIdentity.KubernetesNamespaceUID != "namespace-a" ||
		!afterIdentity.UpdatedAt.Equal(intent.UpdatedAt) {
		t.Fatalf("привязка UID изменила намерение: %+v", afterIdentity)
	}
}

func TestValkeySyncRequiresRecoveryForLostObjects(t *testing.T) {
	t.Run("потерянный Namespace", func(t *testing.T) {
		app := newHTTPTestAPI(t, testAPIConfig{})
		app.startSync(t)
		app.stopSync()
		account := app.registerAccount(t, "")
		created := createValkey(t, app, account, map[string]any{"name": "lost-namespace", "prefix": "lostnamespace"})
		if err := app.database.BindValkeyNamespaceUID(
			context.Background(),
			created.ID,
			"missing-namespace-uid",
		); err != nil {
			t.Fatalf("подготовить UID отсутствующего Namespace: %v", err)
		}

		if err := app.syncService.RunDelivery(context.Background()); err != nil {
			t.Fatalf("выполнить сверку после потери Namespace: %v", err)
		}
		waitForSyncRecoveryReason(t, app, created.ID, true)
		if err := app.adminKubernetes.Get(
			context.Background(),
			client.ObjectKey{Name: "valkey-" + created.Slug},
			&corev1.Namespace{},
		); !apierrors.IsNotFound(err) {
			t.Fatalf("потерянный Namespace был создан заново: %v", err)
		}
	})

	t.Run("потерянный Secret", func(t *testing.T) {
		app := newHTTPTestAPI(t, testAPIConfig{})
		account := app.registerAccount(t, "")
		app.startSync(t)
		created := createValkey(t, app, account, map[string]any{"name": "lost-secret", "prefix": "lostsecret"})
		namespaceName := "valkey-" + created.Slug
		t.Cleanup(func() {
			app.stopSync()
			cleanupSyncNamespace(t, app, namespaceName)
		})

		resource := waitForValkeyInstance(t, app, created.Slug, func(*valkeyv1alpha1.ValkeyInstance) bool {
			return true
		})
		confirmValkeyStatus(t, app, resource)
		waitForHTTPInstance(t, app, account, created.ID, func(instance valkeydomain.Instance) bool {
			return instance.Status == "running" && !instance.IsRecoveryRequired
		})
		setOperatorRecovery(t, app, resource, true)
		waitForHTTPInstance(t, app, account, created.ID, func(instance valkeydomain.Instance) bool {
			return instance.IsRecoveryRequired
		})

		secret := &corev1.Secret{}
		secretKey := client.ObjectKey{Namespace: namespaceName, Name: valkeyv1alpha1.AuthSecretName(created.Slug)}
		if err := app.adminKubernetes.Get(context.Background(), secretKey, secret); err != nil {
			t.Fatalf("прочитать удаляемый Secret: %v", err)
		}
		data := cloneSecretData(secret.Data)
		labels := maps.Clone(secret.Labels)
		secretType := secret.Type
		if err := app.adminKubernetes.Delete(context.Background(), secret); err != nil {
			t.Fatalf("удалить Secret: %v", err)
		}
		waitForSyncRecoveryReason(t, app, created.ID, true)
		if err := app.adminKubernetes.Get(
			context.Background(),
			secretKey,
			&corev1.Secret{},
		); !apierrors.IsNotFound(
			err,
		) {
			t.Fatalf("потерянный Secret был создан заново: %v", err)
		}

		restored := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespaceName, Name: secretKey.Name, Labels: labels},
			Type:       secretType,
			Data:       data,
		}
		if err := app.adminKubernetes.Create(context.Background(), restored); err != nil {
			t.Fatalf("восстановить Secret: %v", err)
		}
		waitForSyncRecoveryReason(t, app, created.ID, false)
		if instance := waitForHTTPInstance(t, app, account, created.ID, func(instance valkeydomain.Instance) bool {
			return instance.IsRecoveryRequired
		}); !instance.IsRecoveryRequired {
			t.Fatal("причина оператора снята вместе с причиной доставки")
		}
		removedCredential := removeServiceCredential(t, app, namespaceName, created.Slug)
		waitForSyncRecoveryReason(t, app, created.ID, true)
		assertServiceCredentialAbsent(t, app, namespaceName, created.Slug)
		restoreServiceCredential(t, app, namespaceName, created.Slug, removedCredential)
		waitForSyncRecoveryReason(t, app, created.ID, false)

		setOperatorRecovery(t, app, resource, false)
		waitForHTTPInstance(t, app, account, created.ID, func(instance valkeydomain.Instance) bool {
			return !instance.IsRecoveryRequired
		})
	})

	t.Run("потерянный ValkeyInstance", func(t *testing.T) {
		app := newHTTPTestAPI(t, testAPIConfig{})
		account := app.registerAccount(t, "")
		app.startSync(t)
		created := createValkey(t, app, account, map[string]any{"name": "lost-resource", "prefix": "lostcr"})
		namespaceName := "valkey-" + created.Slug
		t.Cleanup(func() {
			app.stopSync()
			cleanupSyncNamespace(t, app, namespaceName)
		})

		resource := waitForValkeyInstance(t, app, created.Slug, func(*valkeyv1alpha1.ValkeyInstance) bool {
			return true
		})
		confirmValkeyStatus(t, app, resource)
		waitForHTTPInstance(t, app, account, created.ID, func(instance valkeydomain.Instance) bool {
			return instance.ObservedGeneration == 1
		})
		app.stopSync()

		resource = &valkeyv1alpha1.ValkeyInstance{}
		key := client.ObjectKey{Namespace: namespaceName, Name: created.Slug}
		if err := app.adminKubernetes.Get(context.Background(), key, resource); err != nil {
			t.Fatalf("перечитать удаляемый ValkeyInstance: %v", err)
		}
		resource.Finalizers = nil
		if err := app.adminKubernetes.Update(context.Background(), resource); err != nil {
			t.Fatalf("снять защитную отметку потерянного ValkeyInstance: %v", err)
		}
		if err := app.adminKubernetes.Delete(context.Background(), resource); err != nil {
			t.Fatalf("удалить ValkeyInstance вне протокола: %v", err)
		}
		if err := app.syncService.RunDelivery(context.Background()); err != nil {
			t.Fatalf("выполнить сверку после потери ValkeyInstance: %v", err)
		}
		waitForSyncRecoveryReason(t, app, created.ID, true)
		if err := app.adminKubernetes.Get(
			context.Background(), client.ObjectKey{Name: namespaceName}, &corev1.Namespace{},
		); err != nil {
			t.Fatalf("Namespace удалён после потери ValkeyInstance: %v", err)
		}
	})
}

func TestValkeySyncLeavesForeignAndOrphanNamespacesUntouched(t *testing.T) {
	generator := &sequenceSlugGenerator{values: []string{"aaaaaa", "bbbbbb"}}
	app := newHTTPTestAPI(t, testAPIConfig{slugGenerator: generator})
	account := app.registerAccount(t, "")
	app.startSync(t)
	foreignName := "valkey-foreign-aaaaaa"
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: foreignName,
		Labels: map[string]string{
			valkeyv1alpha1.ManagedByLabelKey: valkeyv1alpha1.ManagedByLabelValue,
			valkeyv1alpha1.InstanceLabelKey:  "someone-else",
		},
	}}
	if err := app.adminKubernetes.Create(context.Background(), foreign); err != nil {
		t.Fatalf("создать чужой Namespace: %v", err)
	}
	t.Cleanup(func() {
		app.stopSync()
		cleanupSyncNamespace(t, app, foreignName)
	})
	created := createValkey(t, app, account, map[string]any{
		"name": "foreign-namespace", "prefix": "foreign",
	})
	healthy := createValkey(t, app, account, map[string]any{
		"name": "healthy-namespace", "prefix": "healthy",
	})
	waitForSyncRecoveryReason(t, app, created.ID, true)
	healthyResource := waitForValkeyInstance(t, app, healthy.Slug, func(*valkeyv1alpha1.ValkeyInstance) bool {
		return true
	})
	t.Cleanup(func() { cleanupSyncNamespace(t, app, healthyResource.Namespace) })
	observed := &corev1.Namespace{}
	if err := app.adminKubernetes.Get(context.Background(), client.ObjectKey{Name: foreignName}, observed); err != nil {
		t.Fatalf("чужой Namespace удалён: %v", err)
	}
	if observed.Labels[valkeyv1alpha1.InstanceLabelKey] != "someone-else" {
		t.Fatalf("метки чужого Namespace изменены: %v", observed.Labels)
	}

	app.stopSync()
	orphanID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("создать UUIDv7: %v", err)
	}
	orphanName := "valkey-orphan-aaaaaa"
	orphan := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: orphanName,
		Labels: map[string]string{
			valkeyv1alpha1.ManagedByLabelKey:  valkeyv1alpha1.ManagedByLabelValue,
			valkeyv1alpha1.InstanceLabelKey:   "orphan-aaaaaa",
			valkeyv1alpha1.InstanceIDLabelKey: orphanID.String(),
			valkeyv1alpha1.UserIDLabelKey:     account.ID.String(),
		},
	}}
	if err := app.adminKubernetes.Create(context.Background(), orphan); err != nil {
		t.Fatalf("создать Namespace без строки БД: %v", err)
	}
	t.Cleanup(func() { cleanupSyncNamespace(t, app, orphanName) })
	databaseFailure := errors.New("чтение PostgreSQL прервано тестом")
	failingService := valkeysync.NewService(
		failingFindRepository{Store: app.database, instanceID: orphanID, err: databaseFailure},
		app.kubernetes,
		app.logger,
	)
	if err := failingService.RunDelivery(context.Background()); !errors.Is(err, databaseFailure) {
		t.Fatalf("ошибка PostgreSQL принята за отсутствие строки: %v", err)
	}
	if err := app.adminKubernetes.Get(
		context.Background(), client.ObjectKey{Name: orphanName}, &corev1.Namespace{},
	); err != nil {
		t.Fatalf("Namespace удалён после ошибки PostgreSQL: %v", err)
	}
	if err := app.syncService.RunDelivery(context.Background()); err != nil {
		t.Fatalf("сверить Namespace без строки БД: %v", err)
	}
	if !strings.Contains(app.logs.String(), "Namespace не имеет строки PostgreSQL") {
		t.Fatal("Namespace без строки БД не отражён в журнале")
	}
	if err := app.adminKubernetes.Get(
		context.Background(), client.ObjectKey{Name: orphanName}, &corev1.Namespace{},
	); err != nil {
		t.Fatalf("Namespace без строки БД удалён: %v", err)
	}
}

func TestValkeySyncContinuesPasswordDeliveryAndRetriesConflict(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	app.startSync(t)
	created := createValkey(t, app, account, map[string]any{"name": "delivery-retries", "prefix": "retry"})
	namespaceName := "valkey-" + created.Slug
	t.Cleanup(func() {
		app.stopSync()
		cleanupSyncNamespace(t, app, namespaceName)
	})

	resource := waitForValkeyInstance(t, app, created.Slug, func(*valkeyv1alpha1.ValkeyInstance) bool {
		return true
	})
	confirmValkeyStatus(t, app, resource)
	waitForHTTPInstance(t, app, account, created.ID, func(instance valkeydomain.Instance) bool {
		return instance.ObservedGeneration == 1
	})
	app.stopSync()

	rotated := app.requestJSON(
		t,
		http.MethodPost,
		instancePath(created.ID)+"/credentials/rotate",
		map[string]any{"password": rotatedValkeyPassword, "expected_password_version": 1},
		mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": uuid.NewString()}),
	)
	assertStatus(t, rotated, http.StatusAccepted)
	failingClient := &interceptUpdateClient{
		Client: app.kubernetes,
		intercept: func(context.Context, client.Object, ...client.UpdateOption) error {
			return errors.New("запись ValkeyInstance прервана тестом")
		},
	}
	failingService := valkeysync.NewService(app.database, failingClient, app.logger)
	if err := failingService.RunDelivery(context.Background()); err != nil {
		t.Fatalf("выполнить прерванную доставку: %v", err)
	}
	assertSecretVersionPresent(t, app, namespaceName, created.Slug, 2)
	resource = getValkeyInstance(t, app, namespaceName, created.Slug)
	if resource.Spec.PasswordVersion != 1 {
		t.Fatalf("версия CR изменилась после прерванной доставки: %d", resource.Spec.PasswordVersion)
	}
	if instance := loadValkey(t, app, created.ID); instance.AppliedPasswordVersion != 1 {
		t.Fatalf("доставка подтвердила пароль без status: %+v", instance)
	}

	if err := app.syncService.RunDelivery(context.Background()); err != nil {
		t.Fatalf("продолжить доставку пароля: %v", err)
	}
	resource = getValkeyInstance(t, app, namespaceName, created.Slug)
	if resource.Spec.PasswordVersion != 2 || resource.Spec.DesiredGeneration != 2 {
		t.Fatalf("доставка пароля не продолжилась: %+v", resource.Spec)
	}
	confirmValkeyStatus(t, app, resource)
	if err := app.syncService.RunImport(context.Background()); err != nil {
		t.Fatalf("импортировать подтверждение пароля: %v", err)
	}
	waitForHTTPInstance(t, app, account, created.ID, func(instance valkeydomain.Instance) bool {
		return instance.ObservedGeneration == 2 && instance.AppliedPasswordVersion == 2
	})
	tamperPasswordHash(t, app, namespaceName, created.Slug, 2)
	if err := app.syncService.RunDelivery(context.Background()); err != nil {
		t.Fatalf("сверить повреждённый хеш пароля: %v", err)
	}
	waitForSyncRecoveryReason(t, app, created.ID, true)
	assertPasswordHashRemainsTampered(t, app, namespaceName, created.Slug, 2)
	restoreCurrentPasswordHash(t, app, namespaceName, created.Slug, 2, loadValkey(t, app, created.ID).AppPasswordHash)
	if err := app.syncService.RunDelivery(context.Background()); err != nil {
		t.Fatalf("сверить восстановленный хеш пароля: %v", err)
	}
	waitForSyncRecoveryReason(t, app, created.ID, false)

	whitelist := app.requestJSON(
		t,
		http.MethodPut,
		instancePath(created.ID)+"/whitelist",
		map[string]any{"is_whitelist_enabled": false, "whitelist_cidrs": []string{"198.51.100.0/24"}},
		bearer(account.Token),
	)
	assertStatus(t, whitelist, http.StatusAccepted)
	conflictingClient := &interceptUpdateClient{
		Client: app.kubernetes,
		intercept: func(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
			current := &valkeyv1alpha1.ValkeyInstance{}
			if err := app.adminKubernetes.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
				return err
			}
			current.Annotations = maps.Clone(current.Annotations)
			if current.Annotations == nil {
				current.Annotations = make(map[string]string, 1)
			}
			current.Annotations["operator.example/revision"] = "preserved"
			if err := app.adminKubernetes.Update(ctx, current); err != nil {
				return err
			}

			return app.kubernetes.Update(ctx, object, options...)
		},
	}
	conflictingService := valkeysync.NewService(app.database, conflictingClient, app.logger)
	if err := conflictingService.RunDelivery(context.Background()); err != nil {
		t.Fatalf("повторить доставку после конфликта: %v", err)
	}
	resource = getValkeyInstance(t, app, namespaceName, created.Slug)
	if resource.Spec.DesiredGeneration != 3 ||
		resource.Annotations["operator.example/revision"] != "preserved" {
		t.Fatalf("конфликт Kubernetes обработан неверно: %+v", resource.ObjectMeta)
	}

	record := loadValkey(t, app, created.ID)
	if containsAny(app.logs.String(), testValkeyPassword, rotatedValkeyPassword, record.AppPasswordHash) {
		t.Fatal("журнал синхронизации содержит пароль или хеш")
	}
}

func TestValkeySyncCoordinatesWithConcurrentDelete(t *testing.T) {
	t.Run("доставка", func(t *testing.T) {
		app := newHTTPTestAPI(t, testAPIConfig{})
		account := app.registerAccount(t, "")
		app.startSync(t)
		created := createValkey(t, app, account, map[string]any{"name": "delete-during-delivery", "prefix": "delpush"})
		namespaceName := "valkey-" + created.Slug
		t.Cleanup(func() {
			app.stopSync()
			cleanupSyncNamespace(t, app, namespaceName)
		})

		resource := waitForValkeyInstance(t, app, created.Slug, func(*valkeyv1alpha1.ValkeyInstance) bool {
			return true
		})
		confirmValkeyStatus(t, app, resource)
		waitForHTTPInstance(t, app, account, created.ID, func(instance valkeydomain.Instance) bool {
			return instance.ObservedGeneration == 1
		})
		app.stopSync()
		assertStatus(t, resizeValkey(t, app, account, created.ID, 2, 2), http.StatusAccepted)

		blockingClient := &blockingUpdateClient{
			Client: app.kubernetes, entered: make(chan struct{}), release: make(chan struct{}),
		}
		service := valkeysync.NewService(app.database, blockingClient, app.logger)
		done := make(chan error, 1)
		go func() { done <- service.RunDelivery(context.Background()) }()
		waitSignal(t, blockingClient.entered)
		assertStatus(
			t,
			app.requestJSON(t, http.MethodDelete, instancePath(created.ID), nil, bearer(account.Token)),
			http.StatusAccepted,
		)
		close(blockingClient.release)
		if err := waitError(t, done); err != nil {
			t.Fatalf("завершить доставку одновременно с DELETE: %v", err)
		}

		resource = getValkeyInstance(t, app, namespaceName, created.Slug)
		if resource.Spec.DesiredGeneration != 2 ||
			!slices.Contains(resource.Finalizers, valkeyv1alpha1.InstanceFinalizer) {
			t.Fatalf("конкурентная доставка оставила незащищённый CR: %+v", resource.ObjectMeta)
		}
		completeDeletionWithoutOperator(t, app, created.Slug, created.ID)
	})

	t.Run("импорт", func(t *testing.T) {
		app := newHTTPTestAPI(t, testAPIConfig{})
		account := app.registerAccount(t, "")
		app.startSync(t)
		created := createValkey(t, app, account, map[string]any{"name": "delete-during-import", "prefix": "delpull"})
		namespaceName := "valkey-" + created.Slug
		t.Cleanup(func() {
			app.stopSync()
			cleanupSyncNamespace(t, app, namespaceName)
		})

		resource := waitForValkeyInstance(t, app, created.Slug, func(*valkeyv1alpha1.ValkeyInstance) bool {
			return true
		})
		confirmValkeyStatus(t, app, resource)
		waitForHTTPInstance(t, app, account, created.ID, func(instance valkeydomain.Instance) bool {
			return instance.ObservedGeneration == 1
		})
		app.stopSync()

		resource = getValkeyInstance(t, app, namespaceName, created.Slug)
		now := metav1.Now()
		resource.Status.Phase = valkeyv1alpha1.InstancePhaseDegraded
		resource.Status.Reason = "DelayedObservation"
		resource.Status.ObservedAt = &now
		if err := app.adminKubernetes.Status().Update(context.Background(), resource); err != nil {
			t.Fatalf("подготовить запоздалое наблюдение: %v", err)
		}

		repository := &blockingImportRepository{
			Store: app.database, entered: make(chan struct{}), release: make(chan struct{}),
		}
		service := valkeysync.NewService(repository, app.kubernetes, app.logger)
		done := make(chan error, 1)
		go func() { done <- service.RunImport(context.Background()) }()
		waitSignal(t, repository.entered)
		assertStatus(
			t,
			app.requestJSON(t, http.MethodDelete, instancePath(created.ID), nil, bearer(account.Token)),
			http.StatusAccepted,
		)
		close(repository.release)
		if err := waitError(t, done); err != nil {
			t.Fatalf("завершить импорт одновременно с DELETE: %v", err)
		}
		record := loadValkey(t, app, created.ID)
		if record.Phase != "running" || record.DeletionRequestedAt == nil {
			t.Fatalf("запоздалое наблюдение вернуло удаляемую запись в работу: %+v", record)
		}
	})
}

func TestValkeySyncTransportFailureDoesNotAffectHTTPReadiness(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	app.startSync(t)
	app.stopSync()
	created := createValkey(t, app, account, map[string]any{"name": "transport-failure", "prefix": "transport"})
	namespaceName := "valkey-" + created.Slug
	t.Cleanup(func() { cleanupSyncNamespace(t, app, namespaceName) })

	failure := errors.New("Kubernetes временно недоступен")
	service := valkeysync.NewService(
		app.database,
		transportFailureClient{Client: app.kubernetes, err: failure},
		app.logger,
	)
	if err := service.RunDelivery(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("отказ Kubernetes вернул ошибку %v", err)
	}
	assertStatus(t, app.requestJSON(t, http.MethodGet, "/readyz", nil, nil), http.StatusOK)
	if err := app.syncService.RunDelivery(context.Background()); err != nil {
		t.Fatalf("продолжить доставку после восстановления Kubernetes: %v", err)
	}
	waitForValkeyInstance(t, app, created.Slug, func(*valkeyv1alpha1.ValkeyInstance) bool { return true })
}

func TestValkeySyncRecoversCreateBoundaries(t *testing.T) {
	t.Run("потерянный ответ Create", func(t *testing.T) {
		app := newHTTPTestAPI(t, testAPIConfig{})
		account := app.registerAccount(t, "")
		app.startSync(t)
		app.stopSync()
		created := createValkey(t, app, account, map[string]any{"name": "lost-create-response", "prefix": "lostcreate"})
		namespaceName := "valkey-" + created.Slug
		t.Cleanup(func() { cleanupSyncNamespace(t, app, namespaceName) })

		lostResponseClient := &interceptCreateClient{
			Client: app.kubernetes,
			intercept: func(ctx context.Context, object client.Object, options ...client.CreateOption) error {
				if err := app.kubernetes.Create(ctx, object, options...); err != nil {
					return err
				}

				return errors.New("ответ Create потерян в тесте")
			},
		}
		service := valkeysync.NewService(app.database, lostResponseClient, app.logger)
		if err := service.RunDelivery(context.Background()); err != nil {
			t.Fatalf("выполнить доставку с потерянным ответом: %v", err)
		}
		resource := getValkeyInstance(t, app, namespaceName, created.Slug)
		if record := loadValkey(t, app, created.ID); record.KubernetesCRUID != nil {
			t.Fatalf("UID сохранён после потерянного ответа: %+v", record)
		}
		if err := app.syncService.RunDelivery(context.Background()); err != nil {
			t.Fatalf("повторить доставку после потерянного ответа: %v", err)
		}
		record := loadValkey(t, app, created.ID)
		if record.KubernetesCRUID == nil || *record.KubernetesCRUID != string(resource.UID) {
			t.Fatalf("повторная доставка не сохранила UID CR: %+v", record)
		}
	})

	t.Run("отмена до первого CR", func(t *testing.T) {
		app := newHTTPTestAPI(t, testAPIConfig{})
		account := app.registerAccount(t, "")
		app.startSync(t)
		app.stopSync()
		created := createValkey(t, app, account, map[string]any{"name": "canceled-create", "prefix": "cancel"})
		namespaceName := "valkey-" + created.Slug
		t.Cleanup(func() { cleanupSyncNamespace(t, app, namespaceName) })

		failedCreateClient := &interceptCreateClient{
			Client: app.kubernetes,
			intercept: func(context.Context, client.Object, ...client.CreateOption) error {
				return errors.New("создание CR прервано тестом")
			},
		}
		service := valkeysync.NewService(app.database, failedCreateClient, app.logger)
		if err := service.RunDelivery(context.Background()); err != nil {
			t.Fatalf("выполнить прерванное создание: %v", err)
		}
		if err := app.adminKubernetes.Get(
			context.Background(),
			client.ObjectKey{Namespace: namespaceName, Name: created.Slug},
			&valkeyv1alpha1.ValkeyInstance{},
		); !apierrors.IsNotFound(err) {
			t.Fatalf("CR появился после прерванного создания: %v", err)
		}
		assertNamespaceAndSecretExist(t, app, namespaceName, created.Slug)
		assertStatus(
			t,
			app.requestJSON(t, http.MethodDelete, instancePath(created.ID), nil, bearer(account.Token)),
			http.StatusAccepted,
		)
		completeDeletionWithoutOperator(t, app, created.Slug, created.ID)
	})
}

func assertDeliveredIdentity(
	t *testing.T,
	app *testAPI,
	userID uuid.UUID,
	instance valkeydomain.Instance,
	resource *valkeyv1alpha1.ValkeyInstance,
) {
	t.Helper()

	if resource.Namespace != "valkey-"+instance.Slug || resource.Spec.InstanceID != instance.ID.String() ||
		resource.Labels[valkeyv1alpha1.InstanceIDLabelKey] != instance.ID.String() ||
		!slices.Contains(resource.Finalizers, valkeyv1alpha1.InstanceFinalizer) {
		t.Fatalf("ValkeyInstance получил неверную идентичность: %+v", resource.ObjectMeta)
	}
	namespace := &corev1.Namespace{}
	if err := app.adminKubernetes.Get(
		context.Background(),
		client.ObjectKey{Name: resource.Namespace},
		namespace,
	); err != nil {
		t.Fatalf("прочитать Namespace: %v", err)
	}
	if namespace.Labels[valkeyv1alpha1.UserIDLabelKey] != userID.String() ||
		namespace.Labels[valkeyv1alpha1.InstanceIDLabelKey] != instance.ID.String() {
		t.Fatalf("Namespace получил неверные метки: %v", namespace.Labels)
	}
	assertSecretVersionPresent(t, app, resource.Namespace, instance.Slug, 1)
}

func assertApplicationRBAC(t *testing.T, app *testAPI, resource *valkeyv1alpha1.ValkeyInstance) {
	t.Helper()

	copy := resource.DeepCopy()
	copy.Status.Phase = valkeyv1alpha1.InstancePhaseRunning
	if err := app.kubernetes.Status().Update(context.Background(), copy); !apierrors.IsForbidden(err) {
		t.Fatalf("рабочий клиент изменил status: %v", err)
	}
	pods := &corev1.PodList{}
	if err := app.kubernetes.List(
		context.Background(),
		pods,
		client.InNamespace(resource.Namespace),
	); !apierrors.IsForbidden(
		err,
	) {
		t.Fatalf("рабочий клиент прочитал поды: %v", err)
	}
}

func confirmValkeyStatus(t *testing.T, app *testAPI, resource *valkeyv1alpha1.ValkeyInstance) {
	t.Helper()

	prepareServiceCredentials(t, app, resource.Namespace, resource.Spec.Slug)

	current := &valkeyv1alpha1.ValkeyInstance{}
	key := client.ObjectKeyFromObject(resource)
	if err := app.adminKubernetes.Get(context.Background(), key, current); err != nil {
		t.Fatalf("перечитать ValkeyInstance для status: %v", err)
	}
	now := metav1.Now()
	current.Status = valkeyv1alpha1.ValkeyInstanceStatus{
		CredentialsInitialized: true,
		Initialized:            true,
		Phase:                  valkeyv1alpha1.InstancePhaseRunning,
		ObservedAt:             &now,
		PrimaryOrdinal:         pointer(int32(0)),
		PrimaryPodUID:          "pod-uid",
		PrimaryContainerID:     "container-id",
		ObservedGeneration:     current.Spec.DesiredGeneration,
		AppliedPasswordVersion: current.Spec.PasswordVersion,
		AcceptedConfiguration: &valkeyv1alpha1.AcceptedConfiguration{
			InstanceID: current.Spec.InstanceID, Slug: current.Spec.Slug, Mode: current.Spec.Mode,
			VCPU: current.Spec.VCPU, RAMGB: current.Spec.RAMGB, PublicPort: current.Spec.PublicPort,
			Whitelist: *current.Spec.Whitelist, PasswordVersion: current.Spec.PasswordVersion,
			DesiredGeneration: current.Spec.DesiredGeneration,
		},
		Applied: &valkeyv1alpha1.AppliedConfiguration{
			Mode: current.Spec.Mode, VCPU: current.Spec.VCPU, RAMGB: current.Spec.RAMGB,
		},
		Network: &valkeyv1alpha1.NetworkStatus{
			VerificationStatus: valkeyv1alpha1.NetworkVerificationVerified,
			VerifiedAt:         &now,
		},
		Nodes: []valkeyv1alpha1.NodeStatus{{
			Ordinal: 0, PodUID: "pod-uid", ContainerID: "container-id", RunID: uuid.NewString(),
			NodeName: "k3s-server", NodeUID: "node-uid", Role: valkeyv1alpha1.NodeRolePrimary, Readiness: true,
		}},
		Conditions: []metav1.Condition{{
			Type: valkeyv1alpha1.ConditionTypeRecoveryRequired, Status: metav1.ConditionFalse,
			Reason: "Healthy", LastTransitionTime: now,
		}},
	}
	if err := app.adminKubernetes.Status().Update(context.Background(), current); err != nil {
		t.Fatalf("записать status ValkeyInstance: %v", err)
	}
}

func prepareServiceCredentials(t *testing.T, app *testAPI, namespace, slug string) {
	t.Helper()

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: valkeyv1alpha1.AuthSecretName(slug)}
	if err := app.adminKubernetes.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("прочитать Secret для подготовки status: %v", err)
	}
	if _, exists := secret.Data[valkeyv1alpha1.OperatorPasswordKey]; exists {
		return
	}
	password := []byte(base64.RawURLEncoding.EncodeToString([]byte("123456789012345678901234")))
	secret.Data[valkeyv1alpha1.OperatorPasswordKey] = slices.Clone(password)
	secret.Data[valkeyv1alpha1.ReplicaPasswordKey] = slices.Clone(password)
	secret.Data[valkeyv1alpha1.HealthPasswordKey] = slices.Clone(password)
	secret.Data[valkeyv1alpha1.UsersACLKey] = []byte("acl")
	if err := app.adminKubernetes.Update(context.Background(), secret); err != nil {
		t.Fatalf("подготовить служебные поля Secret: %v", err)
	}
}

func setOperatorRecovery(
	t *testing.T,
	app *testAPI,
	resource *valkeyv1alpha1.ValkeyInstance,
	required bool,
) {
	t.Helper()

	current := &valkeyv1alpha1.ValkeyInstance{}
	if err := app.adminKubernetes.Get(context.Background(), client.ObjectKeyFromObject(resource), current); err != nil {
		t.Fatalf("перечитать ValkeyInstance для условия восстановления: %v", err)
	}
	status := metav1.ConditionFalse
	reason := "Recovered"
	if required {
		status = metav1.ConditionTrue
		reason = "OperatorFailure"
	}
	apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: valkeyv1alpha1.ConditionTypeRecoveryRequired, Status: status,
		Reason: reason, LastTransitionTime: metav1.Now(),
	})
	if err := app.adminKubernetes.Status().Update(context.Background(), current); err != nil {
		t.Fatalf("записать условие восстановления: %v", err)
	}
}

func setValkeyCRPhase(
	t *testing.T,
	app *testAPI,
	resource *valkeyv1alpha1.ValkeyInstance,
	phase valkeyv1alpha1.InstancePhase,
) {
	t.Helper()

	current := &valkeyv1alpha1.ValkeyInstance{}
	if err := app.adminKubernetes.Get(context.Background(), client.ObjectKeyFromObject(resource), current); err != nil {
		t.Fatalf("перечитать ValkeyInstance для фазы: %v", err)
	}
	now := metav1.Now()
	current.Status.Phase = phase
	current.Status.ObservedAt = &now
	if err := app.adminKubernetes.Status().Update(context.Background(), current); err != nil {
		t.Fatalf("записать фазу %s: %v", phase, err)
	}
}

func waitForSyncRecoveryReason(t *testing.T, app *testAPI, instanceID uuid.UUID, present bool) {
	t.Helper()

	deadline := time.Now().Add(syncWaitTimeout)
	for time.Now().Before(deadline) {
		instance := loadValkey(t, app, instanceID)
		if (instance.SyncRecoveryReason != nil) == present {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("причина восстановления доставки не получила значение present=%t", present)
}

func cloneSecretData(data map[string][]byte) map[string][]byte {
	result := make(map[string][]byte, len(data))
	for key, value := range data {
		result[key] = slices.Clone(value)
	}

	return result
}

func assertSecretVersionPresent(t *testing.T, app *testAPI, namespace, slug string, version int64) {
	t.Helper()

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: valkeyv1alpha1.AuthSecretName(slug)}
	if err := app.adminKubernetes.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("прочитать Secret: %v", err)
	}
	passwordKey, err := valkeyv1alpha1.AppPasswordHashKey(version)
	if err != nil {
		t.Fatalf("получить ключ версии пароля: %v", err)
	}
	if _, exists := secret.Data[passwordKey]; !exists {
		t.Fatalf("в Secret нет ключа версии %d", version)
	}
}

func removeOldPasswordHash(t *testing.T, app *testAPI, namespace, slug string) {
	t.Helper()

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: valkeyv1alpha1.AuthSecretName(slug)}
	if err := app.adminKubernetes.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("прочитать Secret для очистки прежней версии: %v", err)
	}
	passwordKey, err := valkeyv1alpha1.AppPasswordHashKey(1)
	if err != nil {
		t.Fatalf("получить ключ прежней версии пароля: %v", err)
	}
	delete(secret.Data, passwordKey)
	if err := app.adminKubernetes.Update(context.Background(), secret); err != nil {
		t.Fatalf("очистить прежнюю версию пароля: %v", err)
	}
}

func assertOldPasswordHashAbsent(t *testing.T, app *testAPI, namespace, slug string) {
	t.Helper()

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: valkeyv1alpha1.AuthSecretName(slug)}
	if err := app.adminKubernetes.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("прочитать Secret после сверки: %v", err)
	}
	passwordKey, err := valkeyv1alpha1.AppPasswordHashKey(1)
	if err != nil {
		t.Fatalf("получить ключ прежней версии пароля: %v", err)
	}
	if _, exists := secret.Data[passwordKey]; exists {
		t.Fatal("доставка вернула очищенную прежнюю версию пароля")
	}
}

func tamperPasswordHash(t *testing.T, app *testAPI, namespace, slug string, version int64) {
	t.Helper()

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: valkeyv1alpha1.AuthSecretName(slug)}
	if err := app.adminKubernetes.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("прочитать Secret для проверки конфликта хеша: %v", err)
	}
	passwordKey, err := valkeyv1alpha1.AppPasswordHashKey(version)
	if err != nil {
		t.Fatalf("получить ключ версии пароля: %v", err)
	}
	secret.Data[passwordKey] = []byte(strings.Repeat("0", 64))
	if err := app.adminKubernetes.Update(context.Background(), secret); err != nil {
		t.Fatalf("подготовить конфликт хеша: %v", err)
	}
}

func assertPasswordHashRemainsTampered(t *testing.T, app *testAPI, namespace, slug string, version int64) {
	t.Helper()

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: valkeyv1alpha1.AuthSecretName(slug)}
	if err := app.adminKubernetes.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("прочитать Secret после конфликта хеша: %v", err)
	}
	passwordKey, err := valkeyv1alpha1.AppPasswordHashKey(version)
	if err != nil {
		t.Fatalf("получить ключ версии пароля: %v", err)
	}
	if string(secret.Data[passwordKey]) != strings.Repeat("0", 64) {
		t.Fatal("доставка молча перезаписала конфликтующий хеш")
	}
}

func restoreCurrentPasswordHash(
	t *testing.T,
	app *testAPI,
	namespace string,
	slug string,
	version int64,
	hash string,
) {
	t.Helper()

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: valkeyv1alpha1.AuthSecretName(slug)}
	if err := app.adminKubernetes.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("прочитать Secret для восстановления хеша: %v", err)
	}
	passwordKey, err := valkeyv1alpha1.AppPasswordHashKey(version)
	if err != nil {
		t.Fatalf("получить ключ версии пароля: %v", err)
	}
	secret.Data[passwordKey] = []byte(hash)
	if err := app.adminKubernetes.Update(context.Background(), secret); err != nil {
		t.Fatalf("восстановить хеш: %v", err)
	}
}

func removeServiceCredential(t *testing.T, app *testAPI, namespace, slug string) []byte {
	t.Helper()

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: valkeyv1alpha1.AuthSecretName(slug)}
	if err := app.adminKubernetes.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("прочитать Secret для проверки служебных полей: %v", err)
	}
	value := slices.Clone(secret.Data[valkeyv1alpha1.HealthPasswordKey])
	delete(secret.Data, valkeyv1alpha1.HealthPasswordKey)
	if err := app.adminKubernetes.Update(context.Background(), secret); err != nil {
		t.Fatalf("удалить служебное поле Secret: %v", err)
	}

	return value
}

func assertServiceCredentialAbsent(t *testing.T, app *testAPI, namespace, slug string) {
	t.Helper()

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: valkeyv1alpha1.AuthSecretName(slug)}
	if err := app.adminKubernetes.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("прочитать Secret после сверки служебных полей: %v", err)
	}
	if _, exists := secret.Data[valkeyv1alpha1.HealthPasswordKey]; exists {
		t.Fatal("доставка дописала отсутствующее служебное поле")
	}
}

func restoreServiceCredential(t *testing.T, app *testAPI, namespace, slug string, value []byte) {
	t.Helper()

	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: valkeyv1alpha1.AuthSecretName(slug)}
	if err := app.adminKubernetes.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("прочитать Secret для восстановления служебного поля: %v", err)
	}
	secret.Data[valkeyv1alpha1.HealthPasswordKey] = value
	if err := app.adminKubernetes.Update(context.Background(), secret); err != nil {
		t.Fatalf("восстановить служебное поле Secret: %v", err)
	}
}

func waitForValkeyInstance(
	t *testing.T,
	app *testAPI,
	slug string,
	matches func(*valkeyv1alpha1.ValkeyInstance) bool,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()

	deadline := time.Now().Add(syncWaitTimeout)
	key := client.ObjectKey{Namespace: "valkey-" + slug, Name: slug}
	for time.Now().Before(deadline) {
		resource := &valkeyv1alpha1.ValkeyInstance{}
		err := app.adminKubernetes.Get(context.Background(), key, resource)
		if err == nil && matches(resource) {
			return resource
		}
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("прочитать ValkeyInstance: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("ValkeyInstance %s не достиг ожидаемого состояния", slug)

	return nil
}

func getValkeyInstance(t *testing.T, app *testAPI, namespace, slug string) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()

	resource := &valkeyv1alpha1.ValkeyInstance{}
	key := client.ObjectKey{Namespace: namespace, Name: slug}
	if err := app.adminKubernetes.Get(context.Background(), key, resource); err != nil {
		t.Fatalf("прочитать ValkeyInstance: %v", err)
	}

	return resource
}

func waitForHTTPInstance(
	t *testing.T,
	app *testAPI,
	account testAccount,
	instanceID uuid.UUID,
	matches func(valkeydomain.Instance) bool,
) valkeydomain.Instance {
	t.Helper()

	deadline := time.Now().Add(syncWaitTimeout)
	for time.Now().Before(deadline) {
		response := app.requestJSON(t, http.MethodGet, instancePath(instanceID), nil, bearer(account.Token))
		if response.StatusCode == http.StatusOK {
			instance := decodeResponse[valkeydomain.Instance](t, response)
			if matches(instance) {
				return instance
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("HTTP не вернул ожидаемое состояние инстанса %s", instanceID)

	return valkeydomain.Instance{}
}

func waitForQuotaUsage(t *testing.T, app *testAPI, account testAccount, vcpu, ramGB int) {
	t.Helper()

	deadline := time.Now().Add(syncWaitTimeout)
	for time.Now().Before(deadline) {
		response := app.requestJSON(t, http.MethodGet, "/v1/me", nil, bearer(account.Token))
		if response.StatusCode == http.StatusOK {
			current := decodeResponse[currentUserResponse](t, response)
			if current.Usage.UsedVCPU == vcpu && current.Usage.UsedRAMGB == ramGB {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("резерв не стал vcpu=%d ram_gb=%d", vcpu, ramGB)
}

func assertImportedRows(t *testing.T, app *testAPI, instanceID uuid.UUID, nodes, events int64) {
	t.Helper()

	assertDatabaseCount(
		t,
		app.database.DB().Model(&store.ValkeyInstanceNode{}).Where("instance_id = ?", instanceID),
		nodes,
	)
	assertDatabaseCount(
		t,
		app.database.DB().Model(&store.ValkeyInstancePhaseEvent{}).Where("instance_id = ?", instanceID),
		events,
	)
}

func databaseCount(t *testing.T, query *gorm.DB) int64 {
	t.Helper()

	var count int64
	if err := query.Count(&count).Error; err != nil {
		t.Fatalf("посчитать строки: %v", err)
	}

	return count
}

func assertNamespaceAndSecretExist(t *testing.T, app *testAPI, namespace, slug string) {
	t.Helper()

	if err := app.adminKubernetes.Get(
		context.Background(), client.ObjectKey{Name: namespace}, &corev1.Namespace{},
	); err != nil {
		t.Fatalf("Namespace удалён до завершения ValkeyInstance: %v", err)
	}
	if err := app.adminKubernetes.Get(
		context.Background(),
		client.ObjectKey{Namespace: namespace, Name: valkeyv1alpha1.AuthSecretName(slug)},
		&corev1.Secret{},
	); err != nil {
		t.Fatalf("Secret удалён до завершения ValkeyInstance: %v", err)
	}
}

func removeInstanceFinalizer(t *testing.T, app *testAPI, resource *valkeyv1alpha1.ValkeyInstance) {
	t.Helper()

	current := &valkeyv1alpha1.ValkeyInstance{}
	if err := app.adminKubernetes.Get(context.Background(), client.ObjectKeyFromObject(resource), current); err != nil {
		t.Fatalf("перечитать удаляемый ValkeyInstance: %v", err)
	}
	current.Finalizers = slices.DeleteFunc(current.Finalizers, func(value string) bool {
		return value == valkeyv1alpha1.InstanceFinalizer
	})
	if err := app.adminKubernetes.Update(context.Background(), current); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("снять защитную отметку ValkeyInstance: %v", err)
	}
}

func waitForKubernetesDeletion(t *testing.T, app *testAPI, namespace, slug string) {
	t.Helper()

	deadline := time.Now().Add(syncWaitTimeout)
	for time.Now().Before(deadline) {
		namespaceErr := app.adminKubernetes.Get(
			context.Background(), client.ObjectKey{Name: namespace}, &corev1.Namespace{},
		)
		resourceErr := app.adminKubernetes.Get(
			context.Background(), client.ObjectKey{Namespace: namespace, Name: slug},
			&valkeyv1alpha1.ValkeyInstance{},
		)
		if apierrors.IsNotFound(namespaceErr) && apierrors.IsNotFound(resourceErr) {
			return
		}
		if namespaceErr != nil && !apierrors.IsNotFound(namespaceErr) {
			t.Fatalf("проверить удаление Namespace: %v", namespaceErr)
		}
		if resourceErr != nil && !apierrors.IsNotFound(resourceErr) {
			t.Fatalf("проверить удаление ValkeyInstance: %v", resourceErr)
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("объекты %s не удалены", slug)
}

func waitForHTTPNotFound(t *testing.T, app *testAPI, account testAccount, instanceID uuid.UUID) {
	t.Helper()

	deadline := time.Now().Add(syncWaitTimeout)
	for time.Now().Before(deadline) {
		response := app.requestJSON(t, http.MethodGet, instancePath(instanceID), nil, bearer(account.Token))
		if response.StatusCode == http.StatusNotFound {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("удалённый инстанс %s остаётся доступен", instanceID)
}

func completeDeletionWithoutOperator(t *testing.T, app *testAPI, slug string, instanceID uuid.UUID) {
	t.Helper()

	deadline := time.Now().Add(syncWaitTimeout)
	finalizerRemoved := false
	for time.Now().Before(deadline) {
		service := valkeysync.NewService(app.database, app.kubernetes, app.logger)
		if err := service.RunDelivery(context.Background()); err != nil {
			t.Fatalf("продолжить удаление: %v", err)
		}
		resource := &valkeyv1alpha1.ValkeyInstance{}
		key := client.ObjectKey{Namespace: "valkey-" + slug, Name: slug}
		err := app.adminKubernetes.Get(context.Background(), key, resource)
		if err == nil && resource.DeletionTimestamp != nil && !finalizerRemoved {
			resource.Finalizers = slices.DeleteFunc(resource.Finalizers, func(value string) bool {
				return value == valkeyv1alpha1.InstanceFinalizer
			})
			if err := app.adminKubernetes.Update(context.Background(), resource); err != nil &&
				!apierrors.IsNotFound(err) {
				t.Fatalf("снять защитную отметку при удалении: %v", err)
			}
			finalizerRemoved = true
		} else if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("проверить удаляемый ValkeyInstance: %v", err)
		}

		var record store.ValkeyInstance
		if err := app.database.DB().Where("id = ?", instanceID).First(&record).Error; err != nil {
			t.Fatalf("прочитать удаляемый инстанс: %v", err)
		}
		if record.DeletedAt != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	var record store.ValkeyInstance
	_ = app.database.DB().Where("id = ?", instanceID).First(&record).Error
	resourceObject := &valkeyv1alpha1.ValkeyInstance{}
	resourceErr := app.adminKubernetes.Get(
		context.Background(),
		client.ObjectKey{Namespace: "valkey-" + slug, Name: slug},
		resourceObject,
	)
	namespaceObject := &corev1.Namespace{}
	namespaceErr := app.adminKubernetes.Get(
		context.Background(),
		client.ObjectKey{Name: "valkey-" + slug},
		namespaceObject,
	)
	stage := ""
	if record.DeletionStage != nil {
		stage = string(*record.DeletionStage)
	}
	t.Fatalf(
		"удаление инстанса %s не завершилось: stage=%s cr=%v cr_deleting=%v namespace=%v namespace_deleting=%v namespace_finalizers=%v namespace_conditions=%v",
		instanceID,
		stage,
		resourceErr,
		resourceObject.DeletionTimestamp,
		namespaceErr,
		namespaceObject.DeletionTimestamp,
		namespaceObject.Spec.Finalizers,
		namespaceObject.Status.Conditions,
	)
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("ожидаемая граница операции не достигнута")
	}
}

func waitError(t *testing.T, result <-chan error) error {
	t.Helper()

	select {
	case err := <-result:
		return err
	case <-time.After(syncWaitTimeout):
		t.Fatal("операция не завершилась")

		return nil
	}
}

func cleanupSyncNamespace(t *testing.T, app *testAPI, namespace string) {
	t.Helper()

	resources := &valkeyv1alpha1.ValkeyInstanceList{}
	if err := app.adminKubernetes.List(context.Background(), resources, client.InNamespace(namespace)); err == nil {
		for index := range resources.Items {
			resource := &resources.Items[index]
			if len(resource.Finalizers) > 0 {
				resource.Finalizers = nil
				if err := app.adminKubernetes.Update(context.Background(), resource); err != nil &&
					!apierrors.IsNotFound(err) {
					t.Errorf("снять защитные отметки тестового ValkeyInstance: %v", err)
				}
			}
		}
	}
	object := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := app.adminKubernetes.Delete(context.Background(), object); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("удалить тестовый Namespace: %v", err)
	}
}

func pointer[T any](value T) *T {
	return &value
}
