package operator

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

func Test_CPUMillicores_WithCurrentContainerMetric_ReturnsWholeMillicores(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-2 * time.Minute)
	tests := []struct {
		name      string
		metrics   *metricsv1beta1.PodMetrics
		startedAt time.Time
		want      *int64
	}{
		{
			name:      "свежее значение округляется до целых милликор",
			metrics:   podCPU(now.Add(-10*time.Second), "125500u"),
			startedAt: startedAt,
			want:      int64Pointer(126),
		},
		{name: "ответ отсутствует", startedAt: startedAt},
		{
			name:      "отметка старше минуты",
			metrics:   podCPU(now.Add(-time.Minute-time.Nanosecond), "125m"),
			startedAt: startedAt,
		},
		{
			name:      "отметка предшествует запуску контейнера",
			metrics:   podCPU(startedAt.Add(-time.Nanosecond), "125m"),
			startedAt: startedAt,
		},
		{
			name:      "значение CPU отрицательное",
			metrics:   podCPU(now.Add(-10*time.Second), "-1m"),
			startedAt: startedAt,
		},
		{
			name: "метрика другого контейнера",
			metrics: &metricsv1beta1.PodMetrics{
				Timestamp: metav1.NewTime(now.Add(-10 * time.Second)),
				Containers: []metricsv1beta1.ContainerMetrics{{
					Name: "sidecar", Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1m")},
				}},
			},
			startedAt: startedAt,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := cpuMillicores(test.metrics, test.startedAt, now)
			if (got == nil) != (test.want == nil) || got != nil && *got != *test.want {
				t.Fatalf("получено %v, ожидалось %v", pointerValue(got), pointerValue(test.want))
			}
		})
	}
}

func Test_ReadCPUMillicores_WhenMetricsAPIIsUnavailable_ReturnsNil(t *testing.T) {
	reader := podMetricsReaderFunc(func(context.Context, string, string) (*metricsv1beta1.PodMetrics, error) {
		return nil, errors.New("metrics API недоступен")
	})
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	if got := readCPUMillicores(t.Context(), reader, "default", "valkey-0", now.Add(-time.Minute), now); got != nil {
		t.Fatalf("ошибка API дала CPU %d", *got)
	}
}

func podCPU(timestamp time.Time, value string) *metricsv1beta1.PodMetrics {
	return &metricsv1beta1.PodMetrics{
		Timestamp: metav1.NewTime(timestamp),
		Containers: []metricsv1beta1.ContainerMetrics{{
			Name: "valkey", Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(value)},
		}},
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}

func pointerValue(value *int64) any {
	if value == nil {
		return nil
	}

	return *value
}

type podMetricsReaderFunc func(context.Context, string, string) (*metricsv1beta1.PodMetrics, error)

func (f podMetricsReaderFunc) GetPodMetrics(
	ctx context.Context,
	namespace string,
	name string,
) (*metricsv1beta1.PodMetrics, error) {
	return f(ctx, namespace, name)
}
