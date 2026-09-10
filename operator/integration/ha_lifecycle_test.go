//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func Test_HA01HA04HA07FP04PW01RZ02RZ03_RunHALifecycle_WithFailuresAndMutations_PreservesAvailabilityAndData(
	t *testing.T,
) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "haops1")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	servicePasswords := h.servicePasswords(t, instance)

	primary := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{primary})
	setValue(t, primary, "ha-key", "before-failures")
	readOnlyInstance := *instance
	readOnlyInstance.hostname = instance.slug + "-ro." + h.baseDomain
	readOnly := openPersistentConnection(t, h.publicAddr, &readOnlyInstance, h.caFile, false, func() {})
	if value := getValue(t, readOnly, "ha-key"); value != "before-failures" {
		closeConnections([]*persistentConnection{readOnly})
		t.Fatalf("реплика вернула %q", value)
	}
	writeRESP(t, readOnly, "SET", "read-only-key", "blocked")
	if _, err := readRESPResult(t, readOnly); err == nil || !strings.Contains(err.Error(), "READONLY") {
		closeConnections([]*persistentConnection{readOnly})
		t.Fatalf("реплика приняла запись: %v", err)
	}
	closeConnections([]*persistentConnection{readOnly})

	replicaOrdinals := make([]int32, 0, 2)
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica {
			replicaOrdinals = append(replicaOrdinals, process.Ordinal)
		}
	}
	if len(replicaOrdinals) != 2 {
		t.Fatalf("FP-04 ожидал две реплики: %+v", status.Status.Nodes)
	}
	primaryUID := status.Status.PrimaryPodUID
	for _, ordinal := range replicaOrdinals {
		current := h.getInstance(t, instance)
		replica, found := nodeStatusAtOrdinal(current.Status.Nodes, ordinal)
		if !found || replica.Role != valkeyv1alpha1.NodeRoleReplica {
			t.Fatalf("FP-04 ordinal %d перестал быть репликой: %+v", ordinal, current.Status.Nodes)
		}
		startedAt := time.Now()
		h.deletePod(t, instance, ordinal)
		h.waitForReplacementOrdinal(t, instance, ordinal, types.UID(replica.PodUID))
		status = h.waitRunning(t, instance)
		t.Logf("FP-04 DELETE replica ordinal %d: %s", ordinal, time.Since(startedAt))
		if status.Status.PrimaryPodUID != primaryUID {
			t.Fatalf("FP-04 удаление реплики сменило primary: %s -> %s", primaryUID, status.Status.PrimaryPodUID)
		}
		assertHAComposition(t, h, instance, status)
		if value := getValue(t, primary, "ha-key"); value != "before-failures" {
			t.Fatalf("FP-04 удаление реплики потеряло ключ: %q", value)
		}
	}

	oldPrimaryOrdinal := *status.Status.PrimaryOrdinal
	oldPrimaryUID := types.UID(status.Status.PrimaryPodUID)
	startedAt := time.Now()
	h.deletePod(t, instance, oldPrimaryOrdinal)
	replacementPod := h.waitForReplacementOrdinal(t, instance, oldPrimaryOrdinal, oldPrimaryUID)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		replacement, found := nodeStatusAtOrdinal(current.Status.Nodes, oldPrimaryOrdinal)
		if !found || replacement.PodUID != string(replacementPod.UID) {
			return false
		}
		pod := &corev1.Pod{}
		if err := h.k8s.Get(t.Context(), client.ObjectKey{
			Name: replacementPod.Name, Namespace: replacementPod.Namespace,
		}, pod); err != nil {
			return false
		}
		role := pod.Labels["role"]
		fullRole := pod.Labels[valkeyv1alpha1.RoleLabelKey]
		if role != fullRole {
			t.Fatalf("FP-04 замена прежнего primary получила разные метки роли: %v", pod.Labels)
		}
		if role != "" && role != string(valkeyv1alpha1.NodeRoleReplica) {
			t.Fatalf("FP-04 замена прежнего primary получила неизвестную роль: %v", pod.Labels)
		}
		admittedReplica := false
		if current.Status.PrimaryOrdinal != nil {
			primary, primaryFound := nodeStatusAtOrdinal(current.Status.Nodes, *current.Status.PrimaryOrdinal)
			primaryPod := &corev1.Pod{}
			primaryPodFound := primaryFound && h.k8s.Get(t.Context(), client.ObjectKey{
				Name:      instance.slug + "-" + strconv.FormatInt(int64(primary.Ordinal), 10),
				Namespace: instance.namespace,
			}, primaryPod) == nil
			admittedReplica = primaryPodFound && primary.Role == valkeyv1alpha1.NodeRolePrimary &&
				replacement.Role == valkeyv1alpha1.NodeRoleReplica && replacement.Readiness &&
				replacement.AppEnabled &&
				replacement.AppPasswordVersion == current.Status.AcceptedConfiguration.PasswordVersion &&
				replacement.Replication != nil && replacement.Replication.LinkUp &&
				!replacement.Replication.SyncInProgress && replacement.Replication.SyncedAt != nil &&
				replacement.Replication.UpstreamHost == primaryPod.Status.PodIP &&
				replacement.Replication.UpstreamPort == 6379
		}
		if role == string(valkeyv1alpha1.NodeRoleReplica) && !admittedReplica {
			t.Fatalf(
				"FP-04 замена прежнего primary получила replica до синхронизации и допуска: process=%+v labels=%v",
				replacement,
				pod.Labels,
			)
		}

		return current.Status.Initialized && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning &&
			current.Status.PrimaryPodUID != "" && current.Status.PrimaryPodUID != string(oldPrimaryUID) &&
			current.Status.Failover == nil && admittedReplica &&
			role == string(valkeyv1alpha1.NodeRoleReplica)
	}, "нового primary и допуска синхронизированной замены прежнего primary")
	t.Logf("FP-04 DELETE primary: %s", time.Since(startedAt))
	assertHAComposition(t, h, instance, status)
	closeConnections([]*persistentConnection{primary})
	primary = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, primary, "ha-key"); value != "before-failures" {
		t.Fatalf("failover потерял синхронизированный ключ: %q", value)
	}

	beforeRotation := processIdentities(status.Status.Nodes)
	beforeACL := readRealAppACLs(t, h, instance, status.Status.Nodes)
	rotationReadOnly := openPersistentConnection(t, h.publicAddr, &readOnlyInstance, h.caFile, false, func() {})
	rotationPubSub := openPersistentConnection(t, h.publicAddr, instance, h.caFile, true, func() {})
	rotationBlocking := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	writeRESP(t, rotationBlocking, "BLPOP", "never-pushed", "0")
	directConnections := make([]*persistentConnection, 0, len(status.Status.Nodes)*2)
	directBlocking := make([]*persistentConnection, 0, len(status.Status.Nodes))
	for _, process := range status.Status.Nodes {
		pod := h.getPodOrdinal(t, instance, process.Ordinal)
		address := net.JoinHostPort(pod.Status.PodIP, "6379")
		directConnections = append(
			directConnections,
			openDirectAppConnection(t, address, instance.password, false),
			openDirectAppConnection(t, address, instance.password, true),
		)
		blocking := openDirectAppConnection(t, address, instance.password, false)
		writeRESP(t, blocking, "XREAD", "BLOCK", "0", "STREAMS", "pw01-stream-"+strconv.Itoa(int(process.Ordinal)), "$")
		directBlocking = append(directBlocking, blocking)
	}
	defer closeConnections(directConnections)
	defer closeConnections(directBlocking)
	oldPassword := instance.password
	newPassword := "rotated-" + mustUUIDv7(t)
	h.rotatePassword(t, instance, newPassword, 2)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.AppliedPasswordVersion == 2 && current.Status.CredentialRotation == nil &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "завершения смены пароля HA")
	assertConnectionClosed(t, primary, false)
	assertConnectionClosed(t, rotationReadOnly, false)
	assertConnectionClosed(t, rotationPubSub, false)
	assertConnectionClosed(t, rotationBlocking, true)
	for _, connection := range directConnections {
		assertConnectionClosed(t, connection, false)
	}
	for _, connection := range directBlocking {
		assertConnectionClosed(t, connection, true)
	}
	closeConnections([]*persistentConnection{primary, rotationReadOnly, rotationPubSub, rotationBlocking})
	closeConnections(directConnections)
	closeConnections(directBlocking)
	instance.password = newPassword
	readOnlyInstance.password = newPassword
	assertAuthenticationRejected(t, h, instance, oldPassword)
	assertProcessIdentities(t, beforeRotation, status.Status.Nodes)
	assertRealAppACLsChangedOnlyPassword(t, h, instance, status.Status.Nodes, beforeACL, newPassword, true)
	assertPasswordsOnAllProcesses(t, h, instance, oldPassword, newPassword)
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("ротация HA изменила служебные пароли")
	}
	primary = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, primary, "ha-key"); value != "before-failures" {
		t.Fatalf("ротация пароля потеряла ключ: %q", value)
	}

	primary = resizeHAAndAssertPreserved(t, h, instance, primary, 2, 2, 3, "before-failures")
	primary = resizeHAAndAssertPreserved(t, h, instance, primary, 1, 2, 4, "before-failures")
	primary = resizeHAAndAssertPreserved(t, h, instance, primary, 2, 2, 5, "before-failures")

	beforeShrink := processIdentities(h.getInstance(t, instance).Status.Nodes)
	setValue(t, primary, "shrink-key", "discarded")
	h.resize(t, instance, 1, 1, 6)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Rollout == nil && current.Status.Applied != nil &&
			current.Status.Applied.VCPU == 1 && current.Status.Applied.RAMGB == 1 &&
			current.Status.ObservedGeneration == 6 && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "RZ-03 завершения HA shrink")
	assertConnectionClosed(t, primary, false)
	closeConnections([]*persistentConnection{primary})
	assertHAComposition(t, h, instance, status)
	assertAllProcessIdentitiesChanged(t, beforeShrink, status.Status.Nodes)
	for _, process := range status.Status.Nodes {
		assertPodResourcesAndMaxmemory(t, h, instance, process, 1, 1)
	}
	primary = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	writeRESP(t, primary, "GET", "shrink-key")
	if value := readRESP(t, primary); value != nil {
		t.Fatalf("RZ-03 сохранил кэш после полного останова: %#v", value)
	}

	deletionReadOnly := openPersistentConnection(t, h.publicAddr, &readOnlyInstance, h.caFile, false, func() {})
	h.requestDeletion(t, instance)
	h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Deletion != nil &&
			(current.Status.Deletion.Stage == valkeyv1alpha1.DeletionStageStopping ||
				current.Status.Deletion.Stage == valkeyv1alpha1.DeletionStageVerifying)
	}, "HA-07 остановки состава")
	secret := &corev1.Secret{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{
		Name: valkeyv1alpha1.AuthSecretName(instance.slug), Namespace: instance.namespace,
	}, secret); err != nil {
		t.Fatalf("HA-07 удалил Secret до доказательства остановки: %v", err)
	}
	assertConnectionClosed(t, primary, false)
	assertConnectionClosed(t, deletionReadOnly, false)
	closeConnections([]*persistentConnection{primary, deletionReadOnly})
	h.waitDeleted(t, instance)
}

