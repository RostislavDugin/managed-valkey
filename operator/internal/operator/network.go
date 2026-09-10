package operator

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	envoyv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

const (
	gatewayName        = "valkey"
	wildcardSecretName = "valkey-wildcard-tls"
)

func (r *ValkeyInstanceReconciler) reconcileNetworkResources(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	changed := false
	for _, endpoint := range networkEndpoints(instance) {
		listenerChanged, err := r.reconcileGatewayListener(ctx, instance, endpoint)
		if err != nil {
			return ctrl.Result{}, err
		}
		routeChanged, err := r.ensureTCPRoute(
			ctx,
			desiredTCPRoute(instance, r.SystemNamespace, endpoint),
		)
		if err != nil {
			return ctrl.Result{}, err
		}
		policyChanged, err := r.reconcileSecurityPolicy(ctx, instance, endpoint)
		if err != nil {
			return ctrl.Result{}, err
		}
		changed = changed || listenerChanged || routeChanged || policyChanged
	}

	return requeueIf(changed), nil
}

type networkEndpoint struct {
	suffix        string
	backendSuffix string
}

func networkEndpoints(instance *valkeyv1alpha1.ValkeyInstance) []networkEndpoint {
	result := []networkEndpoint{{backendSuffix: "-primary"}}
	if instance.Status.AcceptedConfiguration != nil &&
		instance.Status.AcceptedConfiguration.Mode == valkeyv1alpha1.ValkeyModeHA {
		result = append(result, networkEndpoint{suffix: "-ro", backendSuffix: "-replicas"})
	}

	return result
}

func (r *ValkeyInstanceReconciler) reconcileGatewayListener(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	endpoint networkEndpoint,
) (bool, error) {
	desired := desiredGatewayListener(instance, r.BaseDomain, endpoint)
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		gateway := &gatewayv1.Gateway{}
		key := client.ObjectKey{Namespace: r.SystemNamespace, Name: gatewayName}
		if err := reader.Get(ctx, key, gateway); err != nil {
			return fmt.Errorf("прочитать общий Gateway: %w", err)
		}

		for index := range gateway.Spec.Listeners {
			if gateway.Spec.Listeners[index].Name != desired.Name {
				continue
			}
			if reflect.DeepEqual(gateway.Spec.Listeners[index], desired) {
				return nil
			}
			gateway.Spec.Listeners[index] = desired
			if err := r.Update(ctx, gateway); err != nil {
				return err
			}
			changed = true

			return nil
		}

		gateway.Spec.Listeners = append(gateway.Spec.Listeners, desired)
		if err := r.Update(ctx, gateway); err != nil {
			return err
		}
		changed = true

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("обновить listener общего Gateway: %w", err)
	}

	return changed, nil
}

func (r *ValkeyInstanceReconciler) removeGatewayListener(
	ctx context.Context,
	slug string,
	suffix string,
) (bool, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		gateway := &gatewayv1.Gateway{}
		key := client.ObjectKey{Namespace: r.SystemNamespace, Name: gatewayName}
		if err := reader.Get(ctx, key, gateway); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		listeners := slices.DeleteFunc(slices.Clone(gateway.Spec.Listeners), func(listener gatewayv1.Listener) bool {
			return listener.Name == gatewayv1.SectionName(slug+suffix)
		})
		if len(listeners) == len(gateway.Spec.Listeners) {
			return nil
		}
		gateway.Spec.Listeners = listeners
		if err := r.Update(ctx, gateway); err != nil {
			return err
		}
		changed = true

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("удалить listener общего Gateway: %w", err)
	}

	return changed, nil
}

