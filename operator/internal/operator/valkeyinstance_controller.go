// Package operator работает только с Kubernetes и процессами Valkey:
// PostgreSQL и HTTP API сервиса ему недоступны.
package operator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	envoyv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

type ValkeyInstanceReconciler struct {
	client.Client
	APIReader client.Reader

	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	Lease    *LeaseScope

	SystemNamespace string
	ValkeyImage     string
	BaseDomain      string
	Environment     string
	OperatorCIDRs   []string
	Clock           clock.Clock
	InspectProcess  ProcessInspector
	RESTConfig      *rest.Config
	ReadEnvoy       EnvoySnapshotReader
	EnvoyCache      *EnvoySnapshotCache
	UpdateAppAccess AppAccessUpdater
}

const (
	conditionTypeAccepted          = "Accepted"
	conditionTypeUnsupportedChange = "UnsupportedChange"
)

// Права оператора по разделу 3 SYSTEM.md. Объекты инстанса живут в namespace,
// который создаёт API, поэтому большинству ресурсов нужен ClusterRole.
//
// +kubebuilder:rbac:groups=valkey.h3llo-demo.com,resources=valkeyinstances,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=valkey.h3llo-demo.com,resources=valkeyinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=valkey.h3llo-demo.com,resources=valkeyinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets;configmaps;services,verbs=create;get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=create;get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=create;get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=create;get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch;delete
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tcproutes,verbs=create;get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=securitypolicies,verbs=create;get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list
//
// Права, ограниченные namespace системы и namespace Envoy.
//
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch,namespace=valkey-system
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;update;patch,namespace=valkey-system
// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=clienttrafficpolicies,verbs=get;list;watch,namespace=valkey-system
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=create;get;list;watch;update;patch;delete,namespace=valkey-system
// +kubebuilder:rbac:groups="",resources=pods/portforward,verbs=create,namespace=envoy-gateway-system

func (r *ValkeyInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	instance := &valkeyv1alpha1.ValkeyInstance{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, req.NamespacedName, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("прочитать ValkeyInstance: %w", err)
	}

	logger := logf.FromContext(ctx).WithValues("instance_slug", instance.Spec.Slug)
	ctx = logf.IntoContext(ctx, logger)
	if r.Lease != nil && !r.Lease.Active() {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if !instance.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, instance)
	}

	if instance.Status.AcceptedConfiguration != nil {
		changed, err := r.ensureInstanceFinalizer(ctx, instance)
		if err != nil || changed {
			return requeueIf(changed), err
		}
		changed, err = r.reconcileAcceptedConfiguration(ctx, instance)
		if err != nil || changed {
			return requeueIf(changed), err
		}
		if !instance.Status.Initialized && instance.Status.Phase == valkeyv1alpha1.InstancePhaseError {
			return ctrl.Result{}, nil
		}
		timedOut, err := r.reconcileProvisioningDeadline(ctx, instance)
		if err != nil || timedOut {
			return requeueIf(timedOut), err
		}

		result, err := r.reconcileCredentials(ctx, instance)
		if err != nil || !result.IsZero() || !instance.Status.CredentialsInitialized {
			return result, err
		}

		result, err = r.reconcileConfigMap(ctx, instance)
		if err != nil || !result.IsZero() {
			return result, err
		}

		result, err = r.reconcileSingleResources(ctx, instance)
		if err != nil || !result.IsZero() {
			return result, err
		}

		result, err = r.reconcileProcess(ctx, instance)
		if err != nil || !result.IsZero() {
			return result, err
		}

		result, err = r.reconcilePrimaryLabel(ctx, instance)
		if err != nil || !result.IsZero() {
			return result, err
		}

		result, err = r.reconcileNetworkResources(ctx, instance)
		if err != nil || !result.IsZero() {
			return result, err
		}

		networkResult, err := r.reconcileNetworkPrerequisites(ctx, instance)
		if err != nil || instance.Status.Network == nil ||
			instance.Status.Network.VerificationStatus != valkeyv1alpha1.NetworkVerificationVerified ||
			instance.Status.Network.DesiredFingerprint == "" ||
			instance.Status.Network.DesiredFingerprint != instance.Status.Network.VerifiedFingerprint {
			return networkResult, err
		}

		result, err = r.reconcileAppAdmission(ctx, instance)
		if err != nil || !result.IsZero() {
			return result, err
		}

		return networkResult, nil
	}

	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: instance.Namespace}, namespace); err != nil {
		if apierrors.IsNotFound(err) {
			changed, updateErr := r.setRejectedStatus(ctx, instance, "IdentityMismatch", errInvalidIdentity)
			return requeueIf(changed), updateErr
		}

		return ctrl.Result{}, fmt.Errorf("прочитать namespace инстанса: %w", err)
	}

	accepted, err := acceptedConfiguration(instance, namespace)
	if err != nil {
		changed, updateErr := r.setRejectedStatus(ctx, instance, rejectionReason(err), err)
		return requeueIf(changed), updateErr
	}
	changed, err := r.ensureInstanceFinalizer(ctx, instance)
	if err != nil || changed {
		return requeueIf(changed), err
	}

	changed, err = r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		status.AcceptedConfiguration = accepted
		status.Phase = valkeyv1alpha1.InstancePhaseProvisioning
		status.Reason = ""
		setCondition(
			instance,
			status,
			conditionTypeAccepted,
			metav1.ConditionTrue,
			"Accepted",
			"намерение создания принято",
		)
		apimeta.RemoveStatusCondition(&status.Conditions, conditionTypeUnsupportedChange)
	})

	return requeueIf(changed), err
}

