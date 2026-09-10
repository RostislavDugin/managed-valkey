package operator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

const maxMetricNodes = 3

type MetricsSession interface {
	ReadMetrics(context.Context, string) (operatorvalkey.Metrics, error)
	VerifyRunID(context.Context, string) error
	Close()
}

type MetricsSessionFactory func(context.Context, string, string) (MetricsSession, error)

type ValkeyMetricsReconciler struct {
	client.Client
	APIReader   client.Reader
	Lease       *LeaseScope
	PodMetrics  PodMetricsReader
	Clock       clock.Clock
	Interval    time.Duration
	OpenSession MetricsSessionFactory
}

type collectedNodeMetric struct {
	metric valkeyv1alpha1.NodeMetricStatus
	err    error
}

func (r *ValkeyMetricsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	interval := r.Interval
	if interval <= 0 {
		interval = config.MetricsInterval
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	instance := &valkeyv1alpha1.ValkeyInstance{}
	if err := reader.Get(ctx, req.NamespacedName, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("прочитать ValkeyInstance для метрик: %w", err)
	}
	if !instance.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if r.Lease != nil && !r.Lease.Active() {
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	logger := logf.FromContext(ctx).WithValues("instance_slug", instance.Spec.Slug)
	ctx = logf.IntoContext(ctx, logger)
	nodes := slices.Clone(instance.Status.Nodes)
	slices.SortFunc(nodes, func(left, right valkeyv1alpha1.NodeStatus) int {
		return int(left.Ordinal - right.Ordinal)
	})
	if len(nodes) > maxMetricNodes {
		nodes = nodes[:maxMetricNodes]
	}

	password, err := r.operatorPassword(ctx, instance)
	if err != nil {
		logger.Error(err, "не удалось прочитать учётные данные для метрик")
		nodes = nil
	}
	results := make(chan collectedNodeMetric, len(nodes))
	for _, node := range nodes {
		go func() {
			metric, collectErr := r.collectNodeMetric(ctx, instance, node, password)
			results <- collectedNodeMetric{metric: metric, err: collectErr}
		}()
	}

	collected := make([]valkeyv1alpha1.NodeMetricStatus, 0, len(nodes))
	for range nodes {
		result := <-results
		if result.err != nil {
			logger.Error(result.err, "не удалось собрать метрики Valkey")
			continue
		}
		collected = append(collected, result.metric)
	}
	if r.Lease != nil && !r.Lease.Active() {
		return ctrl.Result{RequeueAfter: interval}, nil
	}
	if err := r.updateMetrics(ctx, req.NamespacedName, collected); err != nil {
		if errors.Is(err, ErrLeaseInactive) {
			return ctrl.Result{RequeueAfter: interval}, nil
		}

		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: interval}, nil
}

func (r *ValkeyMetricsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&valkeyv1alpha1.ValkeyInstance{}, builder.WithPredicates(valkeyMetricsPredicate())).
		WithOptions(controller.Options{MaxConcurrentReconciles: config.MaxConcurrentReconciles}).
		Named("valkeymetrics").
		Complete(r)
}

func (r *ValkeyMetricsReconciler) operatorPassword(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (string, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{
		Namespace: instance.Namespace,
		Name:      valkeyv1alpha1.AuthSecretName(instance.Spec.Slug),
	}
	if err := reader.Get(ctx, key, secret); err != nil {
		return "", fmt.Errorf("прочитать Secret метрик: %w", err)
	}
	credentials, err := valkeyv1alpha1.ParseServiceCredentials(secret.Data)
	if err != nil {
		return "", err
	}
	if credentials == nil {
		return "", ErrServiceCredentialsMissing
	}

	return string(credentials.OperatorPassword), nil
}

func (r *ValkeyMetricsReconciler) collectNodeMetric(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	node valkeyv1alpha1.NodeStatus,
	password string,
) (valkeyv1alpha1.NodeMetricStatus, error) {
	pod, container, err := r.currentMetricPod(ctx, instance, node)
	if err != nil {
		return valkeyv1alpha1.NodeMetricStatus{}, err
	}

	openSession := r.OpenSession
	if openSession == nil {
		openSession = r.openMetricsSession
	}
	session, err := openSession(ctx, net.JoinHostPort(pod.Status.PodIP, "6379"), password)
	if err != nil {
		return valkeyv1alpha1.NodeMetricStatus{}, err
	}
	defer session.Close()

	metrics, err := session.ReadMetrics(ctx, password)
	if err != nil {
		return valkeyv1alpha1.NodeMetricStatus{}, err
	}
	if metrics.RunID != node.RunID {
		return valkeyv1alpha1.NodeMetricStatus{}, operatorvalkey.ErrProcessChanged
	}

	now := r.now()
	cpu := readCPUMillicores(
		ctx,
		r.PodMetrics,
		pod.Namespace,
		pod.Name,
		container.State.Running.StartedAt.Time,
		now,
	)
	if err := session.VerifyRunID(ctx, metrics.RunID); err != nil {
		return valkeyv1alpha1.NodeMetricStatus{}, err
	}
	if _, _, err := r.currentMetricPod(ctx, instance, node); err != nil {
		return valkeyv1alpha1.NodeMetricStatus{}, err
	}

	return valkeyv1alpha1.NodeMetricStatus{
		Ordinal: node.Ordinal, PodUID: node.PodUID, ContainerID: node.ContainerID,
		RunID: metrics.RunID, CollectedAt: metav1.NewMicroTime(now),
		Role: valkeyv1alpha1.NodeRole(metrics.Role), UsedMemoryBytes: metrics.UsedMemoryBytes,
		MaxmemoryBytes: metrics.MaxmemoryBytes, ConnectedClients: metrics.ConnectedClients,
		OpsPerSec: metrics.OpsPerSec, KeyspaceHits: metrics.KeyspaceHits,
		KeyspaceMisses: metrics.KeyspaceMisses, EvictedKeys: metrics.EvictedKeys,
		CPUMillicores: cpu,
	}, nil
}

func (r *ValkeyMetricsReconciler) currentMetricPod(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	node valkeyv1alpha1.NodeStatus,
) (*corev1.Pod, *corev1.ContainerStatus, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	pod := &corev1.Pod{}
	key := client.ObjectKey{
		Namespace: instance.Namespace,
		Name:      fmt.Sprintf("%s-%d", instance.Spec.Slug, node.Ordinal),
	}
	if err := reader.Get(ctx, key, pod); err != nil {
		return nil, nil, fmt.Errorf("прочитать Pod метрик: %w", err)
	}
	container := valkeyContainerStatus(pod.Status.ContainerStatuses)
	if pod.Status.PodIP == "" || string(pod.UID) != node.PodUID || container == nil ||
		container.ContainerID != node.ContainerID || container.State.Running == nil {
		return nil, nil, operatorvalkey.ErrProcessChanged
	}

	return pod, container, nil
}

func (r *ValkeyMetricsReconciler) openMetricsSession(
	ctx context.Context,
	address string,
	password string,
) (MetricsSession, error) {
	cfg := operatorvalkey.ClientConfig{
		Address: address, Username: "operator", Password: password,
		DialTimeout: config.ValkeyDialTimeout, CommandTimeout: config.ValkeyCommandTimeout,
	}
	var connection *operatorvalkey.Client
	var err error
	if r.Lease != nil {
		connection, err = r.Lease.DialValkey(ctx, cfg)
	} else {
		connection, err = operatorvalkey.Dial(ctx, cfg)
	}
	if err != nil {
		return nil, err
	}
	session, err := connection.OpenSession(ctx)
	if err != nil {
		connection.Close()

		return nil, err
	}

	return &metricsSession{Client: connection, Session: session}, nil
}

type metricsSession struct {
	*operatorvalkey.Client
	*operatorvalkey.Session
}

func (s *metricsSession) Close() {
	s.Session.Close()
	s.Client.Close()
}

func (r *ValkeyMetricsReconciler) updateMetrics(
	ctx context.Context,
	key client.ObjectKey,
	collected []valkeyv1alpha1.NodeMetricStatus,
) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if r.Lease != nil && !r.Lease.Active() {
			return ErrLeaseInactive
		}
		instance := &valkeyv1alpha1.ValkeyInstance{}
		if err := reader.Get(ctx, key, instance); err != nil {
			return err
		}
		before := instance.DeepCopy()
		instance.Status.Metrics = mergedCurrentMetrics(instance.Status.Nodes, instance.Status.Metrics, collected)
		if reflect.DeepEqual(before.Status.Metrics, instance.Status.Metrics) {
			return nil
		}
		patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})

		return r.Status().Patch(ctx, instance, patch)
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("обновить status.metrics: %w", err)
	}

	return nil
}

