//go:build integration

package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

const (
	barrierTimeout   = 2 * time.Minute
	conditionTimeout = 4 * time.Minute
	pollInterval     = 500 * time.Millisecond
)

var createBarrier = newScenarioBarrier(4)

type scenarioHarness struct {
	t          *testing.T
	env        environment
	api        *apiClient
	kubernetes kubernetesReadClient
	processes  processChecker
	startedAt  time.Time
}

type kubernetesReadClient interface {
	Namespace(context.Context, string) (*corev1.Namespace, error)
	Instance(context.Context, string, string) (*valkeyv1alpha1.ValkeyInstance, error)
	Pod(context.Context, string, string) (*corev1.Pod, error)
}

func newScenarioHarness(t *testing.T) *scenarioHarness {
	t.Helper()
	t.Parallel()

	env := mustLoadEnvironment(t)
	kubernetes, err := newKubernetesReader(env.adminKubeconfig)
	if err != nil {
		createBarrier.Cancel(err)
		t.Fatalf("создать Kubernetes-клиент: %v", err)
	}
	harness := &scenarioHarness{
		t:          t,
		env:        env,
		api:        newAPIClient(env.apiURL),
		kubernetes: kubernetes,
		processes: pidProcessChecker{
			apiPID: env.apiProcessID, operatorPID: env.operatorProcessID,
		},
		startedAt: time.Now(),
	}
	t.Cleanup(func() {
		duration := time.Since(harness.startedAt)
		if err := appendDiagnosticLine(
			filepath.Join(env.diagnosticsDir, "durations.tsv"),
			"scenario",
			t.Name(),
			fmt.Sprintf("%.3f", duration.Seconds()),
		); err != nil {
			t.Errorf("сохранить длительность сценария: %v", err)
		}
	})
	t.Cleanup(func() {
		createBarrier.Cancel(errors.New("один из сценариев завершился до общего создания"))
	})

	return harness
}

func (harness *scenarioHarness) registerAccount() account {
	harness.t.Helper()

	authPassword := newSecret()
	harness.recordSecret(authPassword)
	suffix := strings.ReplaceAll(newSecret()[:8], "_", "a")
	email := fmt.Sprintf("integration-%s-%s@example.com", harness.env.runID, suffix)
	ctx, cancel := context.WithTimeout(context.Background(), conditionTimeout)
	defer cancel()
	registered, err := harness.api.Register(ctx, email, authPassword)
	if err != nil {
		createBarrier.Cancel(err)
		harness.t.Fatalf("зарегистрировать аккаунт: %v", err)
	}
	harness.recordSecret(registered.Token)

	return registered
}

func (harness *scenarioHarness) createInstance(owner account, scenario string) (instance, string) {
	harness.t.Helper()

	password := newSecret()
	harness.recordSecret(password)
	ctx, cancel := context.WithTimeout(context.Background(), barrierTimeout)
	defer cancel()
	if err := createBarrier.Wait(ctx); err != nil {
		harness.t.Fatalf("дождаться одновременного создания: %v", err)
	}
	createdAt := time.Now().UTC()
	created, err := harness.api.Create(
		ctx,
		owner.Token,
		scenario+"-"+owner.ID[:8],
		"it"+owner.ID[:8],
		password,
	)
	if err != nil {
		harness.t.Fatalf("создать Valkey: %v", err)
	}
	namespace := "valkey-" + created.Slug
	if err := appendDiagnosticLine(
		filepath.Join(harness.env.diagnosticsDir, "scenarios.tsv"),
		harness.t.Name(),
		owner.ID,
		created.ID,
		namespace,
		createdAt.Format(time.RFC3339Nano),
	); err != nil {
		harness.t.Fatalf("сохранить идентификаторы сценария: %v", err)
	}

	return created, password
}

