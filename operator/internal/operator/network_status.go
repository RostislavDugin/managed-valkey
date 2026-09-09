package operator

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"time"

	envoyv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	"github.com/RostislavDugin/managed-valkey/internal/logging"
	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

const conditionTypeNetworkReady = "NetworkReady"

var (
	errNetworkObjectNotReady = errors.New("сетевой объект не готов")
	errCertificateInvalid    = errors.New("сертификат недействителен")
)

type networkPrerequisites struct {
	Hostname          string
	BackendNamespace  string
	BackendService    string
	BackendPort       int32
	BackendAddress    string
	WhitelistEnabled  bool
	CIDRs             []string
	CertificateHash   string
	CertificateSecret string
	CertificateSerial string
	CertificateExpiry time.Time
	IdleTimeout       string
}

type networkCheckError struct {
	reason string
	err    error
}

func (e *networkCheckError) Error() string {
	return e.err.Error()
}

func (e *networkCheckError) Unwrap() error {
	return e.err
}

func (r *ValkeyInstanceReconciler) reconcileNetworkPrerequisites(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	expected, checkErr := r.inspectNetworkPrerequisites(ctx, instance)
	desiredFingerprint := ""
	if expected.Hostname != "" {
		desiredFingerprint = networkFingerprint(expected)
	}
	verification := envoyVerification{status: valkeyv1alpha1.NetworkVerificationPending}
	reason := "EnvoyVerificationPending"
	message := "ожидается проверка активной конфигурации Envoy"
	if checkErr != nil {
		reason = networkCheckReason(checkErr)
		message = checkErr.Error()
		if reason == "GatewayNotReady" && instance.Status.Network != nil &&
			instance.Status.Network.VerifiedFingerprint == desiredFingerprint {
			verification = r.verifyEnvoy(ctx, expected)
			reason = verification.reason
			message = "активная конфигурация Envoy ещё не подтверждена"
			if verification.status == valkeyv1alpha1.NetworkVerificationVerified {
				reason = "UnchangedNetworkVerified"
				message = "ранее подтверждённые настройки инстанса активны в текущем составе Envoy"
			}
		}
	} else {
		verification = r.verifyEnvoy(ctx, expected)
		reason = verification.reason
		message = "активная конфигурация Envoy ещё не подтверждена"
		if verification.status == valkeyv1alpha1.NetworkVerificationVerified {
			message = "активная конфигурация подтверждена на двух процессах Envoy"
		}
	}

	_, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		applyNetworkVerification(
			instance,
			status,
			verification,
			desiredFingerprint,
			reason,
			message,
			r.now(),
		)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: config.NetworkVerifyInterval}, nil
}

func applyNetworkVerification(
	instance *valkeyv1alpha1.ValkeyInstance,
	status *valkeyv1alpha1.ValkeyInstanceStatus,
	verification envoyVerification,
	desiredFingerprint string,
	reason string,
	message string,
	now time.Time,
) {
	if status.Network == nil {
		status.Network = &valkeyv1alpha1.NetworkStatus{}
	}
	status.Network.DesiredFingerprint = desiredFingerprint
	status.Network.VerificationStatus = verification.status
	conditionStatus := metav1.ConditionFalse
	switch verification.status {
	case valkeyv1alpha1.NetworkVerificationVerified:
		if verification.fresh || status.Network.VerifiedAt == nil ||
			status.Network.VerifiedFingerprint != desiredFingerprint ||
			!sameEnvoyProcesses(status.Network.EnvoyProcesses, verification.processes) {
			verifiedAt := metav1.NewTime(now)
			status.Network.VerifiedAt = &verifiedAt
			status.Network.VerifiedFingerprint = desiredFingerprint
			status.Network.EnvoyProcesses = slices.Clone(verification.processes)
		}
		conditionStatus = metav1.ConditionTrue
	case valkeyv1alpha1.NetworkVerificationUnknown:
		conditionStatus = metav1.ConditionUnknown
	default:
		if status.Initialized {
			status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
			status.Reason = "NETWORK_NOT_READY"
		}
	}
	setCondition(
		instance,
		status,
		conditionTypeNetworkReady,
		conditionStatus,
		reason,
		message,
	)
}

