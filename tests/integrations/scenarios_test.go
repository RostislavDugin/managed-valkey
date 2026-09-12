//go:build integration

package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

const (
	barrierTimeout   = 2 * time.Minute
	conditionTimeout = 4 * time.Minute
	pollInterval     = 500 * time.Millisecond
)

var createBarrier = newScenarioBarrier(createBarrierSize())

func createBarrierSize() int {
	if os.Getenv("MV_INTEGRATION_TEST") != "" {
		return 1
	}

	return 4
}

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

func (harness *scenarioHarness) createInstance(
	owner account,
	scenario string,
	maintenance *maintenanceWindow,
) (instance, string) {
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
		maintenance,
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
				"namespace=%s namespace_metadata=%t cr_phase=%s cr_metadata=%t "+
					"observed_generation=%d pod_uid=%s pod_phase=%s pod_metadata=%t",
				namespaceObject.Name,
				ownerMetadataMatches(namespaceObject, owner, created),
				resourceObject.Status.Phase,
				ownerMetadataMatches(resourceObject, owner, created),
				resourceObject.Status.ObservedGeneration,
				podObject.UID,
				podObject.Status.Phase,
				podOwnerMetadataMatches(podObject, owner, created),
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
	if !ownerMetadataMatches(namespace, owner, created) ||
		!ownerMetadataMatches(resourceObject, owner, created) ||
		!podOwnerMetadataMatches(pod, owner, created) ||
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

func ownerMetadataMatches(object metav1.Object, owner account, created instance) bool {
	return object.GetLabels()[valkeyv1alpha1.InstanceIDLabelKey] == created.ID &&
		object.GetLabels()[valkeyv1alpha1.UserIDLabelKey] == owner.ID &&
		object.GetAnnotations()[valkeyv1alpha1.UserEmailAnnotationKey] == owner.Email
}

func podOwnerMetadataMatches(pod *corev1.Pod, owner account, created instance) bool {
	return ownerMetadataMatches(pod, owner, created) &&
		pod.Labels[valkeyv1alpha1.RoleLabelKey] == string(valkeyv1alpha1.NodeRolePrimary)
}

func podIsReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}

	return false
}

func (harness *scenarioHarness) waitForMetrics(owner account, created instance) metricPoint {
	harness.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), conditionTimeout)
	defer cancel()
	var point metricPoint
	last, err := waitForCondition(
		ctx,
		harness.processes,
		pollInterval,
		func(ctx context.Context) (string, bool, error) {
			metrics, metricsErr := harness.api.Metrics(ctx, owner.Token, created.ID)
			if metricsErr != nil {
				return fmt.Sprintf("метрики через HTTP пока недоступны: %v", metricsErr), false, nil
			}
			encoded, _ := json.Marshal(metrics)
			current, found := currentMetricPoint(metrics, created)
			if found {
				point = current
			}

			return string(encoded), found, nil
		},
	)
	if err != nil {
		harness.saveLastObservation("metrics", last)
		harness.t.Fatalf("API не вернул полную точку метрик: %v; последнее наблюдение: %s", err, last)
	}

	return point
}

func currentMetricPoint(metrics valkeyMetrics, created instance) (metricPoint, bool) {
	for _, node := range metrics.Nodes {
		if node.Ordinal != 0 || node.Name != created.Slug+"-0" ||
			node.Role != string(valkeyv1alpha1.NodeRolePrimary) {
			continue
		}
		for _, point := range node.Points {
			if metricPointComplete(point) {
				return point, true
			}
		}
	}

	return metricPoint{}, false
}

