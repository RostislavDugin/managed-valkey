//go:build envtest

package operator_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	"github.com/RostislavDugin/managed-valkey/operator/internal/operator"
	valkeyclient "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func TestEnvtestManagerStartsAndStopsOnContext(t *testing.T) {
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

func TestEnvtestValkeyInstanceContract(t *testing.T) {
	restConfig := startEnvironment(t)
	ctx := context.Background()

	k8s, err := client.New(restConfig, client.Options{Scheme: operator.NewScheme()})
	if err != nil {
		t.Fatalf("создать клиент: %v", err)
	}

	legacy := &valkeyv1alpha1.ValkeyInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-a1b2c3", Namespace: "default"},
		Spec: valkeyv1alpha1.ValkeyInstanceSpec{
			InstanceID: "01991ad0-1234-7000-8000-000000000001",
			Slug:       "legacy-a1b2c3",
		},
	}
	if err := k8s.Create(ctx, legacy); err != nil {
		t.Fatalf("создать CR старого формата: %v", err)
	}

	full := newCompleteInstance("shop-a1b2c3")
	full.Spec.DesiredGeneration = 7
	if err := k8s.Create(ctx, full); err != nil {
		t.Fatalf("создать полный CR: %v", err)
	}

	if full.Generation == full.Spec.DesiredGeneration {
		t.Fatalf("metadata.generation и desiredGeneration неожиданно равны %d", full.Generation)
	}

	full.Spec.PublicPort++
	if err := k8s.Update(ctx, full); err != nil {
		t.Fatalf("обновить полный CR: %v", err)
	}
	if full.Generation != 2 || full.Spec.DesiredGeneration != 7 {
		t.Fatalf("поколения после обновления: metadata=%d desired=%d", full.Generation, full.Spec.DesiredGeneration)
	}

	assertImmutableIdentity(t, ctx, k8s, full, func(spec *valkeyv1alpha1.ValkeyInstanceSpec) {
		spec.InstanceID = "01991ad0-1234-7000-8000-000000000099"
	})
	assertImmutableIdentity(t, ctx, k8s, full, func(spec *valkeyv1alpha1.ValkeyInstanceSpec) {
		spec.Slug = "other-a1b2c3"
	})

	assertInvalidSize(t, ctx, k8s, "invalid-vcpu", 3, 4)
	assertInvalidSize(t, ctx, k8s, "invalid-ratio", 16, 1)

	now := metav1.Now()
	primaryOrdinal := int32(0)
	full.Status = valkeyv1alpha1.ValkeyInstanceStatus{
		CredentialsInitialized: true,
		Initialized:            true,
		Phase:                  valkeyv1alpha1.InstancePhaseRunning,
		ObservedAt:             &now,
		PrimaryOrdinal:         &primaryOrdinal,
		PrimaryPodUID:          "pod-uid",
		PrimaryContainerID:     "containerd://container-id",
		ObservedGeneration:     7,
		AppliedPasswordVersion: 1,
		AcceptedConfiguration: &valkeyv1alpha1.AcceptedConfiguration{
			InstanceID:        full.Spec.InstanceID,
			Slug:              full.Spec.Slug,
			Mode:              valkeyv1alpha1.ValkeyModeSingle,
			VCPU:              1,
			RAMGB:             4,
			PublicPort:        full.Spec.PublicPort,
			Whitelist:         valkeyv1alpha1.WhitelistSpec{},
			PasswordVersion:   1,
			DesiredGeneration: 7,
		},
		Applied: &valkeyv1alpha1.AppliedConfiguration{
			Mode:  valkeyv1alpha1.ValkeyModeSingle,
			VCPU:  1,
			RAMGB: 4,
		},
		Network: &valkeyv1alpha1.NetworkStatus{
			VerificationStatus:  valkeyv1alpha1.NetworkVerificationVerified,
			VerifiedAt:          &now,
			DesiredFingerprint:  "desired",
			VerifiedFingerprint: "verified",
			EnvoyProcesses: []valkeyv1alpha1.EnvoyProcessStatus{{
				PodUID:      "envoy-pod-uid",
				NodeName:    "worker-0",
				NodeUID:     "node-uid",
				ContainerID: "containerd://envoy-container-id",
			}},
		},
		Deletion: &valkeyv1alpha1.DeletionStatus{
			Stage:     valkeyv1alpha1.DeletionStageRemovingNetwork,
			StartedAt: now,
		},
		Nodes: []valkeyv1alpha1.NodeStatus{{
			Ordinal:     0,
			PodUID:      "pod-uid",
			ContainerID: "containerd://container-id",
			RunID:       "run-id",
			NodeName:    "worker-0",
			NodeUID:     "node-uid",
			Role:        valkeyv1alpha1.NodeRolePrimary,
			Readiness:   true,
		}},
	}
	if err := k8s.Status().Update(ctx, full); err != nil {
		t.Fatalf("сохранить status: %v", err)
	}

	observed := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(full), observed); err != nil {
		t.Fatalf("прочитать status: %v", err)
	}
	if observed.Status.AcceptedConfiguration == nil || observed.Status.AcceptedConfiguration.DesiredGeneration != 7 {
		t.Fatalf("снимок конфигурации не сохранился: %+v", observed.Status.AcceptedConfiguration)
	}
	if len(observed.Status.Nodes) != 1 || observed.Status.Nodes[0].RunID != "run-id" {
		t.Fatalf("идентичность процесса не сохранилась: %+v", observed.Status.Nodes)
	}
}

