package operator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	"github.com/RostislavDugin/managed-valkey/internal/logging"
	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func Test_ValidateTLSCertificate_WithReadyExpiringOrMismatchedCertificate_ReturnsExpectedState(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	valid := testCertificatePEM(t, "*.valkey.localhost", now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := inspectTLSCertificate(valid, "cache-a1b2c3.valkey.localhost", now); err != nil {
		t.Fatalf("действующий сертификат отклонён: %v", err)
	}
	if _, err := inspectTLSCertificate(valid, "cache-a1b2c3.example.com", now); err == nil {
		t.Fatal("сертификат другого домена принят")
	}

	expired := testCertificatePEM(t, "*.valkey.localhost", now.Add(-2*time.Hour), now.Add(-time.Hour))
	if _, err := inspectTLSCertificate(expired, "cache-a1b2c3.valkey.localhost", now); err == nil {
		t.Fatal("истёкший сертификат принят")
	}
}

func Test_ConditionsReady_WithCurrentGeneration_AcceptsOnlyMatchingObservedGeneration(t *testing.T) {
	conditions := []metav1.Condition{
		{Type: "Accepted", Status: metav1.ConditionTrue, ObservedGeneration: 4},
		{Type: "ResolvedRefs", Status: metav1.ConditionTrue, ObservedGeneration: 4},
	}
	if !conditionsCurrent(conditions, 4, "Accepted", "ResolvedRefs") {
		t.Fatal("актуальные conditions отклонены")
	}
	if conditionsCurrent(conditions, 5, "Accepted", "ResolvedRefs") {
		t.Fatal("conditions прежнего поколения приняты")
	}
}

func Test_ReadOnlyRouteReady_WithMissingOrInvalidConditions_AllowsOnlyMissingReadyEndpoints(t *testing.T) {
	namespace := gatewayv1.Namespace("valkey-system")
	section := gatewayv1.SectionName("cache-a1b2c3-ro")
	route := &gatewayv1alpha2.TCPRoute{
		ObjectMeta: metav1.ObjectMeta{Generation: 4},
		Status: gatewayv1alpha2.TCPRouteStatus{
			RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
				ParentRef: gatewayv1.ParentReference{
					Name: gatewayv1.ObjectName(gatewayName), Namespace: &namespace, SectionName: &section,
				},
				Conditions: []metav1.Condition{
					{Type: "Accepted", Status: metav1.ConditionTrue, ObservedGeneration: 4},
					{
						Type: "ResolvedRefs", Status: metav1.ConditionFalse, Reason: "EndpointsNotFound",
						ObservedGeneration: 4,
					},
				},
			}}},
		},
	}
	if !routeAcceptedWithoutReadyEndpoints(route, "valkey-system", "cache-a1b2c3-ro") {
		t.Fatal("принятый маршрут без готовых endpoints отклонён")
	}
	route.Status.Parents[0].Conditions[1].Reason = "BackendNotFound"
	if routeAcceptedWithoutReadyEndpoints(route, "valkey-system", "cache-a1b2c3-ro") {
		t.Fatal("ошибка ссылки backend принята как отсутствие готовых реплик")
	}
	route.Status.Parents[0].Conditions[1].Reason = "EndpointsNotFound"
	route.Status.Parents[0].Conditions[1].ObservedGeneration = 3
	if routeAcceptedWithoutReadyEndpoints(route, "valkey-system", "cache-a1b2c3-ro") {
		t.Fatal("устаревший статус маршрута принят")
	}
}

