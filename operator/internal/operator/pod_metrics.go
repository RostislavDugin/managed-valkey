package operator

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
)

const podMetricsMaxAge = time.Minute

type PodMetricsReader interface {
	GetPodMetrics(context.Context, string, string) (*metricsv1beta1.PodMetrics, error)
}

type kubernetesPodMetricsReader struct {
	client metricsclientset.Interface
}

func (r kubernetesPodMetricsReader) GetPodMetrics(
	ctx context.Context,
	namespace string,
	name string,
) (*metricsv1beta1.PodMetrics, error) {
	return r.client.MetricsV1beta1().PodMetricses(namespace).Get(ctx, name, metav1.GetOptions{})
}

func readCPUMillicores(
	ctx context.Context,
	reader PodMetricsReader,
	namespace string,
	name string,
	startedAt time.Time,
	now time.Time,
) *int64 {
	if reader == nil {
		return nil
	}
	metrics, err := reader.GetPodMetrics(ctx, namespace, name)
	if err != nil {
		return nil
	}

	return cpuMillicores(metrics, startedAt, now)
}

func cpuMillicores(
	metrics *metricsv1beta1.PodMetrics,
	startedAt time.Time,
	now time.Time,
) *int64 {
	if metrics == nil || metrics.Timestamp.IsZero() || startedAt.IsZero() ||
		metrics.Timestamp.Time.Before(startedAt) || now.Sub(metrics.Timestamp.Time) > podMetricsMaxAge {
		return nil
	}
	for _, container := range metrics.Containers {
		if container.Name != "valkey" {
			continue
		}
		cpu, found := container.Usage[corev1.ResourceCPU]
		if !found || cpu.Sign() < 0 {
			return nil
		}
		value := cpu.MilliValue()

		return &value
	}

	return nil
}