func TestEnvtestCT03OperationStatusContract(t *testing.T) {
	restConfig := startEnvironment(t)
	ctx := context.Background()

	k8s, err := client.New(restConfig, client.Options{Scheme: operator.NewScheme()})
	if err != nil {
		t.Fatalf("создать клиент: %v", err)
	}

	instance := newCompleteInstance("operations-a1b2c3")
	instance.Spec.DesiredGeneration = 17
	if err := k8s.Create(ctx, instance); err != nil {
		t.Fatalf("создать CR: %v", err)
	}
	if instance.Generation == instance.Spec.DesiredGeneration {
		t.Fatalf("metadata.generation подменяет desiredGeneration: %d", instance.Generation)
	}

	now := metav1.Now()
	deadline := metav1.NewTime(now.Add(5 * time.Second))
	source := valkeyv1alpha1.ProcessIdentity{
		PodUID:      "source-pod",
		ContainerID: "containerd://source",
		RunID:       "source-run",
		NodeName:    "worker-0",
		NodeUID:     "source-node",
	}
	candidate := valkeyv1alpha1.ProcessIdentity{
		PodUID:      "candidate-pod",
		ContainerID: "containerd://candidate",
		RunID:       "candidate-run",
		NodeName:    "worker-1",
		NodeUID:     "candidate-node",
	}
	instance.Status = valkeyv1alpha1.ValkeyInstanceStatus{
		Phase: valkeyv1alpha1.InstancePhaseUpdating,
		Nodes: []valkeyv1alpha1.NodeStatus{{
			Ordinal: 1, PodUID: candidate.PodUID, ContainerID: candidate.ContainerID,
			RunID: candidate.RunID, NodeName: candidate.NodeName, NodeUID: candidate.NodeUID,
			Role: valkeyv1alpha1.NodeRoleReplica,
			Replication: &valkeyv1alpha1.ReplicationStatus{
				ReplicationID: "replication-id", Offset: 41, ObservedAt: now,
			},
		}},
		PreviousProcesses: []valkeyv1alpha1.NodeStatus{{
			Ordinal: 0, PodUID: source.PodUID, ContainerID: source.ContainerID,
			RunID: source.RunID, NodeName: source.NodeName, NodeUID: source.NodeUID,
			Role: valkeyv1alpha1.NodeRolePrimary,
		}},
		Failover: &valkeyv1alpha1.FailoverStatus{
			Reason:                 valkeyv1alpha1.FailoverReasonResize,
			Stage:                  valkeyv1alpha1.FailoverStagePromoting,
			StartedAt:              now,
			Source:                 source,
			Candidate:              &candidate,
			SourceAppDisabled:      true,
			SourceClientsKilled:    true,
			CandidateAppDisabled:   true,
			CandidateClientsKilled: true,
			CandidateMayBePrimary:  true,
			ReplicationID:          "replication-id",
			ControlOffset:          42,
			OffsetDeadline:         &deadline,
		},
		Rollout: &valkeyv1alpha1.RolloutStatus{
			DesiredGeneration: 17,
			VCPU:              2,
			RAMGB:             8,
			Image:             "valkey.example/operator:run-17",
			Stage:             valkeyv1alpha1.RolloutStageReplacingReplicas,
			Process:           &candidate,
		},
		CredentialRotation: &valkeyv1alpha1.CredentialRotationStatus{
			TargetVersion:   3,
			PreviousVersion: 2,
			Stage:           valkeyv1alpha1.CredentialRotationStageUpdatingPrimary,
			Confirmations: []valkeyv1alpha1.CredentialRotationConfirmation{{
				Process: candidate,
				Version: 3,
			}},
		},
	}
	if err := k8s.Status().Update(ctx, instance); err != nil {
		t.Fatalf("сохранить стадии операций: %v", err)
	}

	observed := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observed); err != nil {
		t.Fatalf("прочитать стадии операций: %v", err)
	}
	if observed.Status.Phase != valkeyv1alpha1.InstancePhaseUpdating {
		t.Fatalf("фаза не сохранилась: %q", observed.Status.Phase)
	}
	if observed.Status.Failover == nil || observed.Status.Failover.Candidate == nil ||
		observed.Status.Failover.Candidate.ContainerID != candidate.ContainerID ||
		observed.Status.Failover.OffsetDeadline == nil ||
		observed.Status.Failover.OffsetDeadline.Time.Unix() != deadline.Time.Unix() {
		t.Fatalf("failover не сохранился: %+v", observed.Status.Failover)
	}
	if observed.Status.Rollout == nil || observed.Status.Rollout.Image != "valkey.example/operator:run-17" {
		t.Fatalf("rollout не сохранился: %+v", observed.Status.Rollout)
	}
	if observed.Status.CredentialRotation == nil ||
		len(observed.Status.CredentialRotation.Confirmations) != 1 ||
		observed.Status.CredentialRotation.Confirmations[0].Process.RunID != candidate.RunID {
		t.Fatalf("ротация не сохранилась: %+v", observed.Status.CredentialRotation)
	}
	if len(observed.Status.Nodes) != 1 || observed.Status.Nodes[0].Replication == nil ||
		observed.Status.Nodes[0].Replication.Offset != 41 ||
		len(observed.Status.PreviousProcesses) != 1 ||
		observed.Status.PreviousProcesses[0].RunID != source.RunID {
		t.Fatalf("история процессов не сохранилась: nodes=%+v previous=%+v",
			observed.Status.Nodes, observed.Status.PreviousProcesses)
	}

	observed.Status.Phase = valkeyv1alpha1.InstancePhaseDegraded
	if err := k8s.Status().Update(ctx, observed); err != nil {
		t.Fatalf("сохранить degraded: %v", err)
	}

	tests := map[string]func(*valkeyv1alpha1.ValkeyInstanceStatus){
		"phase": func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			status.Phase = valkeyv1alpha1.InstancePhase("invalid")
		},
		"failover reason": func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			status.Failover.Reason = valkeyv1alpha1.FailoverReason("invalid")
		},
		"failover stage": func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			status.Failover.Stage = valkeyv1alpha1.FailoverStage("invalid")
		},
		"rollout stage": func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			status.Rollout.Stage = valkeyv1alpha1.RolloutStage("invalid")
		},
		"credential rotation stage": func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			status.CredentialRotation.Stage = valkeyv1alpha1.CredentialRotationStage("invalid")
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			current := &valkeyv1alpha1.ValkeyInstance{}
			if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), current); err != nil {
				t.Fatalf("прочитать CR: %v", err)
			}
			mutate(&current.Status)
			if err := k8s.Status().Update(ctx, current); !apierrors.IsInvalid(err) {
				t.Fatalf("некорректное значение принято: %v", err)
			}
		})
	}
}