func Test_ReplicaBackendAddress_WithUnadmittedEarlierReplica_ReturnsReplicaSelectedByService(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.PasswordVersion = 2
	syncedAt := metav1.Now()
	unadmitted := failoverNode(0, valkeyv1alpha1.NodeRoleReplica, "history-a", 10, &syncedAt)
	unadmitted.AppEnabled = false
	unadmitted.AppPasswordVersion = 2
	admitted := failoverNode(2, valkeyv1alpha1.NodeRoleReplica, "history-a", 10, &syncedAt)
	admitted.AppEnabled = true
	admitted.AppPasswordVersion = 2
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{unadmitted, admitted}
	unadmittedPod := testReplicaBackendPod(instance, unadmitted, "10.42.0.10", false)
	admittedPod := testReplicaBackendPod(instance, admitted, "10.42.0.12", true)
	k8s := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(unadmittedPod, admittedPod).Build()

	address, err := (&ValkeyInstanceReconciler{Client: k8s}).replicaBackendAddress(ctx, instance)
	if err != nil || address != admittedPod.Status.PodIP {
		t.Fatalf("выбрать адрес реплики из клиентского сервиса: адрес=%q ошибка=%v", address, err)
	}
}

func Test_NetworkFingerprint_WhenInstanceOrNeighborSettingsChange_ContainsOnlyTargetInstanceSettings(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	first := testNetworkPrerequisites(now)
	first.CertificateHash = "certificate-a"
	first.CIDRs = []string{"198.51.100.0/24", "192.0.2.0/24"}
	second := first
	second.BackendAddress = "10.42.1.99"
	second.CIDRs = []string{"192.0.2.0/24", "198.51.100.0/24"}
	if networkFingerprint(first) != networkFingerprint(second) {
		t.Fatal("IP Pod или порядок CIDR изменил отпечаток настроек инстанса")
	}

	second.CIDRs = []string{"203.0.113.0/24"}
	if networkFingerprint(first) == networkFingerprint(second) {
		t.Fatal("изменение whitelist не изменило отпечаток")
	}
	second = first
	second.CertificateHash = "certificate-b"
	if networkFingerprint(first) == networkFingerprint(second) {
		t.Fatal("изменение сертификата не изменило отпечаток")
	}
	second = first
	readOnly := first
	readOnly.Hostname = "cache-a1b2c3-ro.valkey.localhost"
	readOnly.BackendService = "cache-a1b2c3-replicas"
	second.ReadOnly = &readOnly
	if networkFingerprint(first) == networkFingerprint(second) {
		t.Fatal("добавление маршрута реплик не изменило отпечаток")
	}
}

func Test_ReconcileTCPRoute_WhenServerAddsDefaults_DoesNotPatchRepeatedly(t *testing.T) {
	ctx := context.Background()
	instance := completeAcceptedInstance()
	desired := desiredTCPRoute(instance, "valkey-system", networkEndpoints(instance)[0])
	current := desired.DeepCopy()
	current.Spec.Rules[0].BackendRefs[0].Weight = ptr.To[int32](1)
	scheme := NewScheme()
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(current).Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, Scheme: scheme}

	changed, err := reconciler.ensureTCPRoute(ctx, desired)
	if err != nil || changed {
		t.Fatalf("серверное значение TCPRoute вызвало повторный PATCH: changed=%t error=%v", changed, err)
	}
	observed := &gatewayv1alpha2.TCPRoute{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(current), observed); err != nil ||
		observed.Spec.Rules[0].BackendRefs[0].Weight == nil {
		t.Fatalf("серверное значение TCPRoute потеряно: route=%+v error=%v", observed.Spec, err)
	}
}

