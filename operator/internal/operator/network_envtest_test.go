//go:build envtest

package operator

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	envoyv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func Test_Envtest_ReconcileNetworkResources_WhenGatewayChangesConcurrently_PreservesOtherListeners(t *testing.T) {
	environment := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{
		networkTestCRD(gatewayv1.GroupName, "v1", "Gateway", "gateways"),
		networkTestCRD(gatewayv1.GroupName, "v1alpha2", "TCPRoute", "tcproutes"),
		networkTestCRD(envoyv1alpha1.GroupName, "v1alpha1", "SecurityPolicy", "securitypolicies"),
	}}
	restConfig, err := environment.Start()
	if err != nil {
		t.Fatalf("запустить envtest сети: %v", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("остановить envtest сети: %v", err)
		}
	})

	k8s, err := client.New(restConfig, client.Options{Scheme: NewScheme()})
	if err != nil {
		t.Fatalf("создать клиент: %v", err)
	}
	ctx := context.Background()
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayName, Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "envoy",
			Listeners: []gatewayv1.Listener{{
				Name: "neighbor", Port: 41379, Protocol: gatewayv1.TLSProtocolType,
			}},
		},
	}
	if err := k8s.Create(ctx, gateway); err != nil {
		t.Fatalf("создать общий Gateway: %v", err)
	}

	allow := networkTestInstance("allow-a1b2c3", true, []string{"192.0.2.0/24"})
	allow.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	denyAll := networkTestInstance("denyall-b2c3d4", true, nil)
	open := networkTestInstance("open-c3d4e5", false, nil)
	for _, instance := range []*valkeyv1alpha1.ValkeyInstance{allow, denyAll, open} {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instance.Namespace}}
		if err := k8s.Create(ctx, namespace); err != nil {
			t.Fatalf("создать namespace %s: %v", namespace.Name, err)
		}
	}
	barrierClient := &gatewayUpdateBarrier{Client: k8s}
	barrierClient.ready.Add(2)

	errors := make(chan error, 2)
	for _, instance := range []*valkeyv1alpha1.ValkeyInstance{allow, denyAll} {
		instance := instance
		go func() {
			reconciler := &ValkeyInstanceReconciler{
				Client:          barrierClient,
				APIReader:       k8s,
				Scheme:          NewScheme(),
				SystemNamespace: "default",
				BaseDomain:      "valkey.localhost",
			}
			_, err := reconciler.reconcileNetworkResources(ctx, instance)
			errors <- err
		}()
	}
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatalf("параллельный reconcile сети: %v", err)
		}
	}

	reconciler := &ValkeyInstanceReconciler{
		Client:          k8s,
		APIReader:       k8s,
		Scheme:          NewScheme(),
		SystemNamespace: "default",
		BaseDomain:      "valkey.localhost",
	}
	if _, err := reconciler.reconcileNetworkResources(ctx, open); err != nil {
		t.Fatalf("reconcile открытой сети: %v", err)
	}

	if err := k8s.Get(ctx, client.ObjectKeyFromObject(gateway), gateway); err != nil {
		t.Fatalf("прочитать Gateway: %v", err)
	}
	wantListeners := []gatewayv1.SectionName{
		"neighbor", "allow-a1b2c3", "allow-a1b2c3-ro", "denyall-b2c3d4", "open-c3d4e5",
	}
	for _, name := range wantListeners {
		if !slices.ContainsFunc(gateway.Spec.Listeners, func(listener gatewayv1.Listener) bool {
			return listener.Name == name
		}) {
			t.Errorf("Gateway потерял listener %s: %+v", name, gateway.Spec.Listeners)
		}
	}
	allowListener := listenerByName(t, gateway.Spec.Listeners, "allow-a1b2c3")
	if allowListener.Hostname == nil || *allowListener.Hostname != "allow-a1b2c3.valkey.localhost" ||
		allowListener.AllowedRoutes == nil || allowListener.AllowedRoutes.Namespaces == nil ||
		allowListener.AllowedRoutes.Namespaces.Selector == nil ||
		allowListener.AllowedRoutes.Namespaces.Selector.MatchLabels[instanceLabelKey] != "allow-a1b2c3" {
		t.Fatalf("listener не изолирует namespace или SNI: %+v", allowListener)
	}

	routes := &gatewayv1alpha2.TCPRouteList{}
	if err := k8s.List(ctx, routes); err != nil {
		t.Fatalf("прочитать TCPRoute: %v", err)
	}
	if len(routes.Items) != 4 {
		t.Fatalf("создано TCPRoute: %d", len(routes.Items))
	}
	for _, route := range routes.Items {
		backendName := route.Name + "-primary"
		if strings.HasSuffix(route.Name, "-ro") {
			backendName = strings.TrimSuffix(route.Name, "-ro") + "-replicas"
		}
		if len(route.Spec.ParentRefs) != 1 || route.Spec.ParentRefs[0].SectionName == nil ||
			string(*route.Spec.ParentRefs[0].SectionName) != route.Name ||
			len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].BackendRefs) != 1 ||
			string(route.Spec.Rules[0].BackendRefs[0].Name) != backendName {
			t.Errorf("неверный TCPRoute %s: %+v", route.Name, route.Spec)
		}
	}

	allowPolicy := &envoyv1alpha1.SecurityPolicy{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(allow), allowPolicy); err != nil {
		t.Fatalf("прочитать allow SecurityPolicy: %v", err)
	}
	if allowPolicy.Spec.Authorization == nil || len(allowPolicy.Spec.Authorization.Rules) != 1 ||
		!slices.Equal(
			allowPolicy.Spec.Authorization.Rules[0].Principal.ClientCIDRs,
			[]envoyv1alpha1.CIDR{"192.0.2.0/24"},
		) {
		t.Fatalf("неверная allow SecurityPolicy: %+v", allowPolicy.Spec)
	}
	allowReadOnlyPolicy := &envoyv1alpha1.SecurityPolicy{}
	if err := k8s.Get(ctx, client.ObjectKey{
		Namespace: allow.Namespace, Name: allow.Name + "-ro",
	}, allowReadOnlyPolicy); err != nil {
		t.Fatalf("прочитать allow SecurityPolicy реплик: %v", err)
	}
	if allowReadOnlyPolicy.Spec.Authorization == nil ||
		len(allowReadOnlyPolicy.Spec.Authorization.Rules) != 1 ||
		!slices.Equal(
			allowReadOnlyPolicy.Spec.Authorization.Rules[0].Principal.ClientCIDRs,
			[]envoyv1alpha1.CIDR{"192.0.2.0/24"},
		) {
		t.Fatalf("неверная allow SecurityPolicy реплик: %+v", allowReadOnlyPolicy.Spec)
	}
	denyPolicy := &envoyv1alpha1.SecurityPolicy{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(denyAll), denyPolicy); err != nil {
		t.Fatalf("прочитать deny-all SecurityPolicy: %v", err)
	}
	if denyPolicy.Spec.Authorization == nil || denyPolicy.Spec.Authorization.DefaultAction == nil ||
		*denyPolicy.Spec.Authorization.DefaultAction != envoyv1alpha1.AuthorizationActionDeny ||
		len(denyPolicy.Spec.Authorization.Rules) != 0 {
		t.Fatalf("неверная deny-all SecurityPolicy: %+v", denyPolicy.Spec)
	}
	openPolicy := &envoyv1alpha1.SecurityPolicy{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(open), openPolicy); err == nil {
		t.Fatalf("для выключенного whitelist создана SecurityPolicy: %+v", openPolicy)
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("прочитать open SecurityPolicy: %v", err)
	}
}