func metricPointComplete(point metricPoint) bool {
	return point.UsedMemoryBytes != nil && *point.UsedMemoryBytes >= 0 &&
		point.CPUMillicores != nil && *point.CPUMillicores >= 0 &&
		point.ConnectedClients != nil && *point.ConnectedClients >= 0 &&
		point.OpsPerSec != nil && *point.OpsPerSec >= 0 &&
		point.KeyspaceHits != nil && *point.KeyspaceHits >= 0 &&
		point.KeyspaceMisses != nil && *point.KeyspaceMisses >= 0 &&
		point.EvictedKeys != nil && *point.EvictedKeys >= 0
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
	maintenance := maintenanceWindow{DOW: 2, HourUTC: 3, DurationMin: 60}
	created, password := harness.createInstance(owner, "create", &maintenance)
	if created.Host == "" || created.HostRO == "" || created.Host == created.HostRO ||
		created.Maintenance == nil || *created.Maintenance != maintenance {
		t.Fatalf("POST вернул неполные адреса или окно обслуживания: %+v", created)
	}
	namespace := "valkey-" + created.Slug
	harness.cleanupInstance(owner, created.ID, namespace)
	current, _ := harness.waitForReady(owner, created, 1, 1, 1, "")
	if current.Host != created.Host || current.HostRO != created.HostRO ||
		current.Maintenance == nil || *current.Maintenance != maintenance {
		t.Fatalf("GET изменил адреса или окно обслуживания: created=%+v current=%+v", created, current)
	}
	ctx, cancel := context.WithTimeout(context.Background(), conditionTimeout)
	defer cancel()
	currentCredentials, err := harness.api.Credentials(ctx, owner.Token, created.ID)
	if err != nil || currentCredentials.Host != current.Host || currentCredentials.HostRO != current.HostRO {
		t.Fatalf("credentials не вернул оба адреса: credentials=%+v error=%v", currentCredentials, err)
	}
	platformHealthContext, cancelPlatformHealth := context.WithTimeout(context.Background(), conditionTimeout)
	lastPlatformHealth, err := waitForCondition(
		platformHealthContext,
		harness.processes,
		pollInterval,
		func(ctx context.Context) (string, bool, error) {
			observed, healthErr := harness.api.Health(ctx)
			if healthErr != nil {
				return healthErr.Error(), false, nil
			}
			encoded, _ := json.Marshal(observed)
			healthy := observed.Status == "ok" &&
				observed.Checks.PostgreSQL.Status == "ok" &&
				observed.Checks.Kubernetes.Status == "ok" &&
				observed.Checks.Operations.Status == "ok" &&
				observed.Checks.Instances.Status == "ok"

			return string(encoded), healthy, nil
		},
	)
	cancelPlatformHealth()
	if err != nil {
		harness.saveLastObservation("platform-health", lastPlatformHealth)
		t.Fatalf(
			"/health не подтвердил исправное состояние платформы: %v; последнее наблюдение: %s",
			err,
			lastPlatformHealth,
		)
	}
	valkeyStatus, err := harness.api.ValkeyHealth(
		ctx,
		valkeyHealthURI(current.Host, current.Port, password),
		valkeyHealthURI(current.HostRO, current.Port, password),
	)
	if err != nil || valkeyStatus.Status != "ok" || valkeyStatus.Checks.Primary.Status != "ok" ||
		valkeyStatus.Checks.Read == nil || valkeyStatus.Checks.Read.Status != "ok" {
		t.Fatalf("/valkey-health не подтвердил primary и read: health=%+v error=%v", valkeyStatus, err)
	}
	harness.waitForMetrics(owner, created)
	primaryConnection := harness.connect(current.Host, password)
	defer func() { _ = primaryConnection.Close() }()
	readConnection := harness.connect(current.HostRO, password)
	defer func() { _ = readConnection.Close() }()
	if err := primaryConnection.Set(ctx, "create-key", "create-value"); err != nil {
		t.Fatalf("записать значение: %v", err)
	}
	value, err := readConnection.Get(ctx, "create-key")
	if err != nil || value != "create-value" {
		t.Fatalf("прочитать через адрес для чтения: value=%q error=%v", value, err)
	}
}

func valkeyHealthURI(host string, port int, password string) string {
	return (&url.URL{
		Scheme: "rediss",
		User:   url.UserPassword("app", password),
		Host:   net.JoinHostPort(host, fmt.Sprintf("%d", port)),
	}).String()
}

