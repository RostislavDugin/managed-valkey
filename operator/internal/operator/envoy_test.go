package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func Test_ValidateEnvoySnapshot_WithReadyWarmingAndUnknownSnapshots_ReturnsExpectedValidationState(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	expected := testNetworkPrerequisites(now)
	snapshot := testEnvoySnapshot(t, expected, false)
	if err := validateEnvoySnapshot(snapshot, expected, now); err != nil {
		t.Fatalf("готовая конфигурация отклонена: %v", err)
	}

	warming := testEnvoySnapshot(t, expected, true)
	if err := validateEnvoySnapshot(warming, expected, now); !errors.Is(err, errEnvoyPending) {
		t.Fatalf("warming дал неверный результат: %v", err)
	}

	unknown := EnvoyAdminSnapshot{
		ConfigDump: []byte("{\"unexpected\":true}"), Certificates: []byte("{}"),
	}
	if err := validateEnvoySnapshot(unknown, expected, now); !errors.Is(err, errEnvoyUnknown) {
		t.Fatalf("неизвестный формат дал неверный результат: %v", err)
	}
}

func Test_HA04_VerifyEnvoy_WithHAMode_RequiresPrimaryAndReadOnlyRoutes(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	expected := testNetworkPrerequisites(now)
	readOnly := expected
	readOnly.Hostname = "cache-a1b2c3-ro.valkey.localhost"
	readOnly.RouteName = "cache-a1b2c3-ro"
	readOnly.BackendService = "cache-a1b2c3-replicas"
	readOnly.BackendAddress = "10.42.0.11"
	expected.ReadOnly = &readOnly
	snapshot := testEnvoySnapshot(t, expected, false)
	if err := validateEnvoySnapshot(snapshot, expected, now); err != nil {
		t.Fatalf("маршрут primary отсутствует в снимке: %v", err)
	}
	if err := validateEnvoySnapshot(snapshot, readOnly, now); err != nil {
		t.Fatalf("маршрут реплик отсутствует в снимке: %v", err)
	}
	reconciler := &ValkeyInstanceReconciler{
		Client: testEnvoyClient(), SystemNamespace: "valkey-system",
		Clock: clocktesting.NewFakeClock(now),
		ReadEnvoy: func(context.Context, corev1.Pod) (EnvoyAdminSnapshot, error) {
			return snapshot, nil
		},
	}

	result := reconciler.verifyEnvoy(context.Background(), expected)
	if result.status != valkeyv1alpha1.NetworkVerificationVerified {
		t.Fatalf("два маршрута HA не подтверждены: %+v", result)
	}

	broken := expected
	brokenReadOnly := *expected.ReadOnly
	broken.ReadOnly = &brokenReadOnly
	broken.ReadOnly.BackendAddress = "10.42.0.99"
	result = reconciler.verifyEnvoy(context.Background(), broken)
	if result.status != valkeyv1alpha1.NetworkVerificationPending {
		t.Fatalf("неверный endpoint реплик не обнаружен: %+v", result)
	}
}

func Test_HA04_ValidateEnvoySnapshot_WhenReadOnlyReplicaIsNotReady_AcceptsRouteWithoutBackendAddress(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	expected := testNetworkPrerequisites(now)
	readOnly := expected
	readOnly.Hostname = "cache-a1b2c3-ro.valkey.localhost"
	readOnly.RouteName = "cache-a1b2c3-ro"
	readOnly.BackendService = "cache-a1b2c3-replicas"
	readOnly.BackendAddress = ""
	expected.ReadOnly = &readOnly

	snapshot := testEnvoySnapshot(t, expected, false)
	if err := validateEnvoySnapshot(snapshot, readOnly, now); err != nil {
		t.Fatalf("маршрут реплик без готового endpoint отклонён: %v", err)
	}
}