func Test_HA02HA04HA06_CreateHA_WhenOperatorRestarts_EnforcesInitialSafetyAndInstanceIsolation(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	whitelist := valkeyv1alpha1.WhitelistSpec{IsEnabled: true, CIDRs: append(
		[]string{"127.0.0.1/32", h.clientAllowed + "/32", h.clientWrongSNI + "/32"},
		h.operatorCIDRs...,
	)}
	startedAt := time.Now()
	instance := h.createHAWithWhitelist(t, "hasafe", whitelist)
	assigned := h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return !current.Status.Initialized && current.Status.PrimaryOrdinal != nil &&
			*current.Status.PrimaryOrdinal == 0 && len(current.Status.Nodes) == 3
	}, "HA-02 сохранённого первоначального primary")
	h.stopOperator(t)
	assertInitialProcessesFenced(t, h, instance, assigned)
	h.startOperator(t)
	status := h.waitRunning(t, instance)
	t.Logf("HA-01 создание HA с рестартом: %s", time.Since(startedAt))
	if status.Status.PrimaryOrdinal == nil || *status.Status.PrimaryOrdinal != 0 {
		t.Fatalf("HA-02 после рестарта выбрал другой primary: %+v", status.Status)
	}
	assertHAComposition(t, h, instance, status)
	assertHAEndpointSlices(t, h, instance, status)

	readOnlyInstance := *instance
	readOnlyInstance.hostname = instance.slug + "-ro." + h.baseDomain
	connections := h.openHAEnvoyConnections(t, instance)
	t.Cleanup(func() { closeConnections(connections) })
	setValue(t, connections[0], "ha-initial-key", "replicated")
	assertKeyOnAllReplicas(t, h, instance, status, "ha-initial-key", "replicated")

	for _, endpoint := range []*testInstance{instance, &readOnlyInstance} {
		assertAuthenticationRejected(t, h, endpoint, "wrong-password")
		assertWrongSNIRejected(t, h, endpoint)
		assertDockerPing(t, h, endpoint, h.clientAllowed, instance.password, true)
		assertDockerPing(t, h, endpoint, h.clientBlocked, instance.password, false)
	}

	denyAll := h.createHAWithWhitelist(t, "hadeny", valkeyv1alpha1.WhitelistSpec{IsEnabled: true})
	h.waitRunning(t, denyAll)
	denyReadOnly := *denyAll
	denyReadOnly.hostname = denyAll.slug + "-ro." + h.baseDomain
	for _, endpoint := range []*testInstance{denyAll, &denyReadOnly} {
		assertDockerPing(t, h, endpoint, h.clientAllowed, denyAll.password, false)
	}
	neighbor := h.createSingle(t, "haisolation", valkeyv1alpha1.WhitelistSpec{})
	h.waitRunning(t, neighbor)
	assertAuthenticationRejected(t, h, neighbor, instance.password)
	neighborPassword := "neighbor-" + mustUUIDv7(t)
	h.rotatePassword(t, neighbor, neighborPassword, 2)
	h.waitFor(t, neighbor, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.CredentialRotation == nil && current.Status.AppliedPasswordVersion == 2 &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "HA-06 смены пароля соседнего single")
	pingConnections(t, connections)
	neighbor.password = neighborPassword
	h.deleteInstance(t, neighbor)
	pingConnections(t, connections)
	h.deleteInstance(t, denyAll)
	pingConnections(t, connections)

	closeConnections(connections)
	connections = nil
	startedAt = time.Now()
	h.deleteInstance(t, instance)
	t.Logf("HA-07 удаление HA: %s", time.Since(startedAt))
}