func assertImmutableIdentity(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	instance *valkeyv1alpha1.ValkeyInstance,
	change func(*valkeyv1alpha1.ValkeyInstanceSpec),
) {
	t.Helper()

	updated := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), updated); err != nil {
		t.Fatalf("прочитать CR перед изменением идентичности: %v", err)
	}
	change(&updated.Spec)

	if err := k8s.Update(ctx, updated); !apierrors.IsInvalid(err) {
		t.Fatalf("изменение идентичности: получено %v, ожидалась Invalid", err)
	}
}

func assertInvalidSize(t *testing.T, ctx context.Context, k8s client.Client, name string, vcpu, ramGB int32) {
	t.Helper()

	instance := newCompleteInstance(name)
	instance.Spec.VCPU = vcpu
	instance.Spec.RAMGB = ramGB

	if err := k8s.Create(ctx, instance); !apierrors.IsInvalid(err) {
		t.Fatalf("размер %d vCPU/%d GB: получено %v, ожидалась Invalid", vcpu, ramGB, err)
	}
}

func newCompleteInstance(name string) *valkeyv1alpha1.ValkeyInstance {
	return &valkeyv1alpha1.ValkeyInstance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: valkeyv1alpha1.ValkeyInstanceSpec{
			InstanceID:        "01991ad0-1234-7000-8000-000000000002",
			Slug:              name,
			Mode:              valkeyv1alpha1.ValkeyModeSingle,
			VCPU:              1,
			RAMGB:             4,
			PublicPort:        41379,
			Whitelist:         &valkeyv1alpha1.WhitelistSpec{},
			PasswordVersion:   1,
			DesiredGeneration: 1,
		},
	}
}

func TestEnvtestReconcileDiagnosesIncompleteResource(t *testing.T) {
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

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "valkey-shop-a1b2c3",
		Labels: map[string]string{
			"valkey.h3llo-demo.com/instance": "shop-a1b2c3",
			"valkey.h3llo-demo.com/user-id":  "01991ad0-1234-7000-8000-000000000010",
		},
	}}
	if err := k8s.Create(ctx, namespace); err != nil {
		t.Fatalf("создать namespace: %v", err)
	}

	instance := &valkeyv1alpha1.ValkeyInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-a1b2c3", Namespace: namespace.Name},
		Spec: valkeyv1alpha1.ValkeyInstanceSpec{
			InstanceID: "01991ad0-1234-7000-8000-000000000001",
			Slug:       "shop-a1b2c3",
		},
	}

	if err := k8s.Create(ctx, instance); err != nil {
		t.Fatalf("создать ValkeyInstance: %v", err)
	}

	observed := waitForInstanceStatus(t, ctx, k8s, client.ObjectKeyFromObject(instance), func(
		status valkeyv1alpha1.ValkeyInstanceStatus,
	) bool {
		condition := apimeta.FindStatusCondition(status.Conditions, "Accepted")
		return condition != nil && condition.Status == metav1.ConditionFalse &&
			condition.Reason == "MissingRequiredFields"
	})

	if len(observed.Finalizers) != 0 {
		t.Errorf("оператор добавил finalizers %v", observed.Finalizers)
	}

	if observed.Status.AcceptedConfiguration != nil || observed.Status.ObservedGeneration != 0 {
		t.Errorf("оператор подтвердил неполный CR: %+v", observed.Status)
	}

	assertNoDependents(ctx, t, k8s, namespace.Name)
}