func Test_VerifyEnvoy_WithConfiguredCountUnavailableProcessOrCompositionChange_ReturnsExpectedVerificationState(
	t *testing.T,
) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	expected := testNetworkPrerequisites(now)
	snapshot := testEnvoySnapshot(t, expected, false)

	t.Run("при одном настроенном процессе подтверждает единственный снимок", func(t *testing.T) {
		k8s := testEnvoyClient()
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: envoyNamespace, Name: "envoy-1"}}
		if err := k8s.Delete(context.Background(), pod); err != nil {
			t.Fatalf("удалить второй Pod Envoy: %v", err)
		}
		reconciler := &ValkeyInstanceReconciler{
			Client: k8s, SystemNamespace: "valkey-system", EnvoyProcesses: 1,
			Clock: clocktesting.NewFakeClock(now),
			ReadEnvoy: func(context.Context, corev1.Pod) (EnvoyAdminSnapshot, error) {
				return snapshot, nil
			},
		}
		result := reconciler.verifyEnvoy(context.Background(), expected)
		if result.status != valkeyv1alpha1.NetworkVerificationVerified || len(result.processes) != 1 {
			t.Fatalf("один настроенный процесс Envoy не подтверждён: %+v", result)
		}
	})

	t.Run("при ответе только одного из двух процессов возвращает неизвестное состояние", func(t *testing.T) {
		k8s := testEnvoyClient()
		reconciler := &ValkeyInstanceReconciler{
			Client:          k8s,
			SystemNamespace: "valkey-system",
			ReadEnvoy: func(_ context.Context, pod corev1.Pod) (EnvoyAdminSnapshot, error) {
				if pod.Name == "envoy-1" {
					return EnvoyAdminSnapshot{}, errors.New("недоступен")
				}

				return snapshot, nil
			},
		}
		result := reconciler.verifyEnvoy(context.Background(), expected)
		if result.status != valkeyv1alpha1.NetworkVerificationUnknown ||
			result.reason != "EnvoyAdminUnavailable" {
			t.Fatalf("один ответ Envoy принят: %+v", result)
		}
	})

	t.Run(
		"при смене процесса во время чтения возвращает неизвестное состояние без содержимого снимка",
		func(t *testing.T) {
			k8s := testEnvoyClient()
			var once sync.Once
			reconciler := &ValkeyInstanceReconciler{
				Client:          k8s,
				SystemNamespace: "valkey-system",
				ReadEnvoy: func(ctx context.Context, _ corev1.Pod) (EnvoyAdminSnapshot, error) {
					once.Do(func() {
						pod := &corev1.Pod{}
						key := client.ObjectKey{Namespace: envoyNamespace, Name: "envoy-1"}
						if err := k8s.Get(ctx, key, pod); err != nil {
							t.Fatalf("прочитать меняющийся Pod Envoy: %v", err)
						}
						pod.Status.ContainerStatuses[0].ContainerID = "containerd://changed"
						if err := k8s.Status().Update(ctx, pod); err != nil {
							t.Fatalf("изменить процесс Envoy: %v", err)
						}
					})

					return snapshot, nil
				},
			}
			result := reconciler.verifyEnvoy(context.Background(), expected)
			if result.status != valkeyv1alpha1.NetworkVerificationUnknown ||
				result.reason != "EnvoyCompositionChanged" {
				t.Fatalf("смена процесса Envoy не обнаружена: %+v", result)
			}
			if strings.Contains(fmt.Sprintf("%+v", result), "sensitive-dump-marker") {
				t.Fatal("дамп Envoy попал в результат проверки")
			}
		},
	)
}

func Test_VerifyEnvoy_WithUnchangedProcessesAndRefreshInterval_ReusesValidatedSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	expected := testNetworkPrerequisites(now)
	snapshot := testEnvoySnapshot(t, expected, false)
	k8s := testEnvoyClient()
	clock := clocktesting.NewFakeClock(now)
	var reads atomic.Int32
	reconciler := &ValkeyInstanceReconciler{
		Client:          k8s,
		SystemNamespace: "valkey-system",
		Clock:           clock,
		EnvoyCache:      NewEnvoySnapshotCache(),
		ReadEnvoy: func(context.Context, corev1.Pod) (EnvoyAdminSnapshot, error) {
			reads.Add(1)
			return snapshot, nil
		},
	}

	if result := reconciler.verifyEnvoy(
		context.Background(),
		expected,
	); result.status != valkeyv1alpha1.NetworkVerificationPending {
		t.Fatalf("первое чтение Envoy не вынесено из reconcile: %+v", result)
	}
	waitEnvoyReads(t, &reads, 2)
	waitEnvoyVerified(t, reconciler, expected)
	if result := reconciler.verifyEnvoy(
		context.Background(),
		expected,
	); result.status != valkeyv1alpha1.NetworkVerificationVerified {
		t.Fatalf("сохранённый снимок Envoy не подтверждён: %+v", result)
	}
	reconciler.EnvoyCache.mu.Lock()
	reconciler.EnvoyCache.snapshots[0].ConfigDump = []byte("{")
	reconciler.EnvoyCache.mu.Unlock()
	if result := reconciler.verifyEnvoy(
		context.Background(),
		expected,
	); result.status != valkeyv1alpha1.NetworkVerificationVerified {
		t.Fatalf("проверенный снимок Envoy разобран повторно: %+v", result)
	}
	if reads.Load() != 2 {
		t.Fatalf("один состав Envoy прочитан %d раз вместо 2", reads.Load())
	}

	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: envoyNamespace, Name: "envoy-1"}
	if err := k8s.Get(context.Background(), key, pod); err != nil {
		t.Fatalf("прочитать Pod Envoy: %v", err)
	}
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
	if err := k8s.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("заменить процесс Envoy: %v", err)
	}
	if result := reconciler.verifyEnvoy(
		context.Background(),
		expected,
	); result.status != valkeyv1alpha1.NetworkVerificationPending {
		t.Fatalf("новый состав Envoy не запустил фоновое чтение: %+v", result)
	}
	waitEnvoyReads(t, &reads, 4)
	waitEnvoyVerified(t, reconciler, expected)
	if reads.Load() != 4 {
		t.Fatalf("новый состав Envoy не вызвал чтение двух процессов: %d", reads.Load())
	}

	clock.Step(10 * time.Second)
	if result := reconciler.verifyEnvoy(
		context.Background(),
		expected,
	); result.status != valkeyv1alpha1.NetworkVerificationVerified {
		t.Fatalf("прежний снимок потерян во время обновления: %+v", result)
	}
	waitEnvoyReads(t, &reads, 6)
	if reads.Load() != 6 {
		t.Fatalf("истёкший снимок Envoy не обновлён: %d", reads.Load())
	}
}

