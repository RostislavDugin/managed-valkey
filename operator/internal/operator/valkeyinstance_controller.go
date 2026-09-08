// Package operator работает только с Kubernetes и процессами Valkey:
// PostgreSQL и HTTP API сервиса ему недоступны.
package operator

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

type ValkeyInstanceReconciler struct {
	client.Client

	Scheme *runtime.Scheme
}

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

// Управление StatefulSet, Secret, сетью и failover добавит отдельное изменение
// по разделам 6 и 15 SYSTEM.md. До тех пор развёрнутый оператор проверяет
// только запуск процесса.
func (r *ValkeyInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logf.FromContext(ctx).V(1).Info("событие ресурса получено, управление ещё не реализовано",
		"instance_slug", req.Name,
		"namespace", req.Namespace,
	)

	return ctrl.Result{}, nil
}

func (r *ValkeyInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyv1alpha1.ValkeyInstance{}).
		Named("valkeyinstance").
		Complete(r)
}