func resizeHAAndAssertPreserved(
	t *testing.T,
	h *harness,
	instance *testInstance,
	connection *persistentConnection,
	vcpu int32,
	ram int32,
	generation int64,
	expectedValue string,
) *persistentConnection {
	t.Helper()
	before := processIdentities(h.getInstance(t, instance).Status.Nodes)
	startedAt := time.Now()
	h.resize(t, instance, vcpu, ram, generation)
	status := h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Rollout == nil && current.Status.Applied != nil &&
			current.Status.Applied.VCPU == vcpu && current.Status.Applied.RAMGB == ram &&
			current.Status.ObservedGeneration == generation &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "RZ-02 завершения последовательного HA-ресайза")
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	assertHAComposition(t, h, instance, status)
	assertAllProcessIdentitiesChanged(t, before, status.Status.Nodes)
	for _, process := range status.Status.Nodes {
		assertPodResourcesAndMaxmemory(t, h, instance, process, vcpu, ram)
	}
	result := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, result, "ha-key"); value != expectedValue {
		closeConnections([]*persistentConnection{result})
		t.Fatalf("RZ-02 потерял синхронизированный ключ: %q", value)
	}
	t.Logf("RZ-02 ресайз HA до %d CPU/%d GiB: %s", vcpu, ram, time.Since(startedAt))
	return result
}