func mergedCurrentMetrics(
	nodes []valkeyv1alpha1.NodeStatus,
	existing []valkeyv1alpha1.NodeMetricStatus,
	collected []valkeyv1alpha1.NodeMetricStatus,
) []valkeyv1alpha1.NodeMetricStatus {
	current := make(map[int32]valkeyv1alpha1.NodeStatus, len(nodes))
	for _, node := range nodes {
		if len(current) == maxMetricNodes {
			break
		}
		current[node.Ordinal] = node
	}
	metrics := make(map[int32]valkeyv1alpha1.NodeMetricStatus, len(current))
	for _, metric := range existing {
		if node, found := current[metric.Ordinal]; found && metricMatchesNode(metric, node) {
			metrics[metric.Ordinal] = metric
		}
	}
	for _, metric := range collected {
		if node, found := current[metric.Ordinal]; found && metricMatchesNode(metric, node) {
			metrics[metric.Ordinal] = metric
		}
	}
	result := make([]valkeyv1alpha1.NodeMetricStatus, 0, len(metrics))
	for _, metric := range metrics {
		result = append(result, metric)
	}
	slices.SortFunc(result, func(left, right valkeyv1alpha1.NodeMetricStatus) int {
		return int(left.Ordinal - right.Ordinal)
	})

	return result
}