func (r *ValkeyInstanceReconciler) inspectNetworkPrerequisites(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (networkPrerequisites, error) {
	accepted := instance.Status.AcceptedConfiguration
	hostname := accepted.Slug + "." + r.BaseDomain

	gateway := &gatewayv1.Gateway{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.SystemNamespace, Name: gatewayName}, gateway); err != nil {
		return networkPrerequisites{}, checkError("GatewayUnavailable", "прочитать Gateway", err)
	}
	listenerReady := false
	for _, status := range gateway.Status.Listeners {
		if status.Name == gatewayv1.SectionName(accepted.Slug) &&
			conditionsCurrent(status.Conditions, gateway.Generation, "Accepted", "Programmed") {
			listenerReady = true
			break
		}
	}
	var gatewayErr error
	if !listenerReady {
		gatewayErr = checkError(
			"GatewayNotReady",
			"listener Gateway не принят для текущего поколения",
			errNetworkObjectNotReady,
		)
	}

	route := &gatewayv1alpha2.TCPRoute{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: accepted.Slug}, route); err != nil {
		return networkPrerequisites{}, checkError("TCPRouteUnavailable", "прочитать TCPRoute", err)
	}
	if !routeReady(route, r.SystemNamespace, accepted.Slug) {
		return networkPrerequisites{}, checkError(
			"TCPRouteNotReady",
			"TCPRoute не принят для текущего поколения",
			errNetworkObjectNotReady,
		)
	}

	if accepted.Whitelist.IsEnabled {
		policy := &envoyv1alpha1.SecurityPolicy{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: accepted.Slug}, policy); err != nil {
			return networkPrerequisites{}, checkError("SecurityPolicyUnavailable", "прочитать SecurityPolicy", err)
		}
		if !policyReady(policy.Status, policy.Generation) {
			return networkPrerequisites{}, checkError(
				"SecurityPolicyNotReady",
				"SecurityPolicy не принята для текущего поколения",
				errNetworkObjectNotReady,
			)
		}
	} else {
		policy := &envoyv1alpha1.SecurityPolicy{}
		err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: accepted.Slug}, policy)
		if err == nil || !apierrors.IsNotFound(err) {
			return networkPrerequisites{}, checkError(
				"SecurityPolicyUnexpected",
				"SecurityPolicy присутствует при выключенном whitelist",
				errNetworkObjectNotReady,
			)
		}
	}

	trafficPolicy := &envoyv1alpha1.ClientTrafficPolicy{}
	trafficPolicyKey := client.ObjectKey{Namespace: r.SystemNamespace, Name: "valkey-connections"}
	if err := r.Get(ctx, trafficPolicyKey, trafficPolicy); err != nil {
		return networkPrerequisites{}, checkError(
			"ClientTrafficPolicyUnavailable",
			"прочитать ClientTrafficPolicy",
			err,
		)
	}
	if !clientTrafficPolicyTargetsGateway(trafficPolicy) ||
		trafficPolicy.Spec.Timeout == nil || trafficPolicy.Spec.Timeout.TCP == nil ||
		trafficPolicy.Spec.Timeout.TCP.IdleTimeout == nil ||
		!policyReady(trafficPolicy.Status, trafficPolicy.Generation) {
		return networkPrerequisites{}, checkError(
			"ClientTrafficPolicyNotReady",
			"ClientTrafficPolicy не принята для текущего поколения",
			errNetworkObjectNotReady,
		)
	}

	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{Namespace: r.SystemNamespace, Name: wildcardSecretName}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return networkPrerequisites{}, checkError("CertificateUnavailable", "прочитать TLS Secret", err)
	}
	certificateDetails, err := inspectTLSCertificate(secret.Data[corev1.TLSCertKey], hostname, r.now())
	if err != nil {
		return networkPrerequisites{}, checkError("CertificateInvalid", "проверить TLS-сертификат", err)
	}
	if err := r.validateEnvironmentCertificate(ctx); err != nil {
		return networkPrerequisites{}, err
	}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: accepted.Slug + "-0"}, pod); err != nil {
		return networkPrerequisites{}, checkError("BackendUnavailable", "прочитать Pod backend", err)
	}
	if pod.Status.PodIP == "" {
		return networkPrerequisites{}, checkError(
			"BackendNotReady",
			"Pod backend не получил IP",
			errNetworkObjectNotReady,
		)
	}

	expected := networkPrerequisites{
		Hostname:          hostname,
		BackendNamespace:  instance.Namespace,
		BackendService:    accepted.Slug + "-primary",
		BackendPort:       valkeyPort,
		BackendAddress:    pod.Status.PodIP,
		WhitelistEnabled:  accepted.Whitelist.IsEnabled,
		CIDRs:             slices.Clone(accepted.Whitelist.CIDRs),
		CertificateHash:   certificateDetails.Hash,
		CertificateSecret: r.SystemNamespace + "/" + wildcardSecretName,
		CertificateSerial: certificateDetails.Serial,
		CertificateExpiry: certificateDetails.NotAfter,
		IdleTimeout:       string(*trafficPolicy.Spec.Timeout.TCP.IdleTimeout),
	}
	if gatewayErr != nil {
		return expected, gatewayErr
	}

	return expected, nil
}