func assertAllProcessIdentitiesChanged(
	t *testing.T,
	before map[int32]valkeyv1alpha1.ProcessIdentity,
	after []valkeyv1alpha1.NodeStatus,
) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("число процессов изменилось: %d != %d", len(before), len(after))
	}
	for _, process := range after {
		if previous, found := before[process.Ordinal]; !found || previous.PodUID == process.PodUID ||
			previous.ContainerID == process.ContainerID || previous.RunID == process.RunID {
			t.Fatalf("ordinal %d не получил новый процесс: before=%+v after=%+v", process.Ordinal, previous, process)
		}
	}
}

func (h *harness) createHA(t *testing.T, prefix string) *testInstance {
	return h.createHAWithSize(t, prefix, 1, 1)
}

func (h *harness) createHAWithSize(t *testing.T, prefix string, vcpu, ram int32) *testInstance {
	return h.createHAWithSizeAndWhitelist(t, prefix, vcpu, ram, valkeyv1alpha1.WhitelistSpec{})
}

func (h *harness) createHAWithWhitelist(
	t *testing.T,
	prefix string,
	whitelist valkeyv1alpha1.WhitelistSpec,
) *testInstance {
	return h.createHAWithSizeAndWhitelist(t, prefix, 1, 1, whitelist)
}