func Test_UpdateNetworkVerification_WithoutNewSnapshot_KeepsLastSuccessfulVerification(t *testing.T) {
	verifiedAt := metav1.NewTime(time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC))
	processes := []valkeyv1alpha1.EnvoyProcessStatus{{
		PodUID: "envoy-1", NodeName: "worker-1", NodeUID: "node-1", ContainerID: "containerd://1",
	}, {
		PodUID: "envoy-2", NodeName: "worker-2", NodeUID: "node-2", ContainerID: "containerd://2",
	}}
	instance := &valkeyv1alpha1.ValkeyInstance{ObjectMeta: metav1.ObjectMeta{Generation: 3}}
	status := valkeyv1alpha1.ValkeyInstanceStatus{Network: &valkeyv1alpha1.NetworkStatus{
		VerificationStatus:  valkeyv1alpha1.NetworkVerificationVerified,
		VerifiedAt:          &verifiedAt,
		DesiredFingerprint:  "same",
		VerifiedFingerprint: "same",
		EnvoyProcesses:      processes,
	}}
	now := verifiedAt.Add(time.Minute)
	applyNetworkVerification(
		instance,
		&status,
		envoyVerification{
			status:    valkeyv1alpha1.NetworkVerificationVerified,
			reason:    "EnvoyVerified",
			processes: processes,
		},
		"same",
		"EnvoyVerified",
		"подтверждено",
		now,
	)
	if !status.Network.VerifiedAt.Equal(&verifiedAt) {
		t.Fatalf("переиспользованный снимок изменил verifiedAt: %v", status.Network.VerifiedAt)
	}
	capturedAt := now.Add(time.Minute)
	applyNetworkVerification(
		instance,
		&status,
		envoyVerification{
			status:     valkeyv1alpha1.NetworkVerificationVerified,
			reason:     "EnvoyVerified",
			processes:  processes,
			capturedAt: capturedAt,
		},
		"same",
		"EnvoyVerified",
		"подтверждено",
		capturedAt.Add(time.Minute),
	)
	if !status.Network.VerifiedAt.Time.Equal(capturedAt) {
		t.Fatalf("новый фоновый снимок не изменил verifiedAt: %v", status.Network.VerifiedAt)
	}

	applyNetworkVerification(
		instance,
		&status,
		envoyVerification{
			status: valkeyv1alpha1.NetworkVerificationUnknown,
			reason: "EnvoyAdminUnavailable",
		},
		"same",
		"EnvoyAdminUnavailable",
		"admin API недоступен",
		capturedAt.Add(2*time.Minute),
	)
	if status.Network.VerificationStatus != valkeyv1alpha1.NetworkVerificationUnknown ||
		!status.Network.VerifiedAt.Time.Equal(capturedAt) ||
		status.Network.VerifiedFingerprint != "same" ||
		!sameEnvoyProcesses(status.Network.EnvoyProcesses, processes) {
		t.Fatalf("ошибка Envoy стёрла последнее подтверждение: %+v", status.Network)
	}
	if !networkVerificationAllowsOperations(status.Network, envoyVerification{
		status:    valkeyv1alpha1.NetworkVerificationUnknown,
		reason:    "EnvoyAdminUnavailable",
		processes: processes,
	}, "same") {
		t.Fatal("неизменный проверенный состав заблокирован из-за недоступного admin API")
	}
	status.Initialized = true
	status.Phase = valkeyv1alpha1.InstancePhaseRunning
	coldCache := envoyVerification{
		status: valkeyv1alpha1.NetworkVerificationPending, reason: "EnvoyRefreshPending", processes: processes,
	}
	applyNetworkVerification(
		instance,
		&status,
		coldCache,
		"same",
		coldCache.reason,
		"снимок обновляется",
		capturedAt.Add(3*time.Minute),
	)
	if status.Phase != valkeyv1alpha1.InstancePhaseRunning ||
		!networkVerificationAllowsOperations(status.Network, coldCache, "same") {
		t.Fatalf("пустой кэш Envoy изменил доступность исправного инстанса: %+v", status)
	}
	changedProcesses := slices.Clone(processes)
	changedProcesses[0].ContainerID = "containerd://replacement"
	changedCache := envoyVerification{
		status: valkeyv1alpha1.NetworkVerificationPending,
		reason: "EnvoyRefreshPending", processes: changedProcesses,
	}
	applyNetworkVerification(
		instance,
		&status,
		changedCache,
		"same",
		changedCache.reason,
		"новый процесс проверяется",
		capturedAt.Add(4*time.Minute),
	)
	if status.Phase != valkeyv1alpha1.InstancePhaseRunning {
		t.Fatalf("проверка нового процесса Envoy объявлена подтверждённой потерей: %+v", status)
	}
	if networkVerificationAllowsOperations(status.Network, envoyVerification{
		status:    valkeyv1alpha1.NetworkVerificationUnknown,
		reason:    "EnvoyAdminUnavailable",
		processes: changedProcesses,
	}, "same") {
		t.Fatal("новый состав Envoy допущен по прежнему подтверждению")
	}
	if networkVerificationAllowsOperations(status.Network, envoyVerification{
		status:    valkeyv1alpha1.NetworkVerificationUnknown,
		reason:    "EnvoyFormatUnknown",
		processes: processes,
	}, "same") {
		t.Fatal("неизвестный формат Envoy допущен по прежнему подтверждению")
	}
}