func TestEnvtestReconcileAcceptsStableInitialConfiguration(t *testing.T) {
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

	creating := createCompleteInstance(t, ctx, k8s, "shop-a1b2c3", valkeyv1alpha1.ValkeyModeSingle)
	creating = waitForAcceptedConfiguration(t, ctx, k8s, creating)

	creating = updateInstance(t, ctx, k8s, client.ObjectKeyFromObject(creating), func(
		instance *valkeyv1alpha1.ValkeyInstance,
	) {
		instance.Spec.RAMGB = 8
		instance.Spec.DesiredGeneration = 2
	})
	creating = waitForConditionReason(
		t, ctx, k8s, creating, "ConfigurationPending", "CurrentGenerationIncomplete",
	)
	if creating.Status.AcceptedConfiguration.RAMGB != 4 ||
		creating.Status.AcceptedConfiguration.DesiredGeneration != 1 ||
		creating.Status.ObservedGeneration != 0 {
		t.Fatalf("изменился принятый снимок: %+v", creating.Status)
	}

	creating = updateInstance(t, ctx, k8s, client.ObjectKeyFromObject(creating), func(
		instance *valkeyv1alpha1.ValkeyInstance,
	) {
		instance.Spec.RAMGB = 4
		instance.Spec.DesiredGeneration = 1
	})
	waitForInstanceStatus(t, ctx, k8s, client.ObjectKeyFromObject(creating), func(
		status valkeyv1alpha1.ValkeyInstanceStatus,
	) bool {
		return apimeta.FindStatusCondition(status.Conditions, "ConfigurationPending") == nil
	})

	running := createCompleteInstance(t, ctx, k8s, "cache-b2c3d4", valkeyv1alpha1.ValkeyModeSingle)
	running = waitForAcceptedConfiguration(t, ctx, k8s, running)
	running = updateInstanceStatus(t, ctx, k8s, client.ObjectKeyFromObject(running), func(
		status *valkeyv1alpha1.ValkeyInstanceStatus,
	) {
		status.Phase = valkeyv1alpha1.InstancePhaseRunning
		status.Initialized = true
	})
	running = updateInstance(t, ctx, k8s, client.ObjectKeyFromObject(running), func(
		instance *valkeyv1alpha1.ValkeyInstance,
	) {
		instance.Spec.Whitelist.IsEnabled = true
		instance.Spec.Whitelist.CIDRs = []string{"203.0.113.0/24"}
		instance.Spec.DesiredGeneration = 2
	})
	running = waitForUnsupportedChange(t, ctx, k8s, running)
	if running.Status.Phase != valkeyv1alpha1.InstancePhaseRunning || running.Status.ObservedGeneration != 0 {
		t.Fatalf("неподдерживаемое изменение подтверждено: %+v", running.Status)
	}

	running = updateInstance(t, ctx, k8s, client.ObjectKeyFromObject(running), func(
		instance *valkeyv1alpha1.ValkeyInstance,
	) {
		instance.Finalizers = []string{"test.finalizer"}
	})
	if err := k8s.Delete(ctx, running); err != nil {
		t.Fatalf("запросить удаление: %v", err)
	}
	deleting := waitForInstanceStatus(t, ctx, k8s, client.ObjectKeyFromObject(running), func(
		status valkeyv1alpha1.ValkeyInstanceStatus,
	) bool {
		return status.Phase == valkeyv1alpha1.InstancePhaseRunning
	})
	if deleting.DeletionTimestamp.IsZero() {
		t.Fatal("deletionTimestamp не установлен")
	}

	ha := createCompleteInstance(t, ctx, k8s, "queue-c3d4e5", valkeyv1alpha1.ValkeyModeHA)
	ha = waitForAcceptedConfiguration(t, ctx, k8s, ha)
	if ha.Status.AcceptedConfiguration.Mode != valkeyv1alpha1.ValkeyModeHA {
		t.Fatalf("HA принят с неверным режимом: %+v", ha.Status.AcceptedConfiguration)
	}

	assertNoDependents(ctx, t, k8s, creating.Namespace)
	assertNoDependents(ctx, t, k8s, running.Namespace)
	assertNoDependents(ctx, t, k8s, ha.Namespace)
}

func TestEnvtestCredentialsInitializationSurvivesRestartBoundary(t *testing.T) {
	restConfig := startEnvironment(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k8s, err := client.New(restConfig, client.Options{Scheme: operator.NewScheme()})
	if err != nil {
		t.Fatalf("создать клиент: %v", err)
	}

	hash := strings.Repeat("ab", 32)
	fresh := prepareInstanceWithSecret(t, ctx, k8s, "fresh-d4e5f6", hash, nil)

	operatorPassword := encodedPassword(1)
	replicaPassword := encodedPassword(2)
	healthPassword := encodedPassword(3)
	persistedCredentials := map[string][]byte{
		valkeyv1alpha1.OperatorPasswordKey: operatorPassword,
		valkeyv1alpha1.ReplicaPasswordKey:  replicaPassword,
		valkeyv1alpha1.HealthPasswordKey:   healthPassword,
		valkeyv1alpha1.UsersACLKey: valkeyclient.InitialACL(
			hash,
			string(operatorPassword),
			string(replicaPassword),
			string(healthPassword),
		),
	}
	persisted := prepareInstanceWithSecret(t, ctx, k8s, "saved-e5f6g7", hash, persistedCredentials)

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
	go func() { _ = operator.Run(ctx, mgr) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache manager не синхронизировался")
	}

	for _, instance := range []*valkeyv1alpha1.ValkeyInstance{fresh, persisted} {
		observed := waitForInstanceStatus(t, ctx, k8s, client.ObjectKeyFromObject(instance), func(
			status valkeyv1alpha1.ValkeyInstanceStatus,
		) bool {
			return status.CredentialsInitialized
		})
		if observed.Status.Initialized {
			t.Fatalf("%s преждевременно получил initialized", instance.Name)
		}

		secret := &corev1.Secret{}
		secretKey := client.ObjectKey{
			Namespace: instance.Namespace,
			Name:      valkeyv1alpha1.AuthSecretName(instance.Name),
		}
		if err := k8s.Get(ctx, secretKey, secret); err != nil {
			t.Fatalf("прочитать Secret %s: %v", instance.Name, err)
		}
		credentials, err := valkeyv1alpha1.ParseServiceCredentials(secret.Data)
		if err != nil || credentials == nil {
			t.Fatalf("разобрать Secret %s: credentials=%+v error=%v", instance.Name, credentials, err)
		}
		if bytes.Equal(credentials.OperatorPassword, credentials.ReplicaPassword) ||
			bytes.Equal(credentials.OperatorPassword, credentials.HealthPassword) ||
			bytes.Equal(credentials.ReplicaPassword, credentials.HealthPassword) {
			t.Fatalf("%s получил совпадающие служебные пароли", instance.Name)
		}
		if string(secret.Data["api-field"]) != "preserved" {
			t.Fatalf("%s потерял поле API", instance.Name)
		}
		if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].UID != instance.UID ||
			secret.OwnerReferences[0].Controller == nil || !*secret.OwnerReferences[0].Controller {
			t.Fatalf("%s получил неверный ownerReference: %+v", instance.Name, secret.OwnerReferences)
		}
	}

	persistedSecret := &corev1.Secret{}
	if err := k8s.Get(ctx, client.ObjectKey{
		Namespace: persisted.Namespace,
		Name:      valkeyv1alpha1.AuthSecretName(persisted.Name),
	}, persistedSecret); err != nil {
		t.Fatalf("прочитать сохранённый Secret: %v", err)
	}
	if !bytes.Equal(persistedSecret.Data[valkeyv1alpha1.OperatorPasswordKey], operatorPassword) ||
		!bytes.Equal(persistedSecret.Data[valkeyv1alpha1.ReplicaPasswordKey], replicaPassword) ||
		!bytes.Equal(persistedSecret.Data[valkeyv1alpha1.HealthPasswordKey], healthPassword) {
		t.Fatal("сохранённые служебные пароли были заменены")
	}

	assertCredentialsPrecedeStatefulSet(ctx, t, k8s, fresh)
	assertCredentialsPrecedeStatefulSet(ctx, t, k8s, persisted)
}