func (r *ValkeyInstanceReconciler) removeNetworkResources(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
	slug := instance.Spec.Slug
	changed := false
	endpoints := networkEndpoints(instance)
	objects := make([]client.Object, 0, len(endpoints)*2)
	for _, endpoint := range endpoints {
		listenerChanged, err := r.removeGatewayListener(ctx, slug, endpoint.suffix)
		if err != nil {
			return false, err
		}
		changed = changed || listenerChanged
		name := slug + endpoint.suffix
		objects = append(
			objects,
			&gatewayv1alpha2.TCPRoute{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace}},
			&envoyv1alpha1.SecurityPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace}},
		)
	}
	for _, object := range objects {
		if err := r.Delete(ctx, object); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("удалить сетевой ресурс %s: %w", object.GetName(), err)
		}
		changed = true
	}

	return changed, nil
}

func desiredGatewayListener(
	instance *valkeyv1alpha1.ValkeyInstance,
	baseDomain string,
	endpoint networkEndpoint,
) gatewayv1.Listener {
	accepted := instance.Status.AcceptedConfiguration
	name := accepted.Slug + endpoint.suffix
	hostname := gatewayv1.Hostname(name + "." + baseDomain)
	mode := gatewayv1.TLSModeTerminate
	secretGroup := gatewayv1.Group("")
	secretKind := gatewayv1.Kind("Secret")
	namespacesFrom := gatewayv1.NamespacesFromSelector
	routeGroup := gatewayv1.Group(gatewayv1.GroupName)

	return gatewayv1.Listener{
		Name:     gatewayv1.SectionName(name),
		Hostname: &hostname,
		Port:     accepted.PublicPort,
		Protocol: gatewayv1.TLSProtocolType,
		TLS: &gatewayv1.ListenerTLSConfig{
			Mode: &mode,
			CertificateRefs: []gatewayv1.SecretObjectReference{{
				Group: &secretGroup,
				Kind:  &secretKind,
				Name:  gatewayv1.ObjectName(wildcardSecretName),
			}},
		},
		AllowedRoutes: &gatewayv1.AllowedRoutes{
			Namespaces: &gatewayv1.RouteNamespaces{
				From: &namespacesFrom,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
					instanceLabelKey: accepted.Slug,
				}},
			},
			Kinds: []gatewayv1.RouteGroupKind{{
				Group: &routeGroup,
				Kind:  gatewayv1.Kind("TCPRoute"),
			}},
		},
	}
}

func desiredTCPRoute(
	instance *valkeyv1alpha1.ValkeyInstance,
	systemNamespace string,
	endpoint networkEndpoint,
) *gatewayv1alpha2.TCPRoute {
	accepted := instance.Status.AcceptedConfiguration
	gatewayGroup := gatewayv1.Group(gatewayv1.GroupName)
	gatewayKind := gatewayv1.Kind("Gateway")
	gatewayNamespace := gatewayv1.Namespace(systemNamespace)
	name := accepted.Slug + endpoint.suffix
	sectionName := gatewayv1.SectionName(name)
	serviceGroup := gatewayv1.Group("")
	serviceKind := gatewayv1.Kind("Service")
	servicePort := valkeyPort

	return &gatewayv1alpha2.TCPRoute{
		ObjectMeta: ownedObjectMeta(instance, name, workloadResourceLabels(instance)),
		Spec: gatewayv1alpha2.TCPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
				Group:       &gatewayGroup,
				Kind:        &gatewayKind,
				Namespace:   &gatewayNamespace,
				Name:        gatewayv1.ObjectName(gatewayName),
				SectionName: &sectionName,
			}}},
			Rules: []gatewayv1alpha2.TCPRouteRule{{
				BackendRefs: []gatewayv1.BackendRef{{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Group: &serviceGroup,
						Kind:  &serviceKind,
						Name:  gatewayv1.ObjectName(accepted.Slug + endpoint.backendSuffix),
						Port:  &servicePort,
					},
				}},
			}},
		},
	}
}