func (h *harness) createHAWithSizeAndWhitelist(
	t *testing.T,
	prefix string,
	vcpu int32,
	ram int32,
	whitelist valkeyv1alpha1.WhitelistSpec,
) *testInstance {
	t.Helper()
	stamp := strings.ToLower(strconv.FormatInt(time.Now().UnixNano(), 36))
	if len(stamp) > 6 {
		stamp = stamp[len(stamp)-6:]
	}
	slug := prefix + "-" + stamp
	if len(prefix) < 3 || len(prefix) > 20 {
		t.Fatalf("неверный префикс slug %q", prefix)
	}
	namespace := "valkey-" + slug
	instanceID := mustUUIDv7(t)
	userID := mustUUIDv7(t)
	userEmail := prefix + "@example.com"
	password := "test-" + mustUUIDv7(t)
	digest := sha256.Sum256([]byte(password))
	namespaceObject := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: namespace,
		Labels: map[string]string{
			instanceLabel:                     slug,
			valkeyv1alpha1.InstanceIDLabelKey: instanceID,
			userIDLabel:                       userID,
		},
		Annotations: map[string]string{valkeyv1alpha1.UserEmailAnnotationKey: userEmail},
	}}
	if err := h.k8s.Create(t.Context(), namespaceObject); err != nil {
		t.Fatalf("создать namespace HA: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: valkeyv1alpha1.AuthSecretName(slug), Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			valkeyv1alpha1.AppPasswordHashKeyPrefix + "1": []byte(hex.EncodeToString(digest[:])),
		},
	}
	if err := h.k8s.Create(t.Context(), secret); err != nil {
		t.Fatalf("создать Secret HA: %v", err)
	}
	resource := &valkeyv1alpha1.ValkeyInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name: slug, Namespace: namespace,
			Labels: map[string]string{
				instanceLabel:                     slug,
				valkeyv1alpha1.InstanceIDLabelKey: instanceID,
				userIDLabel:                       userID,
			},
			Annotations: map[string]string{valkeyv1alpha1.UserEmailAnnotationKey: userEmail},
		},
		Spec: valkeyv1alpha1.ValkeyInstanceSpec{
			InstanceID: instanceID, Slug: slug, Mode: valkeyv1alpha1.ValkeyModeHA,
			VCPU: vcpu, RAMGB: ram, PublicPort: 41379, Whitelist: &whitelist,
			PasswordVersion: 1, DesiredGeneration: 1,
		},
	}
	if err := h.k8s.Create(t.Context(), resource); err != nil {
		t.Fatalf("создать ValkeyInstance HA: %v", err)
	}
	result := &testInstance{
		slug: slug, namespace: namespace, instanceID: instanceID, userID: userID, userEmail: userEmail,
		password: password,
		hostname: slug + "." + h.baseDomain,
	}
	h.created = append(h.created, result)
	return result
}

func assertHAComposition(
	t *testing.T,
	h *harness,
	instance *testInstance,
	status *valkeyv1alpha1.ValkeyInstance,
) {
	t.Helper()
	if len(status.Status.Nodes) != 3 || status.Status.PrimaryOrdinal == nil {
		t.Fatalf("неполный status HA: %+v", status.Status)
	}
	nodes := make(map[string]struct{}, 3)
	primaries := 0
	replicas := 0
	primary := processAtOrdinal(t, status, *status.Status.PrimaryOrdinal)
	primaryPod := h.getPodOrdinal(t, instance, primary.Ordinal)
	for _, process := range status.Status.Nodes {
		nodes[process.NodeName] = struct{}{}
		switch process.Role {
		case valkeyv1alpha1.NodeRolePrimary:
			primaries++
		case valkeyv1alpha1.NodeRoleReplica:
			replicas++
			if process.Replication == nil || !process.Replication.LinkUp ||
				process.Replication.SyncedAt == nil ||
				process.Replication.UpstreamHost != primaryPod.Status.PodIP ||
				process.Replication.UpstreamPort != 6379 {
				t.Fatalf("реплика ordinal %d не подтверждена у текущего primary: %+v", process.Ordinal, process)
			}
		}
		waitForPodMetadata(t, h, instance, process)
	}
	if len(nodes) != 3 || primaries != 1 || replicas != 2 {
		t.Fatalf("неверное размещение или роли HA: nodes=%v status=%+v", nodes, status.Status.Nodes)
	}
}

