package operator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/RostislavDugin/managed-valkey/internal/logging"
	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func TestValidateTLSCertificate(t *testing.T) {
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

func TestConditionsMustMatchCurrentGeneration(t *testing.T) {
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

func TestNetworkFingerprintContainsOnlyInstanceSettings(t *testing.T) {
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
}

func TestNetworkVerificationKeepsLastSuccessWithoutNewSnapshot(t *testing.T) {
	verifiedAt := metav1.NewTime(time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC))
	processes := []valkeyv1alpha1.EnvoyProcessStatus{{
		PodUID: "envoy-1", NodeName: "worker-1", NodeUID: "node-1", ContainerID: "containerd://1",
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
		now,
	)
	if status.Network.VerificationStatus != valkeyv1alpha1.NetworkVerificationUnknown ||
		!status.Network.VerifiedAt.Equal(&verifiedAt) ||
		status.Network.VerifiedFingerprint != "same" ||
		!sameEnvoyProcesses(status.Network.EnvoyProcesses, processes) {
		t.Fatalf("ошибка Envoy стёрла последнее подтверждение: %+v", status.Network)
	}
}

func TestCertificateResourceRulesDifferByEnvironment(t *testing.T) {
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