func TestEnvtestSingleResourcesAreOwnedAndIdempotent(t *testing.T) {
	restConfig := startEnvironment(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k8s, err := client.New(restConfig, client.Options{Scheme: operator.NewScheme()})
	if err != nil {
		t.Fatalf("создать клиент: %v", err)
	}
	instance := prepareInstanceWithSecret(
		t,
		ctx,
		k8s,
		"objects-j0k1l2",
		strings.Repeat("ab", 32),
		nil,
	)

	mgr, err := operator.NewManager(restConfig, config.Config{
		SystemNamespace: "default",
		ValkeyImage:     "valkey/valkey:8.1.9",
	}, discardLogger(),
		operator.WithSkipControllerNameValidation(),
		operator.WithProbeAddr(freeAddress(t)),
		operator.WithLeaderElection(false),
	)
	if err != nil {
		t.Fatalf("создать manager: %v", err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- operator.Run(ctx, mgr) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache manager не синхронизировался")
	}

	statefulSet := waitForStatefulSet(t, ctx, k8s, client.ObjectKeyFromObject(instance))
	if statefulSet.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType ||
		statefulSet.Spec.Replicas == nil || *statefulSet.Spec.Replicas != 1 {
		t.Fatalf("неверный StatefulSet: %+v", statefulSet.Spec)
	}
	if len(statefulSet.OwnerReferences) != 1 || statefulSet.OwnerReferences[0].UID != instance.UID {
		t.Fatalf("StatefulSet не принадлежит CR: %v", statefulSet.OwnerReferences)
	}

	configMaps, services, policies := waitForSingleResources(t, ctx, k8s, instance.Namespace)
	if len(configMaps.Items) != 1 || configMaps.Items[0].Immutable == nil || !*configMaps.Items[0].Immutable {
		t.Fatalf("неверные ConfigMap: %+v", configMaps.Items)
	}
	assertOwnedBy(t, &configMaps.Items[0], instance.UID)
	if len(services.Items) != 2 {
		t.Fatalf("создано %d Services вместо 2", len(services.Items))
	}
	for index := range services.Items {
		if strings.HasSuffix(services.Items[index].Name, "-ro") ||
			strings.Contains(services.Items[index].Name, "replica") {
			t.Fatalf("single получил Service %s", services.Items[index].Name)
		}
		assertOwnedBy(t, &services.Items[index], instance.UID)
	}
	if len(policies.Items) != 1 {
		t.Fatalf("создано %d NetworkPolicy вместо 1", len(policies.Items))
	}
	assertOwnedBy(t, &policies.Items[0], instance.UID)
	pdbs := &policyv1.PodDisruptionBudgetList{}
	if err := k8s.List(ctx, pdbs, client.InNamespace(instance.Namespace)); err != nil {
		t.Fatalf("прочитать PDB: %v", err)
	}
	if len(pdbs.Items) != 0 {
		t.Fatalf("single получил PDB: %v", pdbs.Items)
	}

	haNamespace := createInstanceNamespace(t, ctx, k8s, "objects-ha-k1l2m3")
	haSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: valkeyv1alpha1.AuthSecretName("objects-ha-k1l2m3"), Namespace: haNamespace.Name,
		},
		Data: map[string][]byte{
			valkeyv1alpha1.AppPasswordHashKeyPrefix + "1": []byte(strings.Repeat("cd", 32)),
		},
	}
	if err := k8s.Create(ctx, haSecret); err != nil {
		t.Fatalf("создать Secret HA: %v", err)
	}
	ha := newCompleteInstance("objects-ha-k1l2m3")
	ha.Namespace = haNamespace.Name
	ha.Spec.Mode = valkeyv1alpha1.ValkeyModeHA
	if err := k8s.Create(ctx, ha); err != nil {
		t.Fatalf("создать HA: %v", err)
	}
	haStatefulSet := waitForStatefulSet(t, ctx, k8s, client.ObjectKeyFromObject(ha))
	if haStatefulSet.Spec.Replicas == nil || *haStatefulSet.Spec.Replicas != 3 ||
		haStatefulSet.Spec.Template.Spec.Affinity == nil ||
		haStatefulSet.Spec.Template.Spec.Affinity.PodAntiAffinity == nil ||
		len(
			haStatefulSet.Spec.Template.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		) != 1 {
		t.Fatalf("неверный StatefulSet HA: %+v", haStatefulSet.Spec)
	}
	haPDB := waitForPodDisruptionBudget(t, ctx, k8s, client.ObjectKeyFromObject(ha))
	if haPDB.Spec.MinAvailable == nil || haPDB.Spec.MinAvailable.IntVal != 2 {
		t.Fatalf("неверный PDB HA: %+v", haPDB.Spec)
	}
	assertOwnedBy(t, haPDB, ha.UID)

	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("остановить manager: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("manager не остановился")
	}

	serviceVersions := make(map[string]string, len(services.Items))
	for _, service := range services.Items {
		serviceVersions[service.Name] = service.ResourceVersion
	}
	reconciler := &operator.ValkeyInstanceReconciler{
		Client:          k8s,
		Scheme:          operator.NewScheme(),
		SystemNamespace: "default",
		ValkeyImage:     "valkey/valkey:8.1.9",
		BaseDomain:      config.DefaultBaseDomain,
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "valkey", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "envoy",
			Listeners: []gatewayv1.Listener{{
				Name: "bootstrap", Protocol: gatewayv1.TCPProtocolType, Port: 41379,
			}},
		},
	}
	if err := k8s.Create(context.Background(), gateway); err != nil {
		t.Fatalf("создать общий Gateway: %v", err)
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("создать сетевые ресурсы перед проверкой: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("зафиксировать состояние сети перед проверкой: %v", err)
	}
	currentInstance := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(instance), currentInstance); err != nil {
		t.Fatalf("прочитать CR перед неизменным reconcile: %v", err)
	}
	instanceVersion := currentInstance.ResourceVersion
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("неизменный reconcile: %v", err)
	}

	assertResourceVersion(t, context.Background(), k8s, currentInstance, instanceVersion)
	assertResourceVersion(t, context.Background(), k8s, statefulSet, statefulSet.ResourceVersion)
	assertResourceVersion(t, context.Background(), k8s, &configMaps.Items[0], configMaps.Items[0].ResourceVersion)
	for index := range services.Items {
		assertResourceVersion(
			t,
			context.Background(),
			k8s,
			&services.Items[index],
			serviceVersions[services.Items[index].Name],
		)
	}
	assertResourceVersion(t, context.Background(), k8s, &policies.Items[0], policies.Items[0].ResourceVersion)
}

