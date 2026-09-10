package operator

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	envoyv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

func NewScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(valkeyv1alpha1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(gatewayv1alpha2.Install(scheme))
	utilruntime.Must(envoyv1alpha1.AddToScheme(scheme))

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
	systemNamespace := cfg.SystemNamespace
	if systemNamespace == "" {
		systemNamespace = config.DefaultSystemNamespace
	}
	probeAddr := cfg.ProbeAddr
	if probeAddr == "" {
		probeAddr = config.DefaultProbeAddr
	}

	managerOptions := ctrl.Options{
		Scheme: NewScheme(),
		Cache: ctrlcache.Options{ByObject: map[client.Object]ctrlcache.ByObject{
			&gatewayv1.Gateway{}: {
				Namespaces: map[string]ctrlcache.Config{systemNamespace: {}},
			},
			&envoyv1alpha1.ClientTrafficPolicy{}: {
				Namespaces: map[string]ctrlcache.Config{systemNamespace: {}},
			},
		}},

		// Снимки инстансов идут в status ресурса; отдельный Prometheus-эндпоинт
		// добавит изменение с поведением оператора.
		Metrics: metricsserver.Options{BindAddress: "0"},

		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          config.LeaderElection,
		LeaderElectionID:        config.LeaderElectionID,
		LeaderElectionNamespace: systemNamespace,
	}

	for _, option := range options {
		option(&managerOptions)
	}

	mgr, err := ctrl.NewManager(restConfig, managerOptions)
	if err != nil {
		return nil, fmt.Errorf("создать manager: %w", err)
	}
	leaseScope := NewLeaseScope()
	if err := mgr.Add(leaseScope); err != nil {
		return nil, fmt.Errorf("добавить lifecycle Lease: %w", err)
	}
	valkeyImage := cfg.ValkeyImage
	if valkeyImage == "" {
		valkeyImage = config.DefaultValkeyImage
	}
	baseDomain := cfg.BaseDomain
	if baseDomain == "" {
		baseDomain = config.DefaultBaseDomain
	}
	envoyProcesses := cfg.EnvoyProcesses
	if envoyProcesses == 0 {
		envoyProcesses = config.DefaultEnvoyProcesses
	}
	if envoyProcesses < 1 || envoyProcesses > config.DefaultEnvoyProcesses {
		return nil, fmt.Errorf("число процессов Envoy должно быть от 1 до %d", config.DefaultEnvoyProcesses)
	}

	reconciler := &ValkeyInstanceReconciler{
		Client:               mgr.GetClient(),
		APIReader:            mgr.GetAPIReader(),
		Scheme:               mgr.GetScheme(),
		Recorder:             mgr.GetEventRecorder("managed-valkey-operator"),
		Lease:                leaseScope,
		SystemNamespace:      systemNamespace,
		ValkeyImage:          valkeyImage,
		BaseDomain:           baseDomain,
		Environment:          cfg.Logging.Environment,
		OperatorCIDRs:        slices.Clone(cfg.OperatorCIDRs),
		Clock:                clock.RealClock{},
		ObservationStartedAt: time.Now().UTC(),
		RESTConfig:           restConfig,
		EnvoyCache:           NewEnvoySnapshotCache(),
		EnvoyProcesses:       envoyProcesses,
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
