package operator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

func NewScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(valkeyv1alpha1.AddToScheme(scheme))

	return scheme
}

type Option func(*ctrl.Options)

// Имена контроллеров проверяются на весь процесс, поэтому снимать проверку
// приходится тестам, которые поднимают несколько manager подряд.
func WithSkipControllerNameValidation() Option {
	return func(options *ctrl.Options) {
		options.Controller.SkipNameValidation = new(bool)
		*options.Controller.SkipNameValidation = true
	}
}

func WithProbeAddr(addr string) Option {
	return func(options *ctrl.Options) {
		options.HealthProbeBindAddress = addr
	}
}

func WithLeaderElection(enabled bool) Option {
	return func(options *ctrl.Options) {
		options.LeaderElection = enabled
	}
}

func NewManager(
	restConfig *rest.Config,
	cfg config.Config,
	logger *slog.Logger,
	options ...Option,
) (ctrl.Manager, error) {
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	managerOptions := ctrl.Options{
		Scheme: NewScheme(),

		// Снимки инстансов идут в status ресурса; отдельный Prometheus-эндпоинт
		// добавит изменение с поведением оператора.
		Metrics: metricsserver.Options{BindAddress: "0"},

		HealthProbeBindAddress:  config.ProbeAddr,
		LeaderElection:          config.LeaderElection,
		LeaderElectionID:        config.LeaderElectionID,
		LeaderElectionNamespace: cfg.SystemNamespace,
	}

	for _, option := range options {
		option(&managerOptions)
	}

	mgr, err := ctrl.NewManager(restConfig, managerOptions)
	if err != nil {
		return nil, fmt.Errorf("создать manager: %w", err)
	}

	reconciler := &ValkeyInstanceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("зарегистрировать контроллер ValkeyInstance: %w", err)
	}

	// Готовность это запущенный manager с синхронизированным cache: фоновых
	// циклов, которые могли бы её задерживать, пока нет.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("добавить проверку живого процесса: %w", err)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("добавить проверку готовности: %w", err)
	}

	return mgr, nil
}

func Run(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("работа manager: %w", err)
	}

	return nil
}