func waitForPodMetadata(
	t *testing.T,
	h *harness,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	err := wait.PollUntilContextTimeout(
		t.Context(), 250*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			if err := h.k8s.Get(ctx, client.ObjectKey{
				Name:      instance.slug + "-" + strconv.FormatInt(int64(process.Ordinal), 10),
				Namespace: instance.namespace,
			}, pod); err != nil {
				return false, err
			}
			return string(pod.UID) == process.PodUID &&
				pod.Labels["role"] == string(process.Role) &&
				pod.Labels[valkeyv1alpha1.RoleLabelKey] == string(process.Role) &&
				pod.Labels[valkeyv1alpha1.InstanceIDLabelKey] == instance.instanceID &&
				pod.Labels[valkeyv1alpha1.UserIDLabelKey] == instance.userID &&
				pod.Annotations[valkeyv1alpha1.UserEmailAnnotationKey] == instance.userEmail, nil
		},
	)
	if err != nil {
		t.Fatalf(
			"дождаться метаданных ordinal %d: %v; uid=%s labels=%v annotations=%v",
			process.Ordinal,
			err,
			pod.UID,
			pod.Labels,
			pod.Annotations,
		)
	}

	return pod.DeepCopy()
}

func assertInitialProcessesFenced(
	t *testing.T,
	h *harness,
	instance *testInstance,
	status *valkeyv1alpha1.ValkeyInstance,
) {
	t.Helper()
	operatorPassword := h.servicePasswords(t, instance)[0]
	primaries := 0
	for _, process := range status.Status.Nodes {
		pod := h.getPodOrdinal(t, instance, process.Ordinal)
		roleOutput, err := execInPod(t, h.adminREST, pod.Namespace, pod.Name, []string{
			"valkey-cli", "--user", "operator", "--pass", operatorPassword,
			"--no-auth-warning", "--raw", "ROLE",
		})
		if err != nil {
			t.Fatalf("HA-02 прочитать роль ordinal %d: output=%q error=%v", process.Ordinal, roleOutput, err)
		}
		role := strings.Split(strings.TrimSpace(roleOutput), "\n")[0]
		if role == "master" {
			primaries++
		} else if role != "slave" {
			t.Fatalf("HA-02 получил неизвестную роль ordinal %d: %q", process.Ordinal, role)
		}
		aclOutput, err := execInPod(t, h.adminREST, pod.Namespace, pod.Name, []string{
			"valkey-cli", "--user", "operator", "--pass", operatorPassword,
			"--no-auth-warning", "--json", "ACL", "GETUSER", "app",
		})
		if err != nil {
			t.Fatalf("HA-02 прочитать ACL ordinal %d: output=%q error=%v", process.Ordinal, aclOutput, err)
		}
		acl := struct {
			Flags []string `json:"flags"`
		}{}
		if err := json.Unmarshal([]byte(strings.TrimSpace(aclOutput)), &acl); err != nil {
			t.Fatalf("HA-02 разобрать ACL ordinal %d: output=%q error=%v", process.Ordinal, aclOutput, err)
		}
		if !slices.Contains(acl.Flags, "off") || slices.Contains(acl.Flags, "on") {
			t.Fatalf("HA-02 app включён до допуска ordinal %d: %v", process.Ordinal, acl.Flags)
		}
	}
	if primaries > 1 {
		t.Fatalf("HA-02 до рестарта доступны %d primary", primaries)
	}
}

func assertKeyOnAllReplicas(
	t *testing.T,
	h *harness,
	instance *testInstance,
	status *valkeyv1alpha1.ValkeyInstance,
	key string,
	expected string,
) {
	t.Helper()
	for _, process := range status.Status.Nodes {
		if process.Role != valkeyv1alpha1.NodeRoleReplica {
			continue
		}
		pod := h.getPodOrdinal(t, instance, process.Ordinal)
		var lastOutput string
		err := wait.PollUntilContextTimeout(
			t.Context(),
			100*time.Millisecond,
			10*time.Second,
			true,
			func(context.Context) (bool, error) {
				output, commandErr := execInPod(t, h.adminREST, pod.Namespace, pod.Name, []string{
					"valkey-cli", "--user", "app", "--pass", instance.password,
					"--no-auth-warning", "--raw", "GET", key,
				})
				lastOutput = output
				return commandErr == nil && strings.TrimSpace(output) == expected, nil
			},
		)
		if err != nil {
			t.Fatalf("HA-01 реплика ordinal %d не получила ключ: output=%q error=%v", process.Ordinal, lastOutput, err)
		}
	}
}