func Test_NT06_VerifyEnvoy_WhenBackgroundRefreshBlocks_DoesNotBlockValkeyHeartbeat(t *testing.T) {
	ctx := context.Background()
	instance, pod, node, secret := processObservationObjects()
	pod.Spec.NodeName = "valkey-worker"
	node.Name = pod.Spec.NodeName
	node.UID = types.UID("valkey-node-uid")
	pod.Finalizers = []string{processFinalizer}
	instance.Status.Initialized = true
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{testObservedNode(pod)}
	instance.Status.Nodes[0].NodeUID = string(node.UID)
	previousHeartbeat := metav1.NewTime(time.Date(2026, 9, 9, 11, 59, 0, 0, time.UTC))
	instance.Status.ObservedAt = &previousHeartbeat
	objects := append(testEnvoyObjects(), instance, pod, node, secret)
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}, &corev1.Pod{}).
		WithObjects(objects...).
		Build()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s, SystemNamespace: "valkey-system",
		Clock: clocktesting.NewFakeClock(now), EnvoyCache: NewEnvoySnapshotCache(),
		ReadEnvoy: func(context.Context, corev1.Pod) (EnvoyAdminSnapshot, error) {
			started <- struct{}{}
			<-release
			return EnvoyAdminSnapshot{}, errors.New("недоступен")
		},
		InspectProcess: func(context.Context, string, string, string, bool) (operatorvalkey.ProcessState, error) {
			return operatorvalkey.ProcessState{Role: "primary", RunID: "run-1"}, nil
		},
	}

	begin := time.Now()
	result := reconciler.verifyEnvoy(ctx, testNetworkPrerequisites(now))
	if result.status != valkeyv1alpha1.NetworkVerificationPending ||
		time.Since(begin) >= config.NetworkVerifyTimeout {
		t.Fatalf("фоновая сверка заблокировала reconcile: result=%+v elapsed=%s", result, time.Since(begin))
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("фоновое чтение Envoy не началось")
		}
	}
	processResult, err := reconciler.reconcileProcess(ctx, instance)
	if err != nil || !processResult.IsZero() || instance.Status.ObservedAt == nil ||
		!instance.Status.ObservedAt.Time.Equal(now) {
		t.Fatalf("Envoy задержал heartbeat Valkey: result=%+v status=%+v error=%v", processResult, instance.Status, err)
	}
	close(release)
}