func Test_NetworkVerification_WithOneUnchangedEnvoy_AllowsNonNetworkOperations(t *testing.T) {
	processes := []valkeyv1alpha1.EnvoyProcessStatus{{
		PodUID: "envoy-1", NodeName: "worker-1", NodeUID: "node-1", ContainerID: "containerd://1",
	}}
	status := &valkeyv1alpha1.NetworkStatus{
		VerifiedFingerprint: "same",
		EnvoyProcesses:      processes,
	}
	verification := envoyVerification{
		status:    valkeyv1alpha1.NetworkVerificationUnknown,
		reason:    "EnvoyAdminUnavailable",
		processes: processes,
	}

	if !networkVerificationAllowsOperations(status, verification, "same") {
		t.Fatal("один неизменный процесс Envoy заблокирован из-за недоступного admin API")
	}
	verification.processes = nil
	status.EnvoyProcesses = nil
	if networkVerificationAllowsOperations(status, verification, "same") {
		t.Fatal("пустой состав Envoy допущен по прежнему подтверждению")
	}
}

func Test_CertificateResourceRules_WithDevelopmentAndProduction_UseEnvironmentSpecificRequirements(t *testing.T) {
	ctx := context.Background()
	withoutCertificate := fake.NewClientBuilder().WithScheme(NewScheme()).Build()
	reconciler := &ValkeyInstanceReconciler{
		Client:          withoutCertificate,
		SystemNamespace: "valkey-system",
		Environment:     logging.EnvironmentDev,
	}
	if err := reconciler.validateEnvironmentCertificate(ctx); err != nil {
		t.Fatalf("dev потребовал объект Certificate: %v", err)
	}

	reconciler.Environment = logging.EnvironmentProd
	if err := reconciler.validateEnvironmentCertificate(ctx); err == nil {
		t.Fatal("prod принял отсутствие объекта Certificate")
	}

	certificate := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name": wildcardSecretName, "namespace": "valkey-system", "generation": int64(3),
		},
		"status": map[string]any{"conditions": []any{map[string]any{
			"type": "Ready", "status": "True", "observedGeneration": int64(3),
		}}},
	}}
	certificate.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cert-manager.io", Version: "v1", Kind: "Certificate",
	})
	withCertificate := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(certificate).Build()
	reconciler.Client = withCertificate
	if err := reconciler.validateEnvironmentCertificate(ctx); err != nil {
		t.Fatalf("готовый prod Certificate отклонён: %v", err)
	}
}

func testCertificatePEM(
	t *testing.T,
	dnsName string,
	notBefore time.Time,
	notAfter time.Time,
) []byte {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("создать ключ сертификата: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("подписать сертификат: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: encoded})
}

func testReplicaBackendPod(
	instance *valkeyv1alpha1.ValkeyInstance,
	process valkeyv1alpha1.NodeStatus,
	address string,
	selected bool,
) *corev1.Pod {
	labels := workloadLabels(instance.Status.AcceptedConfiguration.Slug)
	if selected {
		labels[applicationRoleLabel] = string(valkeyv1alpha1.NodeRoleReplica)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", instance.Status.AcceptedConfiguration.Slug, process.Ordinal),
			Namespace: instance.Namespace,
			UID:       types.UID(process.PodUID),
			Labels:    labels,
		},
		Status: corev1.PodStatus{
			PodIP: address,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        "valkey",
				ContainerID: process.ContainerID,
				State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}