func metricMatchesNode(metric valkeyv1alpha1.NodeMetricStatus, node valkeyv1alpha1.NodeStatus) bool {
	return metric.Ordinal == node.Ordinal && metric.PodUID == node.PodUID &&
		metric.ContainerID == node.ContainerID && metric.RunID == node.RunID
}

func (r *ValkeyMetricsReconciler) now() time.Time {
	if r.Clock == nil {
		return time.Now().UTC()
	}

	return r.Clock.Now().UTC()
}

func valkeyMetricsPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(update event.UpdateEvent) bool {
			before, beforeOK := update.ObjectOld.(*valkeyv1alpha1.ValkeyInstance)
			after, afterOK := update.ObjectNew.(*valkeyv1alpha1.ValkeyInstance)
			if !beforeOK || !afterOK {
				return true
			}

			return !reflect.DeepEqual(before.DeletionTimestamp, after.DeletionTimestamp) ||
				!sameMetricProcessIdentities(before.Status.Nodes, after.Status.Nodes)
		},
	}
}

func sameMetricProcessIdentities(left, right []valkeyv1alpha1.NodeStatus) bool {
	if len(left) != len(right) {
		return false
	}
	for _, leftNode := range left {
		rightNode, found := nodeStatusAtOrdinal(right, leftNode.Ordinal)
		if !found || leftNode.PodUID != rightNode.PodUID ||
			leftNode.ContainerID != rightNode.ContainerID || leftNode.RunID != rightNode.RunID {
			return false
		}
	}

	return true
}