func waitEnvoyReads(t *testing.T, reads *atomic.Int32, expected int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if reads.Load() == expected {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("получено чтений Envoy: %d, ожидалось %d", reads.Load(), expected)
}

func waitEnvoyVerified(
	t *testing.T,
	reconciler *ValkeyInstanceReconciler,
	expected networkPrerequisites,
) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if result := reconciler.verifyEnvoy(
			context.Background(),
			expected,
		); result.status == valkeyv1alpha1.NetworkVerificationVerified {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("фоновый снимок Envoy не стал доступен")
}

func testNetworkPrerequisites(now time.Time) networkPrerequisites {
	return networkPrerequisites{
		Hostname:          "cache-a1b2c3.valkey.localhost",
		RouteName:         "cache-a1b2c3",
		BackendNamespace:  "valkey-cache-a1b2c3",
		BackendService:    "cache-a1b2c3-primary",
		BackendPort:       6379,
		BackendAddress:    "10.42.0.10",
		WhitelistEnabled:  true,
		CIDRs:             []string{"192.0.2.0/24"},
		CertificateSecret: "valkey-system/valkey-wildcard-tls",
		CertificateSerial: "a1b2",
		CertificateExpiry: now.Add(time.Hour),
		IdleTimeout:       "3600s",
	}
}

func testEnvoySnapshot(
	t *testing.T,
	expected networkPrerequisites,
	warming bool,
) EnvoyAdminSnapshot {
	t.Helper()
	configured := []networkPrerequisites{expected}
	if expected.ReadOnly != nil {
		configured = append(configured, *expected.ReadOnly)
	}
	filterChains := make([]any, 0, len(configured))
	endpointConfigs := make([]any, 0, len(configured))
	for _, route := range configured {
		cluster := fmt.Sprintf("tcproute/%s/%s/rule/-1", route.BackendNamespace, route.RouteName)
		filterChains = append(filterChains, testEnvoyFilterChain(route, cluster))
		if route.BackendAddress == "" {
			continue
		}
		endpointConfigs = append(endpointConfigs, map[string]any{
			"endpoint_config": map[string]any{
				"cluster_name": cluster,
				"endpoints": []any{map[string]any{"lb_endpoints": []any{map[string]any{
					"endpoint": map[string]any{"address": map[string]any{
						"socket_address": map[string]any{
							"address": route.BackendAddress, "port_value": route.BackendPort,
						},
					}},
				}}}},
			},
		})
	}
	stateName := "active_state"
	if warming {
		stateName = "warming_state"
	}
	configDump := map[string]any{"configs": []any{
		map[string]any{
			"@type": "type.googleapis.com/envoy.admin.v3.ListenersConfigDump",
			"dynamic_listeners": []any{map[string]any{
				stateName: map[string]any{"listener": map[string]any{
					"filter_chains": filterChains,
				}},
			}},
		},
		map[string]any{
			"@type":                    "type.googleapis.com/envoy.admin.v3.EndpointsConfigDump",
			"dynamic_endpoint_configs": endpointConfigs,
		},
	}}
	certificates := map[string]any{"certificates": []any{map[string]any{
		"cert_chain": []any{map[string]any{
			"path":              "<inline>",
			"serial_number":     "A1:B2",
			"subject_alt_names": []any{map[string]any{"dns": "*.valkey.localhost"}},
			"valid_from":        expected.CertificateExpiry.Add(-2 * time.Hour).Format(time.RFC3339),
			"expiration_time":   expected.CertificateExpiry.Format(time.RFC3339),
		}},
	}}}

	return EnvoyAdminSnapshot{
		ConfigDump:   marshalTestJSON(t, configDump),
		Certificates: marshalTestJSON(t, certificates),
	}
}

func testEnvoyFilterChain(expected networkPrerequisites, cluster string) map[string]any {
	return map[string]any{
		"filter_chain_match": map[string]any{"server_names": []any{expected.Hostname}},
		"transport_socket": map[string]any{"typed_config": map[string]any{
			"common_tls_context": map[string]any{
				"tls_certificate_sds_secret_configs": []any{map[string]any{
					"name": expected.CertificateSecret,
				}},
			},
		}},
		"filters": []any{
			map[string]any{
				"name": "envoy.filters.network.rbac",
				"typed_config": map[string]any{
					"matcher": map[string]any{
						"matcher_list": map[string]any{"matchers": []any{map[string]any{
							"predicate": map[string]any{"custom_match": map[string]any{
								"typed_config": map[string]any{"cidr_ranges": []any{map[string]any{
									"address_prefix": "192.0.2.0",
									"prefix_len":     24,
								}}},
							}},
							"on_match": map[string]any{"action": map[string]any{
								"typed_config": map[string]any{"action": "ALLOW"},
							}},
						}}},
						"on_no_match": map[string]any{"action": map[string]any{
							"typed_config": map[string]any{"action": "DENY"},
						}},
					},
				},
			},
			map[string]any{
				"name": "envoy.filters.network.tcp_proxy",
				"typed_config": map[string]any{
					"cluster": cluster, "idle_timeout": expected.IdleTimeout,
				},
			},
		},
		"sensitive-dump-marker": "must-not-escape",
	}
}

func marshalTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("сформировать снимок Envoy: %v", err)
	}

	return data
}

func testEnvoyClient() client.Client {
	return fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&corev1.Pod{}).
		WithObjects(testEnvoyObjects()...).
		Build()
}

func testEnvoyObjects() []client.Object {
	objects := make([]client.Object, 0, 4)
	for index := range 2 {
		nodeName := fmt.Sprintf("worker-%d", index)
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: nodeName, UID: types.UID("node-uid-" + nodeName),
		}}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("envoy-%d", index),
				Namespace: envoyNamespace,
				UID:       types.UID(fmt.Sprintf("pod-uid-%d", index)),
				Labels: map[string]string{
					"gateway.envoyproxy.io/owning-gateway-name":      gatewayName,
					"gateway.envoyproxy.io/owning-gateway-namespace": "valkey-system",
				},
			},
			Spec: corev1.PodSpec{NodeName: nodeName},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:        "envoy",
				ContainerID: fmt.Sprintf("containerd://%d", index),
				State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}},
		}
		objects = append(objects, node, pod)
	}

	return objects
}