func (r *ValkeyInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyv1alpha1.ValkeyInstance{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&gatewayv1alpha2.TCPRoute{}).
		Owns(&envoyv1alpha1.SecurityPolicy{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretRequests)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(podInstanceRequests)).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(endpointSliceInstanceRequests)).
		Watches(&gatewayv1.Gateway{}, handler.EnqueueRequestsFromMapFunc(r.gatewayRequests)).
		Watches(
			&envoyv1alpha1.ClientTrafficPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.clientTrafficPolicyRequests),
		).
		Named("valkeyinstance").
		Complete(r)
}

func (r *ValkeyInstanceReconciler) reconcileAcceptedConfiguration(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
	differences := configurationDifferences(instance.Spec, *instance.Status.AcceptedConfiguration)

	return r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		if len(differences) == 0 {
			apimeta.RemoveStatusCondition(&status.Conditions, conditionTypeUnsupportedChange)
			return
		}

		setCondition(
			instance,
			status,
			conditionTypeUnsupportedChange,
			metav1.ConditionTrue,
			"SpecChanged",
			"не поддерживается изменение полей: "+strings.Join(differences, ", "),
		)
	})
}

func (r *ValkeyInstanceReconciler) setRejectedStatus(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	reason string,
	cause error,
) (bool, error) {
	return r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		status.Phase = valkeyv1alpha1.InstancePhaseProvisioning
		status.Reason = reason
		setCondition(instance, status, conditionTypeAccepted, metav1.ConditionFalse, reason, cause.Error())
	})
}

func (r *ValkeyInstanceReconciler) updateStatus(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	mutate func(*valkeyv1alpha1.ValkeyInstanceStatus),
) (bool, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	firstAttempt := true
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if !firstAttempt {
			current := &valkeyv1alpha1.ValkeyInstance{}
			if err := reader.Get(ctx, client.ObjectKeyFromObject(instance), current); err != nil {
				return err
			}
			*instance = *current
		}
		firstAttempt = false

		before := instance.DeepCopy()
		mutate(&instance.Status)
		if reflect.DeepEqual(before.Status, instance.Status) {
			return nil
		}
		if err := r.Status().Patch(ctx, instance, client.MergeFrom(before)); err != nil {
			return err
		}
		changed = true

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("обновить status ValkeyInstance: %w", err)
	}

	return changed, nil
}