func Test_ResizeSingleValkey_WithRealApiAndOperator_AppliesRequestedResources(t *testing.T) {
	harness := newScenarioHarness(t)
	owner := harness.registerAccount()
	created, password := harness.createInstance(owner, "resize", nil)
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
	created, password := harness.createInstance(owner, "rotate", nil)
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
	created, _ := harness.createInstance(owner, "delete", nil)
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

func Test_KubernetesReady_WithCompleteOwnerMetadata_ReturnsTrue(t *testing.T) {
	namespace, resourceObject, pod, owner, created := readyKubernetesObjects()
	if !kubernetesReady(namespace, resourceObject, pod, owner, created, 1, 1, 1, "") {
		t.Fatal("полный набор метаданных не признан готовым")
	}
}

func Test_KubernetesReady_WithIncompleteOwnerMetadata_ReturnsFalse(t *testing.T) {
	tests := []struct {
		name   string
		change func(*corev1.Namespace, *valkeyv1alpha1.ValkeyInstance, *corev1.Pod)
	}{
		{
			name: "у Namespace нет адреса электронной почты",
			change: func(namespace *corev1.Namespace, _ *valkeyv1alpha1.ValkeyInstance, _ *corev1.Pod) {
				delete(namespace.Annotations, valkeyv1alpha1.UserEmailAnnotationKey)
			},
		},
		{
			name: "у ValkeyInstance нет идентификатора экземпляра",
			change: func(_ *corev1.Namespace, resourceObject *valkeyv1alpha1.ValkeyInstance, _ *corev1.Pod) {
				delete(resourceObject.Labels, valkeyv1alpha1.InstanceIDLabelKey)
			},
		},
		{
			name: "у Pod нет идентификатора пользователя",
			change: func(_ *corev1.Namespace, _ *valkeyv1alpha1.ValkeyInstance, pod *corev1.Pod) {
				delete(pod.Labels, valkeyv1alpha1.UserIDLabelKey)
			},
		},
		{
			name: "у Pod нет подтверждённой роли",
			change: func(_ *corev1.Namespace, _ *valkeyv1alpha1.ValkeyInstance, pod *corev1.Pod) {
				delete(pod.Labels, valkeyv1alpha1.RoleLabelKey)
			},
		},
		{
			name: "у Pod нет адреса электронной почты",
			change: func(_ *corev1.Namespace, _ *valkeyv1alpha1.ValkeyInstance, pod *corev1.Pod) {
				delete(pod.Annotations, valkeyv1alpha1.UserEmailAnnotationKey)
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			namespace, resourceObject, pod, owner, created := readyKubernetesObjects()
			testCase.change(namespace, resourceObject, pod)
			if kubernetesReady(namespace, resourceObject, pod, owner, created, 1, 1, 1, "") {
				t.Fatal("неполный набор метаданных признан готовым")
			}
		})
	}
}

func Test_CurrentMetricPoint_WithEmptyAndCompleteIntervals_ReturnsOnlyCompletePoint(t *testing.T) {
	created := instance{Slug: "cache-a1b2c3"}
	complete := completeMetricPoint()
	withoutCPU := completeMetricPoint()
	withoutCPU.CPUMillicores = nil
	negative := completeMetricPoint()
	negative.EvictedKeys = metricInt64(-1)
	tests := []struct {
		name    string
		metrics valkeyMetrics
		found   bool
	}{
		{
			name: "пустой ряд",
			metrics: valkeyMetrics{Nodes: []metricNode{{
				Ordinal: 0, Name: created.Slug + "-0", Role: "primary", Points: []metricPoint{{}},
			}}},
		},
		{
			name: "заполненная точка после пустого интервала",
			metrics: valkeyMetrics{Nodes: []metricNode{{
				Ordinal: 0, Name: created.Slug + "-0", Role: "primary", Points: []metricPoint{{}, complete},
			}}},
			found: true,
		},
		{
			name: "точка без загрузки CPU",
			metrics: valkeyMetrics{Nodes: []metricNode{{
				Ordinal: 0, Name: created.Slug + "-0", Role: "primary", Points: []metricPoint{withoutCPU},
			}}},
		},
		{
			name: "точка с отрицательным показателем",
			metrics: valkeyMetrics{Nodes: []metricNode{{
				Ordinal: 0, Name: created.Slug + "-0", Role: "primary", Points: []metricPoint{negative},
			}}},
		},
		{
			name: "точка другого ряда",
			metrics: valkeyMetrics{Nodes: []metricNode{{
				Ordinal: 0, Name: created.Slug + "-0", Role: "replica", Points: []metricPoint{complete},
			}}},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			point, found := currentMetricPoint(testCase.metrics, created)
			if found != testCase.found {
				t.Fatalf("поиск точки: found=%t point=%+v", found, point)
			}
		})
	}
}

func completeMetricPoint() metricPoint {
	return metricPoint{
		UsedMemoryBytes:  metricFloat64(1024),
		CPUMillicores:    metricFloat64(5),
		ConnectedClients: metricFloat64(1),
		OpsPerSec:        metricFloat64(2),
		KeyspaceHits:     metricInt64(3),
		KeyspaceMisses:   metricInt64(4),
		EvictedKeys:      metricInt64(0),
	}
}

func metricFloat64(value float64) *float64 {
	return &value
}

func metricInt64(value int64) *int64 {
	return &value
}

func readyKubernetesObjects() (
	*corev1.Namespace,
	*valkeyv1alpha1.ValkeyInstance,
	*corev1.Pod,
	account,
	instance,
) {
	owner := account{ID: "01991ad0-1234-7000-8000-000000000010", Email: "owner@example.com"}
	created := instance{ID: "01991ad0-1234-7000-8000-000000000020", Slug: "cache-a1b2c3"}
	metadata := metav1.ObjectMeta{
		Labels: map[string]string{
			valkeyv1alpha1.InstanceIDLabelKey: created.ID,
			valkeyv1alpha1.UserIDLabelKey:     owner.ID,
		},
		Annotations: map[string]string{valkeyv1alpha1.UserEmailAnnotationKey: owner.Email},
	}
	namespace := &corev1.Namespace{ObjectMeta: *metadata.DeepCopy()}
	resourceObject := &valkeyv1alpha1.ValkeyInstance{
		ObjectMeta: *metadata.DeepCopy(),
		Spec: valkeyv1alpha1.ValkeyInstanceSpec{
			InstanceID: created.ID, Slug: created.Slug, VCPU: 1, RAMGB: 1, DesiredGeneration: 1,
		},
		Status: valkeyv1alpha1.ValkeyInstanceStatus{
			Phase:                  valkeyv1alpha1.InstancePhaseRunning,
			ObservedGeneration:     1,
			AppliedPasswordVersion: 1,
			Applied:                &valkeyv1alpha1.AppliedConfiguration{VCPU: 1, RAMGB: 1},
		},
	}
	podMetadata := *metadata.DeepCopy()
	podMetadata.Labels[valkeyv1alpha1.RoleLabelKey] = string(valkeyv1alpha1.NodeRolePrimary)
	pod := &corev1.Pod{
		ObjectMeta: podMetadata,
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
			}},
		}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}},
		},
	}

	return namespace, resourceObject, pod, owner, created
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