func TestEnvtestCredentialFailuresDoNotRegenerateOrExposeSecrets(t *testing.T) {
	restConfig := startEnvironment(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k8s, err := client.New(restConfig, client.Options{Scheme: operator.NewScheme()})
	if err != nil {
		t.Fatalf("создать клиент: %v", err)
	}

	missingNamespace := createInstanceNamespace(t, ctx, k8s, "missing-f6g7h8")
	missing := newCompleteInstance("missing-f6g7h8")
	missing.Namespace = missingNamespace.Name
	if err := k8s.Create(ctx, missing); err != nil {
		t.Fatalf("создать CR без Secret: %v", err)
	}

	noHash := prepareInstanceWithSecret(t, ctx, k8s, "nohash-g7h8i9", "", nil)
	hash := strings.Repeat("ab", 32)
	partialPassword := encodedPassword(4)
	partial := prepareInstanceWithSecret(t, ctx, k8s, "partial-h8i9j0", hash, map[string][]byte{
		valkeyv1alpha1.OperatorPasswordKey: partialPassword,
	})
	recovering := prepareInstanceWithSecret(t, ctx, k8s, "recover-i9j0k1", hash, nil)

	var logs bytes.Buffer
	mgr, err := operator.NewManager(restConfig, config.Config{
		SystemNamespace: "default",
	}, slog.New(slog.NewTextHandler(&logs, nil)),
		operator.WithSkipControllerNameValidation(),
		operator.WithProbeAddr(freeAddress(t)),
		operator.WithLeaderElection(false),
	)
	if err != nil {
		t.Fatalf("создать manager: %v", err)
	}
	go func() { _ = operator.Run(ctx, mgr) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache manager не синхронизировался")
	}

	waitForConditionReason(t, ctx, k8s, missing, "CredentialsReady", "SecretNotFound")
	missingSecret := &corev1.Secret{}
	if err := k8s.Get(ctx, client.ObjectKey{
		Namespace: missing.Namespace,
		Name:      valkeyv1alpha1.AuthSecretName(missing.Name),
	}, missingSecret); !apierrors.IsNotFound(err) {
		t.Fatalf("оператор создал отсутствующий Secret: %v", err)
	}

	waitForConditionReason(t, ctx, k8s, noHash, "CredentialsReady", "AppPasswordHashMissing")
	waitForConditionReason(t, ctx, k8s, partial, "CredentialsReady", "ServiceCredentialsPartial")
	partialSecret := &corev1.Secret{}
	if err := k8s.Get(ctx, client.ObjectKey{
		Namespace: partial.Namespace,
		Name:      valkeyv1alpha1.AuthSecretName(partial.Name),
	}, partialSecret); err != nil {
		t.Fatalf("прочитать частичный Secret: %v", err)
	}
	if !bytes.Equal(partialSecret.Data[valkeyv1alpha1.OperatorPasswordKey], partialPassword) ||
		len(partialSecret.Data) != 3 {
		t.Fatalf("частичный комплект был достроен: ключи=%d", len(partialSecret.Data))
	}

	waitForInstanceStatus(t, ctx, k8s, client.ObjectKeyFromObject(recovering), func(
		status valkeyv1alpha1.ValkeyInstanceStatus,
	) bool {
		return status.CredentialsInitialized
	})
	recoveringSecret := &corev1.Secret{}
	recoveringSecretKey := client.ObjectKey{
		Namespace: recovering.Namespace,
		Name:      valkeyv1alpha1.AuthSecretName(recovering.Name),
	}
	if err := k8s.Get(ctx, recoveringSecretKey, recoveringSecret); err != nil {
		t.Fatalf("прочитать инициализированный Secret: %v", err)
	}
	operatorPassword := string(recoveringSecret.Data[valkeyv1alpha1.OperatorPasswordKey])
	if err := k8s.Delete(ctx, recoveringSecret); err != nil {
		t.Fatalf("удалить инициализированный Secret: %v", err)
	}
	waitForConditionReason(t, ctx, k8s, recovering, "RecoveryRequired", "SecretNotFound")

	event := waitForEvent(t, ctx, k8s, recovering.Namespace, recovering.Name, "RecoveryRequired")
	for _, sensitive := range []string{hash, operatorPassword, string(partialPassword)} {
		if strings.Contains(event.Message, sensitive) || strings.Contains(logs.String(), sensitive) {
			t.Fatal("секретное значение попало в Event или лог")
		}
	}
	if !strings.Contains(event.Message, "RECOVERY_REQUIRED") {
		t.Fatalf("Event не содержит код восстановления: %q", event.Message)
	}
}

func waitForConditionReason(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	instance *valkeyv1alpha1.ValkeyInstance,
	conditionType string,
	reason string,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()

	return waitForInstanceStatus(t, ctx, k8s, client.ObjectKeyFromObject(instance), func(
		status valkeyv1alpha1.ValkeyInstanceStatus,
	) bool {
		condition := apimeta.FindStatusCondition(status.Conditions, conditionType)
		return condition != nil && condition.Reason == reason
	})
}

func waitForStatefulSet(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	key client.ObjectKey,
) *appsv1.StatefulSet {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		statefulSet := &appsv1.StatefulSet{}
		if err := k8s.Get(ctx, key, statefulSet); err == nil {
			return statefulSet
		} else if !apierrors.IsNotFound(err) {
			t.Fatalf("прочитать StatefulSet: %v", err)
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("StatefulSet %s не появился", key)
	return nil
}

func waitForPodDisruptionBudget(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	key client.ObjectKey,
) *policyv1.PodDisruptionBudget {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		pdb := &policyv1.PodDisruptionBudget{}
		err := k8s.Get(ctx, key, pdb)
		if err == nil {
			return pdb
		}
		if !apierrors.IsNotFound(err) {
			t.Fatalf("прочитать PodDisruptionBudget: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("PodDisruptionBudget %s не появился", key)

	return nil
}

func waitForSingleResources(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	namespace string,
) (*corev1.ConfigMapList, *corev1.ServiceList, *networkingv1.NetworkPolicyList) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		configMaps := &corev1.ConfigMapList{}
		services := &corev1.ServiceList{}
		policies := &networkingv1.NetworkPolicyList{}
		if err := k8s.List(ctx, configMaps, client.InNamespace(namespace)); err != nil {
			t.Fatalf("прочитать ConfigMap: %v", err)
		}
		if err := k8s.List(ctx, services, client.InNamespace(namespace)); err != nil {
			t.Fatalf("прочитать Services: %v", err)
		}
		if err := k8s.List(ctx, policies, client.InNamespace(namespace)); err != nil {
			t.Fatalf("прочитать NetworkPolicy: %v", err)
		}
		if len(configMaps.Items) == 1 && len(services.Items) == 2 && len(policies.Items) == 1 {
			return configMaps, services, policies
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("дочерние ресурсы в namespace %s не появились", namespace)
	return nil, nil, nil
}

func assertCredentialsPrecedeStatefulSet(
	ctx context.Context,
	t *testing.T,
	k8s client.Client,
	instance *valkeyv1alpha1.ValkeyInstance,
) {
	t.Helper()

	statefulSet := &appsv1.StatefulSet{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), statefulSet); apierrors.IsNotFound(err) {
		return
	} else if err != nil {
		t.Fatalf("прочитать StatefulSet: %v", err)
	}

	observed := &valkeyv1alpha1.ValkeyInstance{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(instance), observed); err != nil {
		t.Fatalf("прочитать ValkeyInstance: %v", err)
	}
	condition := apimeta.FindStatusCondition(observed.Status.Conditions, "CredentialsReady")
	if !observed.Status.CredentialsInitialized || condition == nil ||
		condition.LastTransitionTime.After(statefulSet.CreationTimestamp.Time) {
		t.Fatalf(
			"StatefulSet создан до фиксации credentialsInitialized: condition=%+v created=%s",
			condition,
			statefulSet.CreationTimestamp,
		)
	}
}

func assertOwnedBy(t *testing.T, object client.Object, ownerUID types.UID) {
	t.Helper()

	references := object.GetOwnerReferences()
	if len(references) != 1 || references[0].UID != ownerUID || references[0].Controller == nil ||
		!*references[0].Controller {
		t.Fatalf("%T не принадлежит CR: %v", object, references)
	}
}

func assertResourceVersion(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	object client.Object,
	want string,
) {
	t.Helper()

	fresh, ok := object.DeepCopyObject().(client.Object)
	if !ok {
		t.Fatalf("%T не является client.Object", object)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(object), fresh); err != nil {
		t.Fatalf("прочитать %T: %v", object, err)
	}
	if fresh.GetResourceVersion() != want {
		t.Fatalf(
			"%T изменён при повторном reconcile: resourceVersion %s вместо %s",
			object,
			fresh.GetResourceVersion(),
			want,
		)
	}
}

func waitForEvent(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	namespace string,
	instanceName string,
	reason string,
) corev1.Event {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		events := &corev1.EventList{}
		if err := k8s.List(ctx, events, client.InNamespace(namespace)); err != nil {
			t.Fatalf("прочитать Events: %v", err)
		}
		for _, event := range events.Items {
			if event.InvolvedObject.Name == instanceName && event.Reason == reason {
				return event
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("Event %s для %s не появился", reason, instanceName)
	return corev1.Event{}
}

func prepareInstanceWithSecret(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	slug string,
	hash string,
	serviceData map[string][]byte,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()

	namespace := createInstanceNamespace(t, ctx, k8s, slug)
	data := map[string][]byte{"api-field": []byte("preserved")}
	if hash != "" {
		data["app-password-hash.1"] = []byte(hash)
	}
	for key, value := range serviceData {
		data[key] = value
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      valkeyv1alpha1.AuthSecretName(slug),
			Namespace: namespace.Name,
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if err := k8s.Create(ctx, secret); err != nil {
		t.Fatalf("создать Secret: %v", err)
	}

	instance := newCompleteInstance(slug)
	instance.Namespace = namespace.Name
	if err := k8s.Create(ctx, instance); err != nil {
		t.Fatalf("создать ValkeyInstance: %v", err)
	}

	return instance
}

func encodedPassword(value byte) []byte {
	return []byte(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{value}, 24)))
}

func createCompleteInstance(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	slug string,
	mode valkeyv1alpha1.ValkeyMode,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()

	namespace := createInstanceNamespace(t, ctx, k8s, slug)

	instance := newCompleteInstance(slug)
	instance.Namespace = namespace.Name
	instance.Spec.Mode = mode
	if err := k8s.Create(ctx, instance); err != nil {
		t.Fatalf("создать ValkeyInstance: %v", err)
	}

	return instance
}

func createInstanceNamespace(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	slug string,
) *corev1.Namespace {
	t.Helper()

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "valkey-" + slug,
		Labels: map[string]string{
			"valkey.h3llo-demo.com/instance": slug,
			"valkey.h3llo-demo.com/user-id":  "01991ad0-1234-7000-8000-000000000010",
		},
	}}
	if err := k8s.Create(ctx, namespace); err != nil {
		t.Fatalf("создать namespace: %v", err)
	}

	return namespace
}

func waitForAcceptedConfiguration(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	instance *valkeyv1alpha1.ValkeyInstance,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()

	return waitForInstanceStatus(t, ctx, k8s, client.ObjectKeyFromObject(instance), func(
		status valkeyv1alpha1.ValkeyInstanceStatus,
	) bool {
		return status.AcceptedConfiguration != nil
	})
}

func waitForUnsupportedChange(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	instance *valkeyv1alpha1.ValkeyInstance,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()

	return waitForInstanceStatus(t, ctx, k8s, client.ObjectKeyFromObject(instance), func(
		status valkeyv1alpha1.ValkeyInstanceStatus,
	) bool {
		condition := apimeta.FindStatusCondition(status.Conditions, "UnsupportedChange")
		return condition != nil && condition.Status == metav1.ConditionTrue
	})
}

func waitForInstanceStatus(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	key client.ObjectKey,
	matches func(valkeyv1alpha1.ValkeyInstanceStatus) bool,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		instance := &valkeyv1alpha1.ValkeyInstance{}
		if err := k8s.Get(ctx, key, instance); err != nil {
			t.Fatalf("прочитать ValkeyInstance: %v", err)
		}
		if matches(instance.Status) {
			return instance
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("status ValkeyInstance %s не достиг ожидаемого состояния", key)
	return nil
}

func updateInstance(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	key client.ObjectKey,
	mutate func(*valkeyv1alpha1.ValkeyInstance),
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		instance := &valkeyv1alpha1.ValkeyInstance{}
		if err := k8s.Get(ctx, key, instance); err != nil {
			t.Fatalf("прочитать ValkeyInstance перед update: %v", err)
		}
		mutate(instance)
		if err := k8s.Update(ctx, instance); err == nil {
			return instance
		} else if !apierrors.IsConflict(err) {
			t.Fatalf("обновить ValkeyInstance: %v", err)
		}
	}

	t.Fatalf("обновление ValkeyInstance %s не завершилось", key)
	return nil
}

func updateInstanceStatus(
	t *testing.T,
	ctx context.Context,
	k8s client.Client,
	key client.ObjectKey,
	mutate func(*valkeyv1alpha1.ValkeyInstanceStatus),
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		instance := &valkeyv1alpha1.ValkeyInstance{}
		if err := k8s.Get(ctx, key, instance); err != nil {
			t.Fatalf("прочитать ValkeyInstance перед status update: %v", err)
		}
		mutate(&instance.Status)
		if err := k8s.Status().Update(ctx, instance); err == nil {
			return instance
		} else if !apierrors.IsConflict(err) {
			t.Fatalf("обновить status ValkeyInstance: %v", err)
		}
	}

	t.Fatalf("обновление status ValkeyInstance %s не завершилось", key)
	return nil
}

func assertNoDependents(ctx context.Context, t *testing.T, k8s client.Client, namespace string) {
	t.Helper()

	assertNoStatefulSets(ctx, t, k8s, namespace)

	secrets := &corev1.SecretList{}
	if err := k8s.List(ctx, secrets, client.InNamespace(namespace)); err != nil {
		t.Fatalf("прочитать Secret: %v", err)
	}

	for _, secret := range secrets.Items {
		if secret.Type != corev1.SecretTypeServiceAccountToken {
			t.Errorf("создан Secret %s", secret.Name)
		}
	}

	services := &corev1.ServiceList{}
	if err := k8s.List(ctx, services, client.InNamespace(namespace)); err != nil {
		t.Fatalf("прочитать Service: %v", err)
	}

	for _, service := range services.Items {
		if service.Name != "kubernetes" {
			t.Errorf("создан Service %s", service.Name)
		}
	}
}

func assertNoStatefulSets(ctx context.Context, t *testing.T, k8s client.Client, namespace string) {
	t.Helper()

	statefulSets := &appsv1.StatefulSetList{}
	if err := k8s.List(ctx, statefulSets, client.InNamespace(namespace)); err != nil {
		t.Fatalf("прочитать StatefulSet: %v", err)
	}

	if len(statefulSets.Items) != 0 {
		t.Errorf("создано StatefulSet: %d", len(statefulSets.Items))
	}
}

func startEnvironment(t *testing.T) *rest.Config {
	t.Helper()

	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
		CRDs:                  externalOperatorCRDs(),
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

func externalOperatorCRDs() []*apiextensionsv1.CustomResourceDefinition {
	return []*apiextensionsv1.CustomResourceDefinition{
		externalOperatorCRD("gateway.networking.k8s.io", "v1", "Gateway", "gateways"),
		externalOperatorCRD("gateway.networking.k8s.io", "v1alpha2", "TCPRoute", "tcproutes"),
		externalOperatorCRD("gateway.envoyproxy.io", "v1alpha1", "SecurityPolicy", "securitypolicies"),
		externalOperatorCRD(
			"gateway.envoyproxy.io",
			"v1alpha1",
			"ClientTrafficPolicy",
			"clienttrafficpolicies",
		),
	}
}

func externalOperatorCRD(group, version, kind, plural string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: plural + "." + group,
			Annotations: map[string]string{
				"api-approved.kubernetes.io": "https://github.com/kubernetes-sigs/gateway-api/pull/4530",
			},
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: plural, Singular: strings.ToLower(kind), Kind: kind, ListKind: kind + "List",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: ptr.To(true),
				}},
			}},
		},
	}
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