func (r *ValkeyInstanceReconciler) reconcileProvisioningDeadline(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
	if instance.CreationTimestamp.IsZero() ||
		r.now().Before(instance.CreationTimestamp.Add(config.ProvisionTimeout)) {
		return false, nil
	}

	return r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		status.Phase = valkeyv1alpha1.InstancePhaseError
		status.Reason = "PROVISION_TIMEOUT"
		setCondition(
			instance,
			status,
			conditionTypePublicReady,
			metav1.ConditionFalse,
			"ProvisionTimeout",
			"инстанс не достиг публичной готовности за отведённое время",
		)
	})
}

func setCondition(
	instance *valkeyv1alpha1.ValkeyInstance,
	status *valkeyv1alpha1.ValkeyInstanceStatus,
	conditionType string,
	conditionStatus metav1.ConditionStatus,
	reason string,
	message string,
) {
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		ObservedGeneration: instance.Generation,
		Reason:             reason,
		Message:            message,
	})
}

func rejectionReason(err error) string {
	switch {
	case errors.Is(err, errIncompleteIntent):
		return "MissingRequiredFields"
	case errors.Is(err, errInvalidIdentity):
		return "IdentityMismatch"
	case errors.Is(err, errUnsupportedMode):
		return "UnsupportedMode"
	default:
		return "InvalidSpec"
	}
}

func secretInstanceRequests(_ context.Context, object client.Object) []reconcile.Request {
	if !strings.HasSuffix(object.GetName(), valkeyv1alpha1.AuthSecretSuffix) {
		return nil
	}

	slug := strings.TrimSuffix(object.GetName(), valkeyv1alpha1.AuthSecretSuffix)
	if slug == "" {
		return nil
	}

	return []reconcile.Request{newReconcileRequest(object.GetNamespace(), slug)}
}

func (r *ValkeyInstanceReconciler) secretRequests(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	requests := secretInstanceRequests(ctx, object)
	if len(requests) > 0 {
		return requests
	}
	if object.GetNamespace() == r.SystemNamespace && object.GetName() == wildcardSecretName {
		return r.allInstanceRequests(ctx)
	}

	return nil
}

func (r *ValkeyInstanceReconciler) gatewayRequests(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	if object.GetNamespace() != r.SystemNamespace || object.GetName() != gatewayName {
		return nil
	}

	return r.allInstanceRequests(ctx)
}

func (r *ValkeyInstanceReconciler) clientTrafficPolicyRequests(
	ctx context.Context,
	object client.Object,
) []reconcile.Request {
	if object.GetNamespace() != r.SystemNamespace || object.GetName() != "valkey-connections" {
		return nil
	}

	return r.allInstanceRequests(ctx)
}

func (r *ValkeyInstanceReconciler) allInstanceRequests(ctx context.Context) []reconcile.Request {
	instances := &valkeyv1alpha1.ValkeyInstanceList{}
	if err := r.List(ctx, instances); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(instances.Items))
	for index := range instances.Items {
		requests = append(requests, newReconcileRequest(instances.Items[index].Namespace, instances.Items[index].Name))
	}

	return requests
}

func podInstanceRequests(_ context.Context, object client.Object) []reconcile.Request {
	slug := object.GetLabels()[instanceLabelKey]
	if slug == "" {
		return nil
	}

	return []reconcile.Request{newReconcileRequest(object.GetNamespace(), slug)}
}

func newReconcileRequest(namespace, name string) (request reconcile.Request) {
	request.Namespace = namespace
	request.Name = name

	return request
}

func requeueIf(changed bool) ctrl.Result {
	if changed {
		return ctrl.Result{RequeueAfter: time.Nanosecond}
	}

	return ctrl.Result{}
}