func (harness *scenarioHarness) cleanupInstance(owner account, instanceID, namespace string) {
	harness.t.Helper()

	harness.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), conditionTimeout)
		defer cancel()
		if err := harness.deleteOwnedInstance(ctx, owner, instanceID, namespace); err != nil {
			harness.t.Errorf("дождаться удаления Valkey при очистке: %v", err)
		}
	})
}

func (harness *scenarioHarness) deleteOwnedInstance(
	ctx context.Context,
	owner account,
	instanceID string,
	namespace string,
) error {
	err := harness.api.Delete(ctx, owner.Token, instanceID)
	var statusError *apiStatusError
	if err != nil && !(errors.As(err, &statusError) && statusError.StatusCode == 404) {
		return fmt.Errorf("запросить удаление Valkey: %w", err)
	}

	return harness.waitForDeletion(ctx, owner, instanceID, namespace)
}

func (harness *scenarioHarness) waitForReady(
	owner account,
	created instance,
	expectedVCPU int,
	expectedRAMGB int,
	expectedPasswordVersion int,
	previousPodUID string,
) (instance, *corev1.Pod) {
	harness.t.Helper()

	httpContext, cancelHTTP := context.WithTimeout(context.Background(), conditionTimeout)
	var current instance
	lastHTTP, err := waitForCondition(
		httpContext,
		harness.processes,
		pollInterval,
		func(ctx context.Context) (string, bool, error) {
			observed, getErr := harness.api.Get(ctx, owner.Token, created.ID)
			if getErr != nil {
				return "HTTP-карточка пока недоступна", false, nil
			}
			current = observed
			encoded, _ := json.Marshal(observed)
			ready := observed.Status == "running" && observed.ObservedAt != nil && !observed.IsStale &&
				!observed.IsUpdating && observed.VCPU == expectedVCPU && observed.RAMGB == expectedRAMGB &&
				observed.AppliedVCPU == expectedVCPU && observed.AppliedRAMGB == expectedRAMGB &&
				observed.DesiredGeneration == observed.ObservedGeneration &&
				observed.PasswordVersion == expectedPasswordVersion &&
				observed.AppliedPasswordVersion == expectedPasswordVersion

			return string(encoded), ready, nil
		},
	)
	cancelHTTP()
	if err != nil {
		harness.saveLastObservation("http", lastHTTP)
		harness.t.Fatalf("API не подтвердил готовое состояние: %v; последнее наблюдение: %s", err, lastHTTP)
	}

	namespace := "valkey-" + created.Slug
	var pod *corev1.Pod
	kubernetesContext, cancelKubernetes := context.WithTimeout(context.Background(), conditionTimeout)
	lastKubernetes, err := waitForCondition(
		kubernetesContext,
		harness.processes,
		pollInterval,
		func(ctx context.Context) (string, bool, error) {
			namespaceObject, namespaceErr := harness.kubernetes.Namespace(ctx, namespace)
			resourceObject, resourceErr := harness.kubernetes.Instance(ctx, namespace, created.Slug)
			podObject, podErr := harness.kubernetes.Pod(ctx, namespace, created.Slug)
			if apierrors.IsNotFound(namespaceErr) || apierrors.IsNotFound(resourceErr) || apierrors.IsNotFound(podErr) {
				return "Kubernetes-ресурсы ещё создаются", false, nil
			}
			if namespaceErr != nil {
				return "не удалось прочитать Namespace", false, namespaceErr
			}
			if resourceErr != nil {
				return "не удалось прочитать ValkeyInstance", false, resourceErr
			}
			if podErr != nil {
				return "не удалось прочитать Pod", false, podErr
			}
			pod = podObject
			observation := fmt.Sprintf(
				"namespace=%s cr_phase=%s observed_generation=%d pod_uid=%s pod_phase=%s",
				namespaceObject.Name,
				resourceObject.Status.Phase,
				resourceObject.Status.ObservedGeneration,
				podObject.UID,
				podObject.Status.Phase,
			)
			ready := kubernetesReady(
				namespaceObject,
				resourceObject,
				podObject,
				owner,
				created,
				expectedVCPU,
				expectedRAMGB,
				expectedPasswordVersion,
				previousPodUID,
			)

			return observation, ready, nil
		},
	)
	cancelKubernetes()
	if err != nil {
		harness.saveLastObservation("kubernetes", lastKubernetes)
		harness.t.Fatalf(
			"Kubernetes не подтвердил готовое состояние: %v; последнее наблюдение: %s",
			err,
			lastKubernetes,
		)
	}

	return current, pod
}