func assertDockerPing(
	t *testing.T,
	h *harness,
	instance *testInstance,
	address string,
	password string,
	expected bool,
) {
	t.Helper()
	var output string
	var err error
	if expected {
		output, err = h.dockerCLI(t, instance, address, password, "PING")
	} else {
		output, err = h.dockerCLIWithTimeout(t, 3*time.Second, instance, address, password, "PING")
	}
	passed := err == nil && strings.Contains(output, "PONG")
	if passed != expected {
		t.Fatalf(
			"HA-04 доступ %s к %s: expected=%t output=%q error=%v",
			address,
			instance.hostname,
			expected,
			output,
			err,
		)
	}
}

func assertHAEndpointSlices(
	t *testing.T,
	h *harness,
	instance *testInstance,
	status *valkeyv1alpha1.ValkeyInstance,
) {
	t.Helper()
	primaryUIDs := map[types.UID]struct{}{types.UID(status.Status.PrimaryPodUID): {}}
	replicaUIDs := make(map[types.UID]struct{}, 2)
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica {
			replicaUIDs[types.UID(process.PodUID)] = struct{}{}
		}
	}
	for service, expected := range map[string]map[types.UID]struct{}{
		instance.slug + "-primary":  primaryUIDs,
		instance.slug + "-replicas": replicaUIDs,
	} {
		slices := &discoveryv1.EndpointSliceList{}
		if err := h.k8s.List(
			t.Context(),
			slices,
			client.InNamespace(instance.namespace),
			client.MatchingLabels{discoveryv1.LabelServiceName: service},
		); err != nil {
			t.Fatalf("HA-04 прочитать EndpointSlice %s: %v", service, err)
		}
		observed := make(map[types.UID]struct{}, len(expected))
		for _, endpointSlice := range slices.Items {
			for _, endpoint := range endpointSlice.Endpoints {
				if endpoint.TargetRef == nil || endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready {
					continue
				}
				observed[endpoint.TargetRef.UID] = struct{}{}
			}
		}
		if !maps.Equal(expected, observed) {
			t.Fatalf(
				"HA-04 EndpointSlice %s содержит неверные Pod UID: expected=%v observed=%v",
				service,
				expected,
				observed,
			)
		}
	}
}

func (h *harness) openHAEnvoyConnections(
	t *testing.T,
	instance *testInstance,
) []*persistentConnection {
	t.Helper()
	pods := &corev1.PodList{}
	if err := h.k8s.List(
		t.Context(),
		pods,
		client.InNamespace("envoy-gateway-system"),
		client.MatchingLabels{
			"gateway.envoyproxy.io/owning-gateway-name":      "valkey",
			"gateway.envoyproxy.io/owning-gateway-namespace": systemNamespace,
		},
	); err != nil {
		t.Fatalf("HA-04 прочитать Pod Envoy: %v", err)
	}
	if len(pods.Items) != 2 {
		t.Fatalf("HA-04 на стенде %d процессов Envoy вместо 2", len(pods.Items))
	}
	readOnly := *instance
	readOnly.hostname = instance.slug + "-ro." + h.baseDomain
	connections := make([]*persistentConnection, 0, 4)
	for index := range pods.Items {
		address, stop := h.forwardEnvoy(t, &pods.Items[index])
		connections = append(
			connections,
			openPersistentConnection(t, address, instance, h.caFile, false, func() {}),
			openPersistentConnection(t, address, &readOnly, h.caFile, false, stop),
		)
	}
	return connections
}

func replicaProcess(t *testing.T, instance *valkeyv1alpha1.ValkeyInstance) (int32, types.UID) {
	t.Helper()
	for _, process := range instance.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica {
			return process.Ordinal, types.UID(process.PodUID)
		}
	}
	t.Fatal("реплика не найдена")
	return 0, ""
}