func networkFingerprint(expected networkPrerequisites) string {
	cidrs := slices.Clone(expected.CIDRs)
	slices.Sort(cidrs)
	input := struct {
		Hostname         string   `json:"hostname"`
		BackendNamespace string   `json:"backendNamespace"`
		BackendService   string   `json:"backendService"`
		BackendPort      int32    `json:"backendPort"`
		WhitelistEnabled bool     `json:"whitelistEnabled"`
		CIDRs            []string `json:"cidrs"`
		CertificateHash  string   `json:"certificateHash"`
		IdleTimeout      string   `json:"idleTimeout"`
	}{
		Hostname:         expected.Hostname,
		BackendNamespace: expected.BackendNamespace,
		BackendService:   expected.BackendService,
		BackendPort:      expected.BackendPort,
		WhitelistEnabled: expected.WhitelistEnabled,
		CIDRs:            cidrs,
		CertificateHash:  expected.CertificateHash,
		IdleTimeout:      expected.IdleTimeout,
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)

	return hex.EncodeToString(digest[:])
}

func (r *ValkeyInstanceReconciler) validateEnvironmentCertificate(ctx context.Context) error {
	if r.Environment != logging.EnvironmentProd {
		return nil
	}

	return r.validateProductionCertificate(ctx)
}

func (r *ValkeyInstanceReconciler) validateProductionCertificate(ctx context.Context) error {
	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cert-manager.io", Version: "v1", Kind: "Certificate",
	})
	key := client.ObjectKey{Namespace: r.SystemNamespace, Name: wildcardSecretName}
	if err := r.Get(ctx, key, certificate); err != nil {
		return checkError("CertificateResourceUnavailable", "прочитать Certificate", err)
	}
	conditions, found, err := unstructured.NestedSlice(certificate.Object, "status", "conditions")
	if err != nil || !found || !unstructuredConditionCurrent(conditions, certificate.GetGeneration(), "Ready") {
		return checkError(
			"CertificateResourceNotReady",
			"Certificate не готов для текущего поколения",
			errNetworkObjectNotReady,
		)
	}

	return nil
}

func (r *ValkeyInstanceReconciler) now() time.Time {
	if r.Clock == nil {
		return time.Now()
	}

	return r.Clock.Now()
}

type tlsCertificateDetails struct {
	Hash     string
	Serial   string
	NotAfter time.Time
}

func inspectTLSCertificate(
	certificatePEM []byte,
	hostname string,
	now time.Time,
) (tlsCertificateDetails, error) {
	block, _ := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return tlsCertificateDetails{}, errCertificateInvalid
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return tlsCertificateDetails{}, errors.Join(errCertificateInvalid, err)
	}
	if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return tlsCertificateDetails{}, errCertificateInvalid
	}
	if err := certificate.VerifyHostname(hostname); err != nil {
		return tlsCertificateDetails{}, errors.Join(errCertificateInvalid, err)
	}
	digest := sha256.Sum256(certificate.Raw)

	return tlsCertificateDetails{
		Hash: hex.EncodeToString(digest[:]), Serial: certificate.SerialNumber.Text(16), NotAfter: certificate.NotAfter,
	}, nil
}

func routeReady(route *gatewayv1alpha2.TCPRoute, systemNamespace, section string) bool {
	for _, parent := range route.Status.Parents {
		if parent.ParentRef.Name != gatewayv1.ObjectName(gatewayName) ||
			parent.ParentRef.Namespace == nil || string(*parent.ParentRef.Namespace) != systemNamespace ||
			parent.ParentRef.SectionName == nil || string(*parent.ParentRef.SectionName) != section {
			continue
		}

		return conditionsCurrent(parent.Conditions, route.Generation, "Accepted", "ResolvedRefs")
	}

	return false
}

func policyReady(status gatewayv1.PolicyStatus, generation int64) bool {
	for _, ancestor := range status.Ancestors {
		if conditionsCurrent(ancestor.Conditions, generation, "Accepted") {
			return true
		}
	}

	return false
}

func conditionsCurrent(conditions []metav1.Condition, generation int64, required ...string) bool {
	for _, conditionType := range required {
		condition := apimeta.FindStatusCondition(conditions, conditionType)
		if condition == nil || condition.Status != metav1.ConditionTrue ||
			condition.ObservedGeneration != generation {
			return false
		}
	}

	return true
}

func unstructuredConditionCurrent(
	conditions []any,
	generation int64,
	conditionType string,
) bool {
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok || condition["type"] != conditionType || condition["status"] != "True" {
			continue
		}
		observedGeneration, ok := condition["observedGeneration"].(int64)
		return ok && observedGeneration == generation
	}

	return false
}

func clientTrafficPolicyTargetsGateway(policy *envoyv1alpha1.ClientTrafficPolicy) bool {
	for _, target := range policy.Spec.TargetRefs {
		if target.Group == gatewayv1.Group(gatewayv1.GroupName) && target.Kind == "Gateway" &&
			target.Name == gatewayv1.ObjectName(gatewayName) {
			return true
		}
	}

	return false
}

func checkError(reason, message string, err error) error {
	return &networkCheckError{reason: reason, err: fmt.Errorf("%s: %w", message, err)}
}

func networkCheckReason(err error) string {
	check := &networkCheckError{}
	if errors.As(err, &check) {
		return check.reason
	}

	return "NetworkCheckFailed"
}