func kubernetesReady(
	namespace *corev1.Namespace,
	resourceObject *valkeyv1alpha1.ValkeyInstance,
	pod *corev1.Pod,
	owner account,
	created instance,
	expectedVCPU int,
	expectedRAMGB int,
	expectedPasswordVersion int,
	previousPodUID string,
) bool {
	if namespace.Labels[valkeyv1alpha1.UserIDLabelKey] != owner.ID ||
		namespace.Labels[valkeyv1alpha1.InstanceIDLabelKey] != created.ID ||
		resourceObject.Spec.InstanceID != created.ID || resourceObject.Spec.Slug != created.Slug ||
		resourceObject.Spec.VCPU != int32(expectedVCPU) || resourceObject.Spec.RAMGB != int32(expectedRAMGB) ||
		resourceObject.Status.Phase != valkeyv1alpha1.InstancePhaseRunning ||
		resourceObject.Status.ObservedGeneration != int64(resourceObject.Spec.DesiredGeneration) ||
		resourceObject.Status.AppliedPasswordVersion != int64(expectedPasswordVersion) ||
		resourceObject.Status.Applied == nil ||
		resourceObject.Status.Applied.VCPU != int32(expectedVCPU) ||
		resourceObject.Status.Applied.RAMGB != int32(expectedRAMGB) ||
		pod.DeletionTimestamp != nil || !podIsReady(pod) || len(pod.Spec.Containers) != 1 {
		return false
	}
	if previousPodUID != "" && string(pod.UID) == previousPodUID {
		return false
	}
	requests := pod.Spec.Containers[0].Resources.Requests
	expectedCPU := resource.NewQuantity(int64(expectedVCPU), resource.DecimalSI)
	expectedMemory := resource.NewQuantity(int64(expectedRAMGB)*(1<<30), resource.BinarySI)

	return requests.Cpu().Cmp(*expectedCPU) == 0 && requests.Memory().Cmp(*expectedMemory) == 0
}

func podIsReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

func (harness *scenarioHarness) connect(hostname, password string) *valkeyConnection {
	harness.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), conditionTimeout)
	defer cancel()
	var connection *valkeyConnection
	last, err := waitForCondition(
		ctx,
		harness.processes,
		pollInterval,
		func(ctx context.Context) (string, bool, error) {
			current, connectErr := openValkeyConnection(
				ctx,
				harness.env.publicAddress,
				hostname,
				harness.env.caFile,
				password,
			)
			if connectErr != nil {
				return fmt.Sprintf("подключение к Valkey пока недоступно: %v", connectErr), false, nil
			}
			if pingErr := current.Ping(ctx); pingErr != nil {
				_ = current.Close()

				return "Valkey пока не отвечает на PING", false, nil
			}
			connection = current

			return "AUTH и PING выполнены", true, nil
		},
	)
	if err != nil {
		harness.saveLastObservation("valkey", last)
		harness.t.Fatalf("Valkey не принял TLS-подключение: %v; последнее наблюдение: %s", err, last)
	}

	return connection
}

