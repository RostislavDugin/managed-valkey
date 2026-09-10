package operator

import (
	"testing"

	envoyv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

func Test_NewScheme_WhenCreated_IncludesGatewayAndEnvoyResources(t *testing.T) {
	scheme := NewScheme()
	objects := []runtime.Object{
		&gatewayv1.Gateway{},
		&gatewayv1alpha2.TCPRoute{},
		&envoyv1alpha1.SecurityPolicy{},
		&envoyv1alpha1.ClientTrafficPolicy{},
	}
	for _, object := range objects {
		kinds, _, err := scheme.ObjectKinds(object)
		if err != nil || len(kinds) == 0 {
			t.Errorf("тип %T не зарегистрирован: kinds=%v err=%v", object, kinds, err)
		}
	}
}
