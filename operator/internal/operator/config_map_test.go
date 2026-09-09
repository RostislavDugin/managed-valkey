package operator

import (
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func TestConfigMapMemoryCalculations(t *testing.T) {
	tests := []struct {
		name      string
		vcpu      int32
		ramGB     int32
		maxmemory int64
		backlog   int64
		ioThreads int64
	}{
		{name: "smallest", vcpu: 1, ramGB: 1, maxmemory: 805306368, backlog: 10737418, ioThreads: 1},
		{name: "four cores", vcpu: 4, ramGB: 4, maxmemory: 3221225472, backlog: 42949672, ioThreads: 4},
		{name: "largest", vcpu: 16, ramGB: 128, maxmemory: 103079215104, backlog: 67108864, ioThreads: 8},
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

func TestDesiredConfigMapIsImmutableAndContentAddressed(t *testing.T) {
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

func TestConfigMapContainsNoSecretsAndReadinessUsesRoleAndInfo(t *testing.T) {
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
}
