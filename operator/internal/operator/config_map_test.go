package operator

import (
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func Test_ConfigMapData_WithSupportedResourceSizes_CalculatesMemoryAndIOThreads(t *testing.T) {
	tests := []struct {
		name      string
		vcpu      int32
		ramGB     int32
		maxmemory int64
		backlog   int64
		ioThreads int64
	}{
		{
			name:      "для минимального размера вычисляет память, backlog и один поток ввода-вывода",
			vcpu:      1,
			ramGB:     1,
			maxmemory: 805306368,
			backlog:   10737418,
			ioThreads: 1,
		},
		{
			name:      "для четырёх ядер вычисляет память, backlog и четыре потока ввода-вывода",
			vcpu:      4,
			ramGB:     4,
			maxmemory: 3221225472,
			backlog:   42949672,
			ioThreads: 4,
		},
		{
			name:      "для максимального размера ограничивает backlog и число потоков ввода-вывода",
			vcpu:      16,
			ramGB:     128,
			maxmemory: 103079215104,
			backlog:   67108864,
			ioThreads: 8,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := configMapData(valkeyv1alpha1.AcceptedConfiguration{VCPU: test.vcpu, RAMGB: test.ramGB})
			configuration := data[valkeyConfigKey]
			for _, line := range []string{
				"maxmemory " + strconv.FormatInt(test.maxmemory, 10),
				"repl-backlog-size " + strconv.FormatInt(test.backlog, 10),
				"io-threads " + strconv.FormatInt(test.ioThreads, 10),
				"save \"\"",
				"appendonly no",
			} {
				if !strings.Contains(configuration, line+"\n") {
					t.Errorf("конфигурация не содержит %q", line)
				}
			}
		})
	}
}

func Test_DesiredConfigMap_WhenConfigurationChanges_IsImmutableAndContentAddressed(t *testing.T) {
	instance := &valkeyv1alpha1.ValkeyInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "cache-a1b2c3", Namespace: "valkey-cache-a1b2c3", UID: "instance-uid"},
		Status: valkeyv1alpha1.ValkeyInstanceStatus{
			AcceptedConfiguration: &valkeyv1alpha1.AcceptedConfiguration{
				Slug: "cache-a1b2c3", Mode: valkeyv1alpha1.ValkeyModeSingle, VCPU: 2, RAMGB: 4,
			},
		},
	}
	reconciler := &ValkeyInstanceReconciler{Scheme: NewScheme()}

	configMap, err := reconciler.desiredConfigMap(instance)
	if err != nil {
		t.Fatalf("построить ConfigMap: %v", err)
	}
	if configMap.Immutable == nil || !*configMap.Immutable {
		t.Fatal("ConfigMap не является неизменяемой")
	}
	if !strings.HasSuffix(configMap.Name, configDigest(configMap.Data)) {
		t.Fatalf("имя %q не содержит digest данных", configMap.Name)
	}
	if len(configMap.OwnerReferences) != 1 || configMap.OwnerReferences[0].UID != instance.UID {
		t.Fatalf("неверный ownerReference: %v", configMap.OwnerReferences)
	}

	changed := configMapData(*instance.Status.AcceptedConfiguration)
	changed[valkeyConfigKey] += "io-threads 7\n"
	if configDigest(changed) == configDigest(configMap.Data) {
		t.Fatal("digest не изменился вместе с содержимым")
	}
}

func Test_ConfigMapData_WithSingleMode_ContainsNoSecretsAndChecksRoleAndInfoForReadiness(t *testing.T) {
	data := configMapData(valkeyv1alpha1.AcceptedConfiguration{VCPU: 1, RAMGB: 1})
	all := strings.Join([]string{data[valkeyConfigKey], data[startScriptKey], data[readinessScriptKey]}, "\n")
	for _, secret := range []string{"operator-password-value", strings.Repeat("ab", 32)} {
		if strings.Contains(all, secret) {
			t.Fatalf("ConfigMap содержит секрет %q", secret)
		}
	}
	for _, command := range []string{"ROLE", "INFO replication"} {
		if !strings.Contains(data[readinessScriptKey], command) {
			t.Fatalf("readiness не вызывает %s", command)
		}
	}
	if strings.Contains(data[valkeyConfigKey], "replicaof") {
		t.Fatal("single запускается как replica")
	}
	for _, fragment := range []string{
		"valkey-server /run/valkey/valkey.conf &",
		"trap 'kill -TERM \"$valkey_pid\" 2>/dev/null || true' TERM INT",
		"wait \"$valkey_pid\"",
	} {
		if !strings.Contains(data[startScriptKey], fragment) {
			t.Fatalf("стартовый скрипт не управляет дочерним Valkey: %q", fragment)
		}
	}
}

func Test_ConfigMapData_WithHAMode_StartsAsReplicaAndAcceptsSynchronizedReplicaReadiness(t *testing.T) {
	accepted := valkeyv1alpha1.AcceptedConfiguration{
		Slug: "cache-a1b2c3", Mode: valkeyv1alpha1.ValkeyModeHA, VCPU: 1, RAMGB: 1,
	}
	data := configMapData(accepted)
	configuration := data[valkeyConfigKey]
	if !strings.Contains(
		configuration,
		"replicaof cache-a1b2c3-primary.valkey-cache-a1b2c3.svc 6379\n",
	) {
		t.Fatalf("HA не стартует репликой: %s", configuration)
	}
	readiness := data[readinessScriptKey]
	for _, check := range []string{"master_link_status:up", "master_sync_in_progress:0", "role:slave"} {
		if !strings.Contains(readiness, check) {
			t.Fatalf("readiness HA не проверяет %s: %s", check, readiness)
		}
	}
}
