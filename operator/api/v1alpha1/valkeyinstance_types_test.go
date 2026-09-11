package v1alpha1_test

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func Test_NodeMetricStatus_WhenCPUIsUnavailable_OmitsCPUAndPreservesMicroseconds(t *testing.T) {
	collectedAt := time.Date(2026, time.September, 9, 12, 34, 56, 123456000, time.UTC)
	metric := valkeyv1alpha1.NodeMetricStatus{
		Ordinal:          1,
		PodUID:           "pod-uid",
		ContainerID:      "container-id",
		RunID:            "run-id",
		CollectedAt:      metav1.NewMicroTime(collectedAt),
		Role:             valkeyv1alpha1.NodeRoleReplica,
		UsedMemoryBytes:  1024,
		MaxmemoryBytes:   2048,
		ConnectedClients: 3,
		OpsPerSec:        4,
		KeyspaceHits:     5,
		KeyspaceMisses:   6,
		EvictedKeys:      7,
		CPUMillicores:    nil,
	}

	encoded, err := json.Marshal(metric)
	if err != nil {
		t.Fatalf("закодировать снимок метрик: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("декодировать поля снимка метрик: %v", err)
	}
	if _, exists := fields["cpuMillicores"]; exists {
		t.Fatal("CPU присутствует в JSON при недоступной метрике")
	}

	var decoded valkeyv1alpha1.NodeMetricStatus
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("декодировать снимок метрик: %v", err)
	}
	if !decoded.CollectedAt.Time.Equal(collectedAt) {
		t.Fatalf("время %s, ожидалось %s", decoded.CollectedAt.Time, collectedAt)
	}
	if decoded.CPUMillicores != nil {
		t.Fatalf("CPU равен %v, ожидалось отсутствие значения", decoded.CPUMillicores)
	}
}