func (harness *scenarioHarness) waitForDeletion(
	ctx context.Context,
	owner account,
	instanceID string,
	namespace string,
) error {
	last, err := waitForCondition(
		ctx,
		harness.processes,
		pollInterval,
		func(ctx context.Context) (string, bool, error) {
			_, namespaceErr := harness.kubernetes.Namespace(ctx, namespace)
			namespaceDeleted := apierrors.IsNotFound(namespaceErr)
			if namespaceErr != nil && !namespaceDeleted {
				return "не удалось прочитать Namespace при удалении", false, namespaceErr
			}
			_, getErr := harness.api.Get(ctx, owner.Token, instanceID)
			var statusError *apiStatusError
			apiDeleted := errors.As(getErr, &statusError) && statusError.StatusCode == 404
			items, listErr := harness.api.List(ctx, owner.Token)
			if listErr != nil {
				return "не удалось прочитать список при удалении", false, listErr
			}
			listed := false
			for _, item := range items {
				listed = listed || item.ID == instanceID
			}
			current, meErr := harness.api.Me(ctx, owner.Token)
			if meErr != nil {
				return "не удалось прочитать квоту при удалении", false, meErr
			}
			observation := fmt.Sprintf(
				"namespace_deleted=%t api_deleted=%t listed=%t used_vcpu=%d used_ram_gb=%d",
				namespaceDeleted,
				apiDeleted,
				listed,
				current.Usage.UsedVCPU,
				current.Usage.UsedRAMGB,
			)
			ready := namespaceDeleted && apiDeleted && !listed &&
				current.Usage.UsedVCPU == 0 && current.Usage.UsedRAMGB == 0

			return observation, ready, nil
		},
	)
	if err != nil {
		harness.saveLastObservation("deletion", last)

		return fmt.Errorf("%w; последнее наблюдение: %s", err, last)
	}

	return nil
}

func (harness *scenarioHarness) saveLastObservation(kind, observation string) {
	harness.t.Helper()

	name := strings.NewReplacer("/", "_", " ", "_", "\\", "_").Replace(harness.t.Name())
	path := filepath.Join(harness.env.diagnosticsDir, "last-"+kind+"-"+name+".txt")
	if err := os.WriteFile(path, []byte(observation+"\n"), 0o600); err != nil {
		harness.t.Errorf("сохранить последнее наблюдение: %v", err)
	}
}

func (harness *scenarioHarness) recordSecret(secret string) {
	harness.t.Helper()

	if err := appendDiagnosticLine(harness.env.secretValuesFile, secret); err != nil {
		createBarrier.Cancel(err)
		harness.t.Fatalf("сохранить контрольное значение секрета: %v", err)
	}
}

func newSecret() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

func Test_CreateSingleValkey_WithRealApiAndOperator_BecomesReachableAndRunning(t *testing.T) {
	harness := newScenarioHarness(t)
	owner := harness.registerAccount()
	created, password := harness.createInstance(owner, "create")
	namespace := "valkey-" + created.Slug
	harness.cleanupInstance(owner, created.ID, namespace)
	current, _ := harness.waitForReady(owner, created, 1, 1, 1, "")
	connection := harness.connect(current.Host, password)
	defer func() { _ = connection.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), conditionTimeout)
	defer cancel()
	if err := connection.Set(ctx, "create-key", "create-value"); err != nil {
		t.Fatalf("записать значение: %v", err)
	}
	value, err := connection.Get(ctx, "create-key")
	if err != nil || value != "create-value" {
		t.Fatalf("прочитать значение: value=%q error=%v", value, err)
	}
}

func Test_ResizeSingleValkey_WithRealApiAndOperator_AppliesRequestedResources(t *testing.T) {
	harness := newScenarioHarness(t)
	owner := harness.registerAccount()
	created, password := harness.createInstance(owner, "resize")
	namespace := "valkey-" + created.Slug
	harness.cleanupInstance(owner, created.ID, namespace)
	_, originalPod := harness.waitForReady(owner, created, 1, 1, 1, "")
	ctx, cancel := context.WithTimeout(context.Background(), conditionTimeout)
	defer cancel()
	resized, err := harness.api.Resize(ctx, owner.Token, created.ID, 1, 2)
	if err != nil {
		t.Fatalf("изменить ресурсы: %v", err)
	}
	current, replacementPod := harness.waitForReady(owner, resized, 1, 2, 1, string(originalPod.UID))
	if replacementPod.UID == originalPod.UID {
		t.Fatalf("Pod не заменён: UID=%s", replacementPod.UID)
	}
	connection := harness.connect(current.Host, password)
	defer func() { _ = connection.Close() }()
	if err := connection.Set(ctx, "resize-key", "resize-value"); err != nil {
		t.Fatalf("записать значение после изменения ресурсов: %v", err)
	}
	if value, getErr := connection.Get(ctx, "resize-key"); getErr != nil || value != "resize-value" {
		t.Fatalf("прочитать значение после изменения ресурсов: value=%q error=%v", value, getErr)
	}
}

