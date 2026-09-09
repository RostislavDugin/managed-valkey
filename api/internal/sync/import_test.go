package sync

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func TestBuildMetricSnapshotsKeepsValidNeighborsAndSnapshotRole(t *testing.T) {
	collectedAt := time.Date(2026, time.September, 9, 12, 34, 56, 123456000, time.UTC)
	cpu := int64(125)
	resource := &valkeyv1alpha1.ValkeyInstance{Status: valkeyv1alpha1.ValkeyInstanceStatus{
		Nodes: []valkeyv1alpha1.NodeStatus{
			{
				Ordinal:     0,
				PodUID:      "pod-a",
				ContainerID: "container-a",
				RunID:       "run-a",
				Role:        valkeyv1alpha1.NodeRolePrimary,
			},
			{
				Ordinal:     1,
				PodUID:      "pod-b",
				ContainerID: "container-b",
				RunID:       "run-b",
				Role:        valkeyv1alpha1.NodeRoleReplica,
			},
		},
		Metrics: []valkeyv1alpha1.NodeMetricStatus{
			metricStatus(0, "pod-a", "container-a", "run-a", valkeyv1alpha1.NodeRoleReplica, collectedAt, &cpu),
			metricStatus(2, "pod-c", "container-c", "run-c", valkeyv1alpha1.NodeRoleReplica, collectedAt, nil),
			metricStatus(0, "pod-a", "container-a", "old-run", valkeyv1alpha1.NodeRolePrimary, collectedAt, nil),
			metricStatus(1, "pod-b", "container-b", "run-b", valkeyv1alpha1.NodeRoleReplica, collectedAt, nil),
		},
	}}

	metrics, rejected := buildMetricSnapshots(resource)
	if len(metrics) != 2 || len(rejected) != 2 {
		t.Fatalf("допустимых %d, отклонённых %d", len(metrics), len(rejected))
	}
	if metrics[0].Ordinal != 0 || metrics[0].Role != domain.ValkeyNodeRoleReplica ||
		!metrics[0].TS.Equal(collectedAt) || metrics[0].CPUMillicores == nil || *metrics[0].CPUMillicores != cpu {
		t.Fatalf("первый снимок преобразован неверно: %+v", metrics[0])
	}
	if metrics[1].Ordinal != 1 || metrics[1].RunID != "run-b" || metrics[1].CPUMillicores != nil {
		t.Fatalf("соседний снимок преобразован неверно: %+v", metrics[1])
	}
	if rejected[0].ordinal != 2 || rejected[0].reason != "текущая нода не найдена" ||
		rejected[1].ordinal != 0 || rejected[1].reason != "идентичность процесса не совпадает" {
		t.Fatalf("неожиданные причины отказа: %+v", rejected)
	}
}

func TestBuildMetricSnapshotsRejectsInvalidFields(t *testing.T) {
	collectedAt := time.Date(2026, time.September, 9, 12, 34, 56, 0, time.UTC)
	resource := &valkeyv1alpha1.ValkeyInstance{Status: valkeyv1alpha1.ValkeyInstanceStatus{
		Nodes: []valkeyv1alpha1.NodeStatus{
			{Ordinal: 0, PodUID: "pod-a", ContainerID: "container-a", RunID: "run-a"},
		},
	}}

	tests := []struct {
		name   string
		mutate func(*valkeyv1alpha1.NodeMetricStatus)
	}{
		{name: "пустой Pod UID", mutate: func(metric *valkeyv1alpha1.NodeMetricStatus) { metric.PodUID = "" }},
		{name: "нулевое время", mutate: func(metric *valkeyv1alpha1.NodeMetricStatus) {
			metric.CollectedAt = metav1.MicroTime{}
		}},
		{name: "неизвестная роль", mutate: func(metric *valkeyv1alpha1.NodeMetricStatus) { metric.Role = "unknown" }},
		{name: "отрицательный счётчик", mutate: func(metric *valkeyv1alpha1.NodeMetricStatus) {
			metric.KeyspaceHits = -1
		}},
		{name: "отрицательный CPU", mutate: func(metric *valkeyv1alpha1.NodeMetricStatus) {
			value := int64(-1)
			metric.CPUMillicores = &value
		}},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			metric := metricStatus(
				0,
				"pod-a",
				"container-a",
				"run-a",
				valkeyv1alpha1.NodeRolePrimary,
				collectedAt,
				nil,
			)
			testCase.mutate(&metric)
			resource.Status.Metrics = []valkeyv1alpha1.NodeMetricStatus{metric}

			metrics, rejected := buildMetricSnapshots(resource)
			if len(metrics) != 0 || len(rejected) != 1 || rejected[0].reason != "недопустимые поля" {
				t.Fatalf("допустимые %+v, отклонённые %+v", metrics, rejected)
			}
		})
	}
}

func metricStatus(
	ordinal int32,
	podUID string,
	containerID string,
	runID string,
	role valkeyv1alpha1.NodeRole,
	collectedAt time.Time,
	cpu *int64,
) valkeyv1alpha1.NodeMetricStatus {
	return valkeyv1alpha1.NodeMetricStatus{
		Ordinal: ordinal, PodUID: podUID, ContainerID: containerID, RunID: runID,
		CollectedAt: metav1.NewMicroTime(collectedAt), Role: role,
		UsedMemoryBytes: 100, MaxmemoryBytes: 200, ConnectedClients: 3, OpsPerSec: 4,
		KeyspaceHits: 5, KeyspaceMisses: 6, EvictedKeys: 7, CPUMillicores: cpu,
	}
}