type gatewayUpdateBarrier struct {
	client.Client
	ready sync.WaitGroup
	calls atomic.Int32
}

func (c *gatewayUpdateBarrier) Update(
	ctx context.Context,
	object client.Object,
	options ...client.UpdateOption,
) error {
	if _, ok := object.(*gatewayv1.Gateway); ok && c.calls.Add(1) <= 2 {
		c.ready.Done()
		c.ready.Wait()
	}

	return c.Client.Update(ctx, object, options...)
}

func networkTestCRD(group, version, kind, plural string) *apiextensionsv1.CustomResourceDefinition {
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

func networkTestInstance(
	slug string,
	whitelistEnabled bool,
	cidrs []string,
) *valkeyv1alpha1.ValkeyInstance {
	return &valkeyv1alpha1.ValkeyInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name: slug, Namespace: "valkey-" + slug, UID: types.UID("uid-" + slug),
		},
		Status: valkeyv1alpha1.ValkeyInstanceStatus{AcceptedConfiguration: &valkeyv1alpha1.AcceptedConfiguration{
			Slug: slug, Mode: valkeyv1alpha1.ValkeyModeSingle, PublicPort: 41379,
			Whitelist: valkeyv1alpha1.WhitelistSpec{IsEnabled: whitelistEnabled, CIDRs: cidrs},
		}},
	}
}

func listenerByName(
	t *testing.T,
	listeners []gatewayv1.Listener,
	name gatewayv1.SectionName,
) gatewayv1.Listener {
	t.Helper()
	for _, listener := range listeners {
		if listener.Name == name {
			return listener
		}
	}

	t.Fatal(fmt.Sprintf("listener %s не найден", name))
	return gatewayv1.Listener{}
}