func (h *harness) getPodOrdinal(t *testing.T, instance *testInstance, ordinal int32) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	key := client.ObjectKey{
		Name:      instance.slug + "-" + strconv.FormatInt(int64(ordinal), 10),
		Namespace: instance.namespace,
	}
	if err := h.k8s.Get(t.Context(), key, pod); err != nil {
		t.Fatalf("прочитать Pod ordinal %d: %v", ordinal, err)
	}
	return pod
}

func (h *harness) deletePod(t *testing.T, instance *testInstance, ordinal int32) {
	t.Helper()
	pod := h.getPodOrdinal(t, instance, ordinal)
	if err := h.k8s.Delete(t.Context(), pod); err != nil {
		t.Fatalf("удалить Pod ordinal %d: %v", ordinal, err)
	}
	h.recordFaultEvent(t, "pod-deleted", pod.Namespace+"/"+pod.Name)
}

func (h *harness) waitForReplacementOrdinal(
	t *testing.T,
	instance *testInstance,
	ordinal int32,
	oldUID types.UID,
) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	err := wait.PollUntilContextTimeout(
		t.Context(), time.Second, 5*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			key := client.ObjectKey{
				Name:      instance.slug + "-" + strconv.FormatInt(int64(ordinal), 10),
				Namespace: instance.namespace,
			}
			err := h.k8s.Get(ctx, key, &pod)
			if client.IgnoreNotFound(err) != nil {
				return false, err
			}
			return err == nil && pod.UID != oldUID && pod.Status.PodIP != "", nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться замены Pod ordinal %d: %v", ordinal, err)
	}
	return pod.DeepCopy()
}

func (h *harness) rotatePassword(
	t *testing.T,
	instance *testInstance,
	password string,
	version int64,
) {
	t.Helper()
	h.deliverPasswordHash(t, instance, password, version)
	h.requestPasswordVersion(t, instance, version, version)
}

func (h *harness) deliverPasswordHash(
	t *testing.T,
	instance *testInstance,
	password string,
	version int64,
) {
	t.Helper()
	secret := &corev1.Secret{}
	key := client.ObjectKey{Name: valkeyv1alpha1.AuthSecretName(instance.slug), Namespace: instance.namespace}
	digest := sha256.Sum256([]byte(password))
	if err := retry.RetryOnConflict(wait.Backoff{
		Duration: 100 * time.Millisecond, Factor: 2, Steps: 8,
	}, func() error {
		if err := h.k8s.Get(t.Context(), key, secret); err != nil {
			return err
		}
		secret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+strconv.FormatInt(version, 10)] = []byte(
			hex.EncodeToString(digest[:]),
		)
		return h.k8s.Update(t.Context(), secret)
	}); err != nil {
		t.Fatalf("доставить новый хеш: %v", err)
	}
}

func (h *harness) requestPasswordVersion(
	t *testing.T,
	instance *testInstance,
	version int64,
	generation int64,
) {
	t.Helper()
	if err := retry.RetryOnConflict(wait.Backoff{
		Duration: 100 * time.Millisecond, Factor: 2, Steps: 8,
	}, func() error {
		resource := &valkeyv1alpha1.ValkeyInstance{}
		if err := h.k8s.Get(
			t.Context(), client.ObjectKey{Name: instance.slug, Namespace: instance.namespace}, resource,
		); err != nil {
			return err
		}
		resource.Spec.PasswordVersion = version
		resource.Spec.DesiredGeneration = generation
		return h.k8s.Update(t.Context(), resource)
	}); err != nil {
		t.Fatalf("принять смену пароля: %v", err)
	}
}

func (h *harness) resize(t *testing.T, instance *testInstance, vcpu, ram int32, generation int64) {
	t.Helper()
	if err := retry.RetryOnConflict(wait.Backoff{
		Duration: 100 * time.Millisecond, Factor: 2, Steps: 8,
	}, func() error {
		resource := &valkeyv1alpha1.ValkeyInstance{}
		if err := h.k8s.Get(
			t.Context(), client.ObjectKey{Name: instance.slug, Namespace: instance.namespace}, resource,
		); err != nil {
			return err
		}
		resource.Spec.VCPU = vcpu
		resource.Spec.RAMGB = ram
		resource.Spec.DesiredGeneration = generation
		return h.k8s.Update(t.Context(), resource)
	}); err != nil {
		t.Fatalf("запросить ресайз: %v", err)
	}
}