func (r *ValkeyInstanceReconciler) ensureTCPRoute(
	ctx context.Context,
	desired *gatewayv1alpha2.TCPRoute,
) (bool, error) {
	r.Scheme.Default(desired)
	current := &gatewayv1alpha2.TCPRoute{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("прочитать TCPRoute: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("создать TCPRoute: %w", err)
		}

		return true, nil
	}

	before := current.DeepCopy()
	if managedMetadataMatches(current, desired) &&
		reflect.DeepEqual(current.OwnerReferences, desired.OwnerReferences) &&
		apiequality.Semantic.DeepDerivative(desired.Spec, current.Spec) {
		return false, nil
	}
	mergeManagedMetadata(current, desired)
	current.OwnerReferences = slices.Clone(desired.OwnerReferences)
	current.Spec = *desired.Spec.DeepCopy()
	if err := r.Patch(ctx, current, client.MergeFrom(before)); err != nil {
		return false, fmt.Errorf("обновить TCPRoute: %w", err)
	}

	return current.ResourceVersion != before.ResourceVersion, nil
}

func (r *ValkeyInstanceReconciler) reconcileSecurityPolicy(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	endpoint networkEndpoint,
) (bool, error) {
	desired := desiredSecurityPolicy(instance, endpoint)
	if desired == nil {
		current := &envoyv1alpha1.SecurityPolicy{}
		key := client.ObjectKey{
			Namespace: instance.Namespace,
			Name:      instance.Status.AcceptedConfiguration.Slug + endpoint.suffix,
		}
		if err := r.Get(ctx, key, current); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}

			return false, fmt.Errorf("прочитать SecurityPolicy: %w", err)
		}
		if err := r.Delete(ctx, current); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("удалить SecurityPolicy: %w", err)
		}

		return true, nil
	}

	current := &envoyv1alpha1.SecurityPolicy{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("прочитать SecurityPolicy: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("создать SecurityPolicy: %w", err)
		}

		return true, nil
	}

	before := current.DeepCopy()
	mergeManagedMetadata(current, desired)
	current.OwnerReferences = slices.Clone(desired.OwnerReferences)
	current.Spec = *desired.Spec.DeepCopy()
	if reflect.DeepEqual(before, current) {
		return false, nil
	}
	if err := r.Patch(ctx, current, client.MergeFrom(before)); err != nil {
		return false, fmt.Errorf("обновить SecurityPolicy: %w", err)
	}

	return current.ResourceVersion != before.ResourceVersion, nil
}

func desiredSecurityPolicy(
	instance *valkeyv1alpha1.ValkeyInstance,
	endpoint networkEndpoint,
) *envoyv1alpha1.SecurityPolicy {
	accepted := instance.Status.AcceptedConfiguration
	if !accepted.Whitelist.IsEnabled {
		return nil
	}

	targetGroup := gatewayv1.Group(gatewayv1.GroupName)
	defaultAction := envoyv1alpha1.AuthorizationActionDeny
	policy := &envoyv1alpha1.SecurityPolicy{
		ObjectMeta: ownedObjectMeta(instance, accepted.Slug+endpoint.suffix, workloadResourceLabels(instance)),
		Spec: envoyv1alpha1.SecurityPolicySpec{
			PolicyTargetReferences: envoyv1alpha1.PolicyTargetReferences{
				TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{{
					LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{
						Group: targetGroup,
						Kind:  gatewayv1.Kind("TCPRoute"),
						Name:  gatewayv1.ObjectName(accepted.Slug + endpoint.suffix),
					},
				}},
			},
			Authorization: &envoyv1alpha1.Authorization{DefaultAction: &defaultAction},
		},
	}
	if len(accepted.Whitelist.CIDRs) == 0 {
		return policy
	}

	ruleName := "allowed-clients"
	cidrs := make([]envoyv1alpha1.CIDR, len(accepted.Whitelist.CIDRs))
	for index, cidr := range accepted.Whitelist.CIDRs {
		cidrs[index] = envoyv1alpha1.CIDR(cidr)
	}
	policy.Spec.Authorization.Rules = []envoyv1alpha1.AuthorizationRule{{
		Name:      &ruleName,
		Action:    envoyv1alpha1.AuthorizationActionAllow,
		Principal: envoyv1alpha1.Principal{ClientCIDRs: cidrs},
	}}

	return policy
}