func Test_RotateSingleValkeyPassword_WithRealApiAndOperator_ReplacesApplicationCredential(t *testing.T) {
	harness := newScenarioHarness(t)
	owner := harness.registerAccount()
	created, password := harness.createInstance(owner, "rotate")
	namespace := "valkey-" + created.Slug
	harness.cleanupInstance(owner, created.ID, namespace)
	current, _ := harness.waitForReady(owner, created, 1, 1, 1, "")
	ctx, cancel := context.WithTimeout(context.Background(), conditionTimeout)
	defer cancel()
	connection := harness.connect(current.Host, password)
	if err := connection.Set(ctx, "rotation-key", "preserved-value"); err != nil {
		_ = connection.Close()
		t.Fatalf("записать значение до ротации: %v", err)
	}
	_ = connection.Close()

	rotatedPassword := newSecret()
	harness.recordSecret(rotatedPassword)
	if _, err := harness.api.RotatePassword(ctx, owner.Token, created.ID, rotatedPassword, 1); err != nil {
		t.Fatalf("запросить ротацию пароля: %v", err)
	}
	current, _ = harness.waitForReady(owner, created, 1, 1, 2, "")
	oldConnection, oldErr := openValkeyConnection(
		ctx,
		harness.env.publicAddress,
		current.Host,
		harness.env.caFile,
		password,
	)
	if oldConnection != nil {
		_ = oldConnection.Close()
	}
	if !errors.Is(oldErr, errValkeyCommandRejected) {
		t.Fatalf("старый пароль не отклонён: %v", oldErr)
	}
	connection = harness.connect(current.Host, rotatedPassword)
	defer func() { _ = connection.Close() }()
	value, err := connection.Get(ctx, "rotation-key")
	if err != nil || value != "preserved-value" {
		t.Fatalf("прочитать значение новым паролем: value=%q error=%v", value, err)
	}
}

func Test_DeleteSingleValkey_WithRealApiAndOperator_RemovesKubernetesResourcesAndApiState(t *testing.T) {
	harness := newScenarioHarness(t)
	owner := harness.registerAccount()
	created, _ := harness.createInstance(owner, "delete")
	namespace := "valkey-" + created.Slug
	harness.waitForReady(owner, created, 1, 1, 1, "")
	ctx, cancel := context.WithTimeout(context.Background(), conditionTimeout)
	defer cancel()
	if err := harness.api.Delete(ctx, owner.Token, created.ID); err != nil {
		t.Fatalf("запросить удаление: %v", err)
	}
	if err := harness.waitForDeletion(ctx, owner, created.ID, namespace); err != nil {
		t.Fatalf("дождаться удаления: %v", err)
	}
}

type deletedNamespaceReader struct {
	deleted string
}

func (reader deletedNamespaceReader) Namespace(
	_ context.Context,
	name string,
) (*corev1.Namespace, error) {
	if name == reader.deleted {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, name)
	}

	return &corev1.Namespace{}, nil
}

func (deletedNamespaceReader) Instance(
	context.Context,
	string,
	string,
) (*valkeyv1alpha1.ValkeyInstance, error) {
	return nil, errors.New("неожиданное чтение ValkeyInstance")
}

func (deletedNamespaceReader) Pod(context.Context, string, string) (*corev1.Pod, error) {
	return nil, errors.New("неожиданное чтение Pod")
}
