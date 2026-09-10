//go:build integration

package integration_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	operatorconfig "github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorcontroller "github.com/RostislavDugin/managed-valkey/operator/internal/operator"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

const clusterOperatorName = "operator-integration"

func Test_CT14HA03_ReconcilePendingReplica_WhenCapacityReturns_MarksInstanceRunning(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })
	h.requireNodeCount(t, 4)

	nodeName := "k3s-agent-2"
	capacityNode := "k3s-agent-3"
	h.setNodeUnschedulable(t, nodeName, true)
	h.setNodeUnschedulable(t, capacityNode, true)
	t.Cleanup(func() {
		h.setNodeUnschedulable(t, nodeName, false)
		h.setNodeUnschedulable(t, capacityNode, false)
	})

	instance := h.createHA(t, "pending")
	status := h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Initialized && current.Status.Phase == valkeyv1alpha1.InstancePhaseDegraded &&
			len(current.Status.Nodes) == 2 && current.Status.PrimaryOrdinal != nil
	}, "HA-03 degraded при нехватке нод")
	if len(status.Status.Nodes) != 2 {
		t.Fatalf("HA-03 получил неожиданный состав: %+v", status.Status.Nodes)
	}
	h.waitForPendingPod(t, instance)

	h.setNodeUnschedulable(t, capacityNode, false)
	h.requireNodeCount(t, 4)
	status = h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	addedProcess := valkeyv1alpha1.NodeStatus{}
	found := false
	for _, process := range status.Status.Nodes {
		if process.NodeName == "k3s-agent-3" {
			addedProcess = process
			found = true
			break
		}
	}
	if !found || addedProcess.Role != valkeyv1alpha1.NodeRoleReplica {
		t.Fatalf("HA-03 не разместила Pending-реплику на добавленной ноде: %+v", status.Status.Nodes)
	}
	h.deleteInstance(t, instance)
}

func Test_HA05_EvictPod_WhenTwoPodsAreReady_IsRejectedByPDB(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })
	h.requireNodeCount(t, 3)

	instance := h.createHA(t, "pdbtest")
	status := h.waitRunning(t, instance)
	ordinal, _ := replicaProcess(t, status)
	removed := processAtOrdinal(t, status, ordinal)
	h.setNodeUnschedulable(t, removed.NodeName, true)
	t.Cleanup(func() { h.setNodeUnschedulable(t, removed.NodeName, false) })
	h.deletePod(t, instance, ordinal)
	h.waitForPDBDisruptions(t, instance, 0)

	target := status.Status.Nodes[0]
	if target.Ordinal == ordinal {
		target = status.Status.Nodes[1]
	}
	targetPod := h.getPodOrdinal(t, instance, target.Ordinal)
	err := h.clientset.PolicyV1().Evictions(instance.namespace).Evict(t.Context(), &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: targetPod.Name, Namespace: targetPod.Namespace},
	})
	if !apierrors.IsTooManyRequests(err) {
		t.Fatalf("HA-05 PDB не отклонил Eviction при двух готовых Pod: %v", err)
	}

	h.setNodeUnschedulable(t, removed.NodeName, false)
	h.waitForReplacementOrdinal(t, instance, ordinal, types.UID(removed.PodUID))
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		if current.Status.Phase != valkeyv1alpha1.InstancePhaseRunning {
			return false
		}
		for _, process := range current.Status.Nodes {
			if process.Ordinal == ordinal {
				return process.PodUID != removed.PodUID
			}
		}
		return false
	}, "HA-05 status новой реплики после прямого DELETE")
	assertHAComposition(t, h, instance, status)
	h.deleteInstance(t, instance)
}

func Test_FP01FP02FP03_RecoverHA_WhenPrimaryOrReplicaReceivesSIGKILL_PreservesDataAndRoles(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	ha := h.createHA(t, "sigkill")
	status := h.waitRunning(t, ha)
	assertHAComposition(t, h, ha, status)
	connection := openPersistentConnection(t, h.publicAddr, ha, h.caFile, false, func() {})
	setValue(t, connection, "sigkill-key", "before-primary")

	oldPrimary := primaryProcess(t, status)
	startedAt := time.Now()
	h.sigkillValkeyProcess(t, ha, oldPrimary)
	closeConnections([]*persistentConnection{connection})
	status = h.waitFor(t, ha, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Failover == nil && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning &&
			current.Status.PrimaryPodUID != "" && current.Status.PrimaryPodUID != oldPrimary.PodUID &&
			len(current.Status.Nodes) == 3
	}, "FP-01 восстановления после SIGKILL primary")
	t.Logf("FP-01 SIGKILL primary: %s", time.Since(startedAt))
	assertHAComposition(t, h, ha, status)
	connection = openPersistentConnection(t, h.publicAddr, ha, h.caFile, false, func() {})
	if value := getValue(t, connection, "sigkill-key"); value != "before-primary" {
		t.Fatalf("FP-01 потерял ключ после SIGKILL primary: %q", value)
	}

	primaryUID := status.Status.PrimaryPodUID
	replicaOrdinals := make([]int32, 0, 2)
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica {
			replicaOrdinals = append(replicaOrdinals, process.Ordinal)
		}
	}
	if len(replicaOrdinals) != 2 {
		t.Fatalf("FP-02 ожидал две реплики: %+v", status.Status.Nodes)
	}
	for index, ordinal := range replicaOrdinals {
		current := h.getInstance(t, ha)
		replica, found := nodeStatusAtOrdinal(current.Status.Nodes, ordinal)
		if !found || replica.Role != valkeyv1alpha1.NodeRoleReplica {
			t.Fatalf("FP-02 ordinal %d перестал быть репликой: %+v", ordinal, current.Status.Nodes)
		}
		value := "after-replica-" + strconv.Itoa(index)
		setValue(t, connection, "sigkill-key", value)
		h.sigkillValkeyProcess(t, ha, replica)
		h.waitForReplacementOrdinal(t, ha, ordinal, types.UID(replica.PodUID))
		status = h.waitFor(t, ha, func(candidate *valkeyv1alpha1.ValkeyInstance) bool {
			process, exists := nodeStatusAtOrdinal(candidate.Status.Nodes, ordinal)
			return exists && process.PodUID != replica.PodUID && process.Role == valkeyv1alpha1.NodeRoleReplica &&
				process.Readiness && process.Replication != nil && process.Replication.LinkUp &&
				process.Replication.SyncedAt != nil && candidate.Status.PrimaryPodUID == primaryUID &&
				candidate.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
		}, "FP-02 синхронизации замены реплики")
		if received := getValue(t, connection, "sigkill-key"); received != value {
			t.Fatalf("FP-02 primary потерял запись после SIGKILL реплики: %q", received)
		}
	}
	closeConnections([]*persistentConnection{connection})
	h.deleteInstance(t, ha)
}

func Test_FP01FP02FP03_RecoverSingle_WhenProcessReceivesSIGKILL_ReplacesProcessWithEmptyCache(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	single := h.createSingle(t, "sigone", valkeyv1alpha1.WhitelistSpec{})
	singleStatus := h.waitRunning(t, single)
	if len(singleStatus.Status.Nodes) != 1 {
		t.Fatalf("FP-03 получил неверный состав single: %+v", singleStatus.Status.Nodes)
	}
	singleConnection := openPersistentConnection(t, h.publicAddr, single, h.caFile, false, func() {})
	setValue(t, singleConnection, "single-key", "discarded")
	oldSingle := singleStatus.Status.Nodes[0]
	h.sigkillValkeyProcess(t, single, oldSingle)
	closeConnections([]*persistentConnection{singleConnection})
	h.waitForReplacementOrdinal(t, single, oldSingle.Ordinal, types.UID(oldSingle.PodUID))
	h.waitRunning(t, single)
	singleConnection = openPersistentConnection(t, h.publicAddr, single, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{singleConnection})
	writeRESP(t, singleConnection, "GET", "single-key")
	if value := readRESP(t, singleConnection); value != nil {
		t.Fatalf("FP-03 восстановил прежний кэш single: %#v", value)
	}

	h.deleteInstance(t, single)
}

func Test_FP12_FailOver_WhenReplicasHaveDifferentOffsets_PromotesMostAdvancedReplica(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "fpoffset")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	primary := primaryProcess(t, status)
	replicas := make([]valkeyv1alpha1.NodeStatus, 0, 2)
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica {
			replicas = append(replicas, process)
		}
	}
	if len(replicas) != 2 {
		t.Fatalf("FP-12 ожидала две реплики: %+v", status.Status.Nodes)
	}
	lagging := replicas[0]
	newer := replicas[1]
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "fp12-baseline", "replicated")
	h.waitForReplicaValue(t, instance, lagging, "fp12-baseline", "replicated")
	h.waitForReplicaValue(t, instance, newer, "fp12-baseline", "replicated")

	h.stopOperator(t)
	h.signalValkeyProcess(t, instance, lagging, "STOP")
	continued := false
	defer func() {
		if !continued {
			h.signalValkeyProcess(t, instance, lagging, "CONT")
		}
	}()
	h.waitForValkeyProcessState(t, instance, lagging, "T", "FP-12 SIGSTOP отстающей реплики")
	h.disconnectReplicaClients(t, instance, primary)
	h.waitForConnectedReplicaCount(t, instance, primary, 1)
	setValue(t, connection, "fp12-newer", "preserved")
	h.waitForReplicaValue(t, instance, newer, "fp12-newer", "preserved")
	h.sigkillValkeyProcess(t, instance, primary)
	closeConnections([]*persistentConnection{connection})
	h.signalValkeyProcess(t, instance, lagging, "CONT")
	continued = true
	h.waitForValkeyProcessState(t, instance, lagging, "S", "FP-12 SIGCONT отстающей реплики")

	laggingHistory, laggingOffset := h.directReplicationPosition(t, instance, lagging)
	newerHistory, newerOffset := h.directReplicationPosition(t, instance, newer)
	if laggingHistory == "" || laggingHistory != newerHistory || newerOffset <= laggingOffset {
		t.Fatalf(
			"FP-12 не создала сопоставимое отставание: same_history=%t lagging_offset=%d newer_offset=%d",
			laggingHistory != "" && laggingHistory == newerHistory,
			laggingOffset,
			newerOffset,
		)
	}

	startedAt := time.Now()
	h.startOperator(t)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Failover == nil && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning &&
			current.Status.PrimaryPodUID == newer.PodUID && len(current.Status.Nodes) == 3
	}, "FP-12 выбора более свежей реплики")
	t.Logf("FP-12 переключение на больший offset: %s", time.Since(startedAt))
	assertHAComposition(t, h, instance, status)
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	if value := getValue(t, connection, "fp12-newer"); value != "preserved" {
		t.Fatalf("FP-12 выбрала копию без контрольного ключа: %q", value)
	}

	h.deleteInstance(t, instance)
}

func Test_HA08OP03OP04OP08_ResumeFailover_WhenOperatorRestartsAtEveryStage_CompletesWithoutDataLoss(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "hafence")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	source := primaryProcess(t, status)
	sourcePod := h.getPodOrdinal(t, instance, source.Ordinal)
	sourceAddress := net.JoinHostPort(sourcePod.Status.PodIP, "6379")
	primaryConnection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, primaryConnection, "ha08-key", "preserved")

	beforeSourceACL := newValkeyCommandPoint(
		t,
		sourceAddress,
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandBefore,
		false,
	)
	h.resize(t, instance, 2, 2, 2)
	beforeSourceACL.waitFor(t, 3*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Failover == nil || status.Status.Failover.Candidate == nil ||
		status.Status.Failover.Stage != valkeyv1alpha1.FailoverStageFencing {
		t.Fatalf("HA-08 не сохранила fencing перед ACL: %+v", status.Status.Failover)
	}
	candidate := processForIdentity(t, status.Status.Nodes, *status.Status.Failover.Candidate)
	candidatePod := h.getPodOrdinal(t, instance, candidate.Ordinal)
	candidateAddress := net.JoinHostPort(candidatePod.Status.PodIP, "6379")
	readOnlyInstance := *instance
	readOnlyInstance.hostname = instance.slug + "-ro." + h.baseDomain
	readOnlyConnection := h.openReadOnlyConnectionForRunID(t, &readOnlyInstance, candidate.RunID)
	h.signalValkeyProcess(t, instance, candidate, "STOP")
	h.waitForValkeyProcessState(t, instance, candidate, "T", "OP-08 остановки кандидата до разрыва репликации")
	candidateReplicationFault := h.addNodeNetworkFault(
		t,
		"op08-offset-candidate",
		candidate.NodeName,
		"FORWARD",
		hostCIDR(t, candidatePod.Status.PodIP),
		hostCIDR(t, sourcePod.Status.PodIP),
		6379,
	)
	sourceReplicationFault := h.addNodeNetworkFault(
		t,
		"op08-offset-source",
		source.NodeName,
		"FORWARD",
		hostCIDR(t, candidatePod.Status.PodIP),
		hostCIDR(t, sourcePod.Status.PodIP),
		6379,
	)
	h.disconnectReplicaClients(t, instance, source)
	h.waitForConnectedReplicaCount(t, instance, source, 1)
	setValue(t, primaryConnection, "op08-lag", "not-on-candidate")
	h.signalValkeyProcess(t, instance, candidate, "CONT")
	h.waitForValkeyProcessState(t, instance, candidate, "S", "OP-08 возврата отстающего кандидата")
	_, sourceOffset := h.directReplicationPosition(t, instance, source)
	_, candidateOffset := h.directReplicationPosition(t, instance, candidate)
	if candidateOffset >= sourceOffset {
		t.Fatalf("OP-08 не создала отставание до failover: candidate=%d source=%d", candidateOffset, sourceOffset)
	}

	h.stopOperator(t)
	beforeSourceACL.close()
	status = h.getInstance(t, instance)
	if status.Status.Failover == nil || status.Status.Failover.Stage != valkeyv1alpha1.FailoverStageFencing {
		t.Fatalf("OP-03 потеряла fencing после рестарта до ACL: %+v", status.Status.Failover)
	}

	lostSourceACL := newValkeyCommandPoint(
		t,
		sourceAddress,
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandAfter,
		true,
	)
	h.startOperator(t)
	lostSourceACL.wait(t)
	h.stopOperator(t)
	lostSourceACL.close()
	assertRealProcessRoleACL(t, h, instance, source, "master", instance.password, false)
	pingConnections(t, []*persistentConnection{primaryConnection})

	lostSourceClients := newValkeyCommandPoint(
		t,
		sourceAddress,
		"CLIENT KILL app",
		operatorvalkey.IntegrationCommandAfter,
		true,
	)
	h.startOperator(t)
	lostSourceClients.wait(t)
	h.stopOperator(t)
	lostSourceClients.close()
	assertConnectionClosed(t, primaryConnection, false)
	closeConnections([]*persistentConnection{primaryConnection})

	beforeCandidateACL := newValkeyCommandPoint(
		t,
		candidateAddress,
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandBefore,
		false,
	)
	h.startOperator(t)
	beforeCandidateACL.wait(t)
	h.stopOperator(t)
	beforeCandidateACL.close()
	status = h.getInstance(t, instance)
	if status.Status.Failover == nil || status.Status.Failover.Stage != valkeyv1alpha1.FailoverStageChoosing ||
		status.Status.Failover.OffsetDeadline == nil {
		t.Fatalf("OP-03 не сохранила choosing и deadline до ACL кандидата: %+v", status.Status.Failover)
	}
	offsetDeadline := status.Status.Failover.OffsetDeadline.DeepCopy()
	_, candidateOffset = h.directReplicationPosition(t, instance, candidate)
	if candidateOffset >= status.Status.Failover.ControlOffset {
		t.Fatalf(
			"OP-08 control offset не опережает кандидата: candidate=%d control=%d",
			candidateOffset,
			status.Status.Failover.ControlOffset,
		)
	}
	restartedCandidateACL := newValkeyCommandPoint(
		t,
		candidateAddress,
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandBefore,
		false,
	)
	h.startOperator(t)
	restartedCandidateACL.wait(t)
	h.stopOperator(t)
	restartedCandidateACL.close()
	status = h.getInstance(t, instance)
	if status.Status.Failover == nil || status.Status.Failover.OffsetDeadline == nil ||
		!status.Status.Failover.OffsetDeadline.Equal(offsetDeadline) {
		t.Fatalf("OP-08 изменила deadline после рестарта: %+v", status.Status.Failover)
	}

	lostCandidateACL := newValkeyCommandPoint(
		t,
		candidateAddress,
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandAfter,
		true,
	)
	h.startOperator(t)
	lostCandidateACL.wait(t)
	h.stopOperator(t)
	lostCandidateACL.close()
	assertRealProcessRoleACL(t, h, instance, candidate, "slave", instance.password, false)

	lostCandidateClients := newValkeyCommandPoint(
		t,
		candidateAddress,
		"CLIENT KILL app",
		operatorvalkey.IntegrationCommandAfter,
		true,
	)
	h.startOperator(t)
	lostCandidateClients.wait(t)
	h.stopOperator(t)
	lostCandidateClients.close()
	assertConnectionClosed(t, readOnlyConnection, false)
	closeConnections([]*persistentConnection{readOnlyConnection})
	assertRealProcessRoleACL(t, h, instance, candidate, "slave", instance.password, false)

	status = h.getInstance(t, instance)
	if status.Status.Failover == nil || status.Status.Failover.OffsetDeadline == nil ||
		!status.Status.Failover.OffsetDeadline.Equal(offsetDeadline) {
		t.Fatalf("OP-08 потеряла deadline после fencing кандидата: %+v", status.Status.Failover)
	}
	if _, candidateOffset = h.directReplicationPosition(
		t,
		instance,
		candidate,
	); candidateOffset >= status.Status.Failover.ControlOffset {
		t.Fatalf(
			"OP-08 кандидат не отстаёт: candidate=%d control=%d",
			candidateOffset,
			status.Status.Failover.ControlOffset,
		)
	}
	beforePromotion := newValkeyCommandPoint(
		t,
		candidateAddress,
		"REPLICAOF NO ONE",
		operatorvalkey.IntegrationCommandBefore,
		false,
	)
	h.startOperator(t)
	beforePromotion.waitFor(t, 30*time.Second)
	h.stopOperator(t)
	beforePromotion.close()
	status = h.getInstance(t, instance)
	if status.Status.Failover == nil || status.Status.Failover.Stage != valkeyv1alpha1.FailoverStagePromoting ||
		!status.Status.Failover.CandidateMayBePrimary || status.Status.Failover.OffsetDeadline == nil ||
		!status.Status.Failover.OffsetDeadline.Equal(offsetDeadline) {
		t.Fatalf("OP-03 не сохранила promoting до команды: %+v", status.Status.Failover)
	}

	afterPromotion := newValkeyCommandPoint(
		t,
		candidateAddress,
		"REPLICAOF NO ONE",
		operatorvalkey.IntegrationCommandAfter,
		true,
	)
	h.startOperator(t)
	afterPromotion.wait(t)
	h.stopOperator(t)
	afterPromotion.close()
	assertRealProcessRoleACL(t, h, instance, candidate, "master", instance.password, false)

	lostFormerPrimaryFollow := newValkeyCommandPoint(
		t,
		sourceAddress,
		"REPLICAOF",
		operatorvalkey.IntegrationCommandAfter,
		true,
	)
	h.startOperator(t)
	lostFormerPrimaryFollow.waitFor(t, 30*time.Second)
	h.stopOperator(t)
	lostFormerPrimaryFollow.close()
	status = h.getInstance(t, instance)
	formerPrimary := processAtOrdinal(t, status, source.Ordinal)
	if role := h.directValkeyRole(t, instance, formerPrimary); role != "slave" {
		t.Fatalf("OP-04 потерянный ответ REPLICAOF не изменил роль прежнего primary: %s", role)
	}
	formerPrimaryInfo := h.directReplicationInfo(t, instance, formerPrimary)
	if formerPrimaryInfo["master_host"] != candidatePod.Status.PodIP {
		t.Fatalf("OP-04 прежний primary подключён к %q вместо %q",
			formerPrimaryInfo["master_host"], candidatePod.Status.PodIP)
	}

	beforePrimaryAdmission := newValkeyCommandPoint(
		t,
		candidateAddress,
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandBefore,
		false,
	)
	h.startOperator(t)
	beforePrimaryAdmission.wait(t)
	h.stopOperator(t)
	beforePrimaryAdmission.close()
	status = h.getInstance(t, instance)
	if status.Status.Failover == nil ||
		status.Status.Failover.Stage != valkeyv1alpha1.FailoverStageReconfiguring ||
		status.Status.PrimaryPodUID != candidate.PodUID {
		t.Fatalf("OP-03 не сохранила reconfiguring после продвижения: %+v", status.Status.Failover)
	}
	lostPrimaryAdmission := newValkeyCommandPoint(
		t,
		candidateAddress,
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandAfter,
		true,
	)
	h.startOperator(t)
	lostPrimaryAdmission.waitFor(t, 30*time.Second)
	h.stopOperator(t)
	lostPrimaryAdmission.close()
	assertRealProcessRoleACL(t, h, instance, candidate, "master", instance.password, true)
	status = h.getInstance(t, instance)
	if status.Status.Failover == nil ||
		status.Status.Failover.Stage != valkeyv1alpha1.FailoverStageReconfiguring {
		t.Fatalf("OP-03/OP-04 потеряли reconfiguring после выполненного допуска: %+v", status.Status.Failover)
	}
	if err := candidateReplicationFault.restore(); err != nil {
		t.Fatalf("восстановить исходящий путь репликации OP-08: %v", err)
	}
	if err := sourceReplicationFault.restore(); err != nil {
		t.Fatalf("восстановить входящий путь репликации OP-08: %v", err)
	}
	h.startOperator(t)

	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Failover == nil && current.Status.Rollout == nil &&
			current.Status.PrimaryPodUID == candidate.PodUID && current.Status.Applied != nil &&
			current.Status.Applied.VCPU == 2 && current.Status.Applied.RAMGB == 2 &&
			current.Status.ObservedGeneration == 2 && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "HA-08 завершения переключения после рестартов и потерянных ответов")
	assertHAComposition(t, h, instance, status)
	primaryConnection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, primaryConnection, "ha08-key"); value != "preserved" {
		t.Fatalf("HA-08 потеряла контрольный ключ: %q", value)
	}
	readOnlyConnection = openPersistentConnection(t, h.publicAddr, &readOnlyInstance, h.caFile, false, func() {})
	if value := getValue(t, readOnlyConnection, "ha08-key"); value != "preserved" {
		t.Fatalf("HA-08 новый маршрут чтения вернул %q", value)
	}
	closeConnections([]*persistentConnection{primaryConnection, readOnlyConnection})

	h.deleteInstance(t, instance)
}

func Test_OP05_ResumeFailover_WhenCandidateStopsBeforePromotion_SelectsAnotherCandidateWithoutDataLoss(t *testing.T) {
	testOP05CandidateLoss(t, operatorvalkey.IntegrationCommandBefore)
}

func Test_OP05_ResumeFailover_WhenCandidateStopsAfterPromotion_SelectsAnotherCandidateWithoutDataLoss(t *testing.T) {
	testOP05CandidateLoss(t, operatorvalkey.IntegrationCommandAfter)
}

func testOP05CandidateLoss(t *testing.T, stage operatorvalkey.IntegrationCommandStage) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "opfive"+string(stage))
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	source := primaryProcess(t, status)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "op05-key", string(stage))
	assertKeyOnAllReplicas(t, h, instance, status, "op05-key", string(stage))

	promotionPoint := newValkeyCommandPoint(
		t,
		"",
		"REPLICAOF NO ONE",
		stage,
		stage == operatorvalkey.IntegrationCommandAfter,
	)
	h.sigkillValkeyProcess(t, instance, source)
	closeConnections([]*persistentConnection{connection})
	promotionPoint.waitFor(t, 45*time.Second)
	status = h.getInstance(t, instance)
	if status.Status.Failover == nil || status.Status.Failover.Candidate == nil ||
		!status.Status.Failover.CandidateMayBePrimary {
		t.Fatalf("OP-05 не сохранила потенциальный primary: %+v", status.Status.Failover)
	}
	candidate := processForIdentity(t, status.Status.Nodes, *status.Status.Failover.Candidate)
	expectedRole := "slave"
	if stage == operatorvalkey.IntegrationCommandAfter {
		expectedRole = "master"
	}
	assertRealProcessRoleACL(t, h, instance, candidate, expectedRole, instance.password, false)
	h.sigkillValkeyProcess(t, instance, candidate)
	promotionPoint.close()

	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Failover == nil && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning &&
			current.Status.PrimaryPodUID != "" && current.Status.PrimaryPodUID != source.PodUID &&
			current.Status.PrimaryPodUID != candidate.PodUID && len(current.Status.Nodes) == 3
	}, "OP-05 выбора другой цели после доказанной остановки кандидата")
	assertHAComposition(t, h, instance, status)
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "op05-key"); value != string(stage) {
		t.Fatalf("OP-05 потеряла контрольный ключ после %s: %q", stage, value)
	}
	closeConnections([]*persistentConnection{connection})
	h.deleteInstance(t, instance)
}

func Test_OP06_ResumeFailover_WithLiveUnknownCandidate_BlocksSecondPromotionUntilFenced(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "opsix")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	source := primaryProcess(t, status)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "op06-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "op06-key", "preserved")

	promotionPoint := newValkeyCommandPoint(
		t,
		"",
		"REPLICAOF NO ONE",
		operatorvalkey.IntegrationCommandAfter,
		true,
	)
	h.sigkillValkeyProcess(t, instance, source)
	closeConnections([]*persistentConnection{connection})
	promotionPoint.waitFor(t, 45*time.Second)
	status = h.getInstance(t, instance)
	if status.Status.Failover == nil || status.Status.Failover.Candidate == nil ||
		!status.Status.Failover.CandidateMayBePrimary {
		t.Fatalf("OP-06 не сохранила неизвестный результат продвижения: %+v", status.Status.Failover)
	}
	candidateIdentity := *status.Status.Failover.Candidate
	candidate := processForIdentity(t, status.Status.Nodes, candidateIdentity)
	assertRealProcessRoleACL(t, h, instance, candidate, "master", instance.password, false)
	candidatePod := h.getPodOrdinal(t, instance, candidate.Ordinal)
	fault := h.addNodeNetworkFault(
		t,
		"op06-candidate-isolation",
		candidate.NodeName,
		"FORWARD",
		podRouteSourceCIDR(t, h.k8s),
		hostCIDR(t, candidatePod.Status.PodIP),
		6379,
	)
	promotionPoint.close()
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Failover != nil && current.Status.Failover.Candidate != nil &&
			*current.Status.Failover.Candidate == candidateIdentity &&
			current.Status.Failover.CandidateMayBePrimary &&
			current.Status.Failover.Stage == valkeyv1alpha1.FailoverStagePromoting &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseUnavailable
	}, "OP-06 ожидания живого неизвестного кандидата")

	negativeWindow := operatorconfig.EmptyPrimaryTimeout +
		2*operatorconfig.HealthCheckInterval
	negativeDeadline := time.Now().Add(negativeWindow)
	err := wait.PollUntilContextTimeout(
		t.Context(),
		250*time.Millisecond,
		negativeWindow+time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			if !time.Now().Before(negativeDeadline) {
				return true, nil
			}
			current := &valkeyv1alpha1.ValkeyInstance{}
			if err := h.k8s.Get(
				ctx,
				client.ObjectKey{Name: instance.slug, Namespace: instance.namespace},
				current,
			); err != nil {
				return false, err
			}
			if current.Status.Failover == nil || current.Status.Failover.Candidate == nil ||
				*current.Status.Failover.Candidate != candidateIdentity ||
				!current.Status.Failover.CandidateMayBePrimary {
				return false, fmt.Errorf("неизвестный кандидат заменён без fencing: %+v", current.Status.Failover)
			}
			for _, process := range current.Status.Nodes {
				role := h.directValkeyRole(t, instance, process)
				if role == "master" && process.RunID != candidate.RunID {
					return false, fmt.Errorf("продвинут второй primary ordinal %d", process.Ordinal)
				}
			}
			return false, nil
		},
	)
	if err != nil {
		t.Fatalf("OP-06 не сохранила единственного потенциального primary: %v", err)
	}
	h.sigkillValkeyProcess(t, instance, candidate)
	if err := fault.restore(); err != nil {
		t.Fatalf("снять изоляцию кандидата OP-06: %v", err)
	}
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Failover == nil && current.Status.PrimaryPodUID != source.PodUID &&
			current.Status.PrimaryPodUID != candidate.PodUID &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning && len(current.Status.Nodes) == 3
	}, "OP-06 продолжения после доказанной остановки неизвестного кандидата")
	assertHAComposition(t, h, instance, status)
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "op06-key"); value != "preserved" {
		t.Fatalf("OP-06 потеряла контрольный ключ: %q", value)
	}
	closeConnections([]*persistentConnection{connection})
	h.deleteInstance(t, instance)
}

func Test_RZ10_ReplaceFormerPrimary_WhenHARolloutIsInProgress_WaitsForDirectReplicaUpstream(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "rzten")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	source := primaryProcess(t, status)
	sourcePod := h.getPodOrdinal(t, instance, source.Ordinal)
	sourceAddress := net.JoinHostPort(sourcePod.Status.PodIP, "6379")
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "rz10-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "rz10-key", "preserved")

	beforeSourceACL := newValkeyCommandPoint(
		t,
		sourceAddress,
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandBefore,
		false,
	)
	startedAt := time.Now()
	h.resize(t, instance, 2, 2, 2)
	beforeSourceACL.waitFor(t, 3*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Failover == nil || status.Status.Failover.Candidate == nil ||
		status.Status.Failover.Stage != valkeyv1alpha1.FailoverStageFencing {
		t.Fatalf("RZ-10 не сохранила плановый failover: %+v", status.Status.Failover)
	}
	candidate := processForIdentity(t, status.Status.Nodes, *status.Status.Failover.Candidate)
	var delayed valkeyv1alpha1.NodeStatus
	foundDelayed := false
	for _, process := range status.Status.Nodes {
		if process.Ordinal != source.Ordinal && process.Ordinal != candidate.Ordinal {
			delayed = process
			foundDelayed = true
			break
		}
	}
	if !foundDelayed {
		t.Fatalf("RZ-10 не нашла вторую реплику: %+v", status.Status.Nodes)
	}
	delayedPod := h.getPodOrdinal(t, instance, delayed.Ordinal)
	delayedAddress := net.JoinHostPort(delayedPod.Status.PodIP, "6379")
	beforeFollow := newValkeyCommandPoint(
		t,
		delayedAddress,
		"REPLICAOF",
		operatorvalkey.IntegrationCommandBefore,
		false,
	)
	beforeSourceACL.close()
	beforeFollow.waitFor(t, 45*time.Second)
	status = h.getInstance(t, instance)
	if status.Status.Failover == nil ||
		status.Status.Failover.Stage != valkeyv1alpha1.FailoverStageReconfiguring ||
		status.Status.PrimaryPodUID != candidate.PodUID || status.Status.Rollout == nil {
		t.Fatalf(
			"RZ-10 не удержала reconfiguring: failover=%+v rollout=%+v",
			status.Status.Failover,
			status.Status.Rollout,
		)
	}
	currentSource := processAtOrdinal(t, status, source.Ordinal)
	if currentSource.PodUID != source.PodUID {
		t.Fatalf("RZ-10 заменила бывший primary до прямого upstream: %s -> %s", source.PodUID, currentSource.PodUID)
	}
	delayedInfo := h.directReplicationInfo(t, instance, delayed)
	if delayedInfo["master_host"] != sourcePod.Status.PodIP {
		t.Fatalf("RZ-10 не удержала прежний upstream: %q != %q", delayedInfo["master_host"], sourcePod.Status.PodIP)
	}
	assertRealProcessRoleACL(
		t,
		h,
		instance,
		currentSource,
		h.directValkeyRole(t, instance, currentSource),
		instance.password,
		false,
	)
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})

	beforeFollow.close()
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		updatedSource, found := nodeStatusAtOrdinal(current.Status.Nodes, source.Ordinal)
		return found && updatedSource.PodUID != source.PodUID && current.Status.Failover == nil &&
			current.Status.Rollout == nil && current.Status.PrimaryPodUID == candidate.PodUID &&
			current.Status.Applied != nil && current.Status.Applied.VCPU == 2 &&
			current.Status.Applied.RAMGB == 2 && current.Status.ObservedGeneration == 2 &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "RZ-10 замены бывшего primary после прямой синхронизации")
	t.Logf("RZ-10 плановое переключение и завершение rollout: %s", time.Since(startedAt))
	assertHAComposition(t, h, instance, status)
	currentDelayed := processAtOrdinal(t, status, delayed.Ordinal)
	currentPrimary := primaryProcess(t, status)
	currentPrimaryPod := h.getPodOrdinal(t, instance, currentPrimary.Ordinal)
	if currentDelayed.Replication == nil || currentDelayed.Replication.UpstreamHost != currentPrimaryPod.Status.PodIP ||
		!currentDelayed.Replication.LinkUp || currentDelayed.Replication.SyncInProgress {
		t.Fatalf("RZ-10 не подтвердила прямой upstream: %+v", currentDelayed.Replication)
	}
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "rz10-key"); value != "preserved" {
		t.Fatalf("RZ-10 потеряла контрольный ключ: %q", value)
	}
	closeConnections([]*persistentConnection{connection})
	h.deleteInstance(t, instance)
}

func Test_OP07_TakeOverOperator_WithDelayedAdministrativeConnection_ClosesPreviousConnection(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "opseven")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	primary := primaryProcess(t, status)
	primaryPod := h.getPodOrdinal(t, instance, primary.Ordinal)
	operatorPassword := h.servicePasswords(t, instance)[0]
	beforeObservedAt := status.Status.ObservedAt.DeepCopy()

	h.stopOperator(t)
	connection, err := net.DialTimeout("tcp", net.JoinHostPort(primaryPod.Status.PodIP, "6379"), 3*time.Second)
	if err != nil {
		t.Fatalf("открыть прежнее административное соединение: %v", err)
	}
	delayed := &persistentConnection{conn: connection, read: bufio.NewReader(connection), stop: func() {}}
	writeRESP(t, delayed, "AUTH", "operator", operatorPassword)
	if response := readRESP(t, delayed); response != "OK" {
		t.Fatalf("OP-07 AUTH прежнего оператора вернул %#v", response)
	}
	pingConnections(t, []*persistentConnection{delayed})

	h.startOperator(t)
	h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return beforeObservedAt != nil && current.Status.ObservedAt != nil &&
			current.Status.ObservedAt.After(beforeObservedAt.Time) &&
			current.Status.PrimaryPodUID == primary.PodUID &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "OP-07 приёма управления новым оператором")
	assertConnectionClosed(t, delayed, false)
	closeConnections([]*persistentConnection{delayed})
	assertRealProcessRoleACL(t, h, instance, primary, "master", instance.password, true)
	clientConnection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	pingConnections(t, []*persistentConnection{clientConnection})
	closeConnections([]*persistentConnection{clientConnection})
	h.deleteInstance(t, instance)
}

func Test_OP02_ResumeRollout_AfterOperatorLosesLease_UsesSuccessorWithoutDataLoss(t *testing.T) {
	h := newHarness(t)
	h.startClusterOperator(t)
	t.Cleanup(func() {
		h.stopClusterOperator(t)
		h.close(t)
	})
	h.scaleClusterOperator(t, 2)
	h.waitForClusterOperatorPod(t, clusterOperatorName+"-1", "", "", -1)

	instance := h.createHA(t, "optwo")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "op02-key", "preserved")

	h.resize(t, instance, 2, 2, 2)
	h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Rollout != nil &&
			current.Status.Rollout.Stage == valkeyv1alpha1.RolloutStageReplacingReplicas
	}, "OP-02 выполняемого rollout перед потерей Lease")
	lease := h.operatorLease(t)
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		t.Fatal("OP-02 Lease не имеет владельца")
	}
	previousHolder := *lease.Spec.HolderIdentity
	leaderPodName := strings.Split(previousHolder, "_")[0]
	leaderPod := h.waitForClusterOperatorPod(t, leaderPodName, "", "", -1)
	leaderContainer := podContainer(t, leaderPod, "operator")
	forcedHolder := "op02-forced-holder"
	h.forceOperatorLeaseHolder(t, forcedHolder)
	h.waitForClusterOperatorPod(
		t,
		leaderPodName,
		leaderPod.UID,
		leaderContainer.ContainerID,
		leaderContainer.RestartCount,
	)
	status = h.getInstance(t, instance)
	if status.Status.Rollout == nil {
		t.Fatal("OP-02 rollout завершился до фактической потери Lease")
	}
	h.waitForOperatorLeaseHolder(t, previousHolder, forcedHolder)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Rollout == nil && current.Status.Failover == nil &&
			current.Status.Applied != nil && current.Status.Applied.VCPU == 2 &&
			current.Status.Applied.RAMGB == 2 && current.Status.ObservedGeneration == 2 &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "OP-02 продолжения rollout новым владельцем Lease")
	assertHAComposition(t, h, instance, status)
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "op02-key"); value != "preserved" {
		t.Fatalf("OP-02 потеряла контрольный ключ: %q", value)
	}
	closeConnections([]*persistentConnection{connection})
	h.deleteInstance(t, instance)
}

func Test_FP04_DeleteSinglePod_ReplacesProcessWithEmptyCacheAndStableCredentials(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createSingle(t, "deleteone", valkeyv1alpha1.WhitelistSpec{})
	status := h.waitRunning(t, instance)
	if len(status.Status.Nodes) != 1 {
		t.Fatalf("FP-04 получил неверный состав single: %+v", status.Status.Nodes)
	}
	oldProcess := status.Status.Nodes[0]
	servicePasswords := h.servicePasswords(t, instance)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "delete-single-key", "discarded")

	startedAt := time.Now()
	h.deletePod(t, instance, oldProcess.Ordinal)
	h.waitForReplacementOrdinal(t, instance, oldProcess.Ordinal, types.UID(oldProcess.PodUID))
	status = h.waitRunning(t, instance)
	t.Logf("FP-04 DELETE single: %s", time.Since(startedAt))
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	if len(status.Status.Nodes) != 1 || status.Status.Nodes[0].PodUID == oldProcess.PodUID {
		t.Fatalf("FP-04 не заменил процесс single: %+v", status.Status.Nodes)
	}
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("FP-04 изменил служебные пароли single")
	}
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	writeRESP(t, connection, "GET", "delete-single-key")
	if value := readRESP(t, connection); value != nil {
		t.Fatalf("FP-04 восстановил прежний кэш single: %#v", value)
	}

	h.deleteInstance(t, instance)
}

func assertCT10IndependentInstanceObservation(
	t *testing.T,
	h *harness,
	blocked *testInstance,
	blockedStatus *valkeyv1alpha1.ValkeyInstance,
	healthy *testInstance,
	healthyStatus *valkeyv1alpha1.ValkeyInstance,
) {
	t.Helper()
	if len(blockedStatus.Status.Nodes) == 0 {
		t.Fatal("CT-10 недоступный инстанс не содержит процессов")
	}
	blockedProcess := blockedStatus.Status.Nodes[0]
	for _, process := range blockedStatus.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica {
			blockedProcess = process
			break
		}
	}
	beforeObservedAt := healthyStatus.Status.ObservedAt.DeepCopy()
	if beforeObservedAt == nil {
		t.Fatal("CT-10 исправный инстанс не имеет исходного heartbeat")
	}

	connection := openPersistentConnection(t, h.publicAddr, healthy, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	pingConnections(t, []*persistentConnection{connection})

	h.signalValkeyProcess(t, blocked, blockedProcess, "STOP")
	continued := false
	defer func() {
		if !continued {
			h.signalValkeyProcess(t, blocked, blockedProcess, "CONT")
		}
	}()
	h.waitForValkeyProcessState(t, blocked, blockedProcess, "T", "CT-10 SIGSTOP недоступного инстанса")

	startedAt := time.Now()
	lastObservedAt := beforeObservedAt.Time
	heartbeats := make([]time.Time, 0, 2)
	sawTransportFailure := false
	observationDeadline := 4*operatorconfig.ValkeyCommandTimeout + 4*operatorconfig.HealthCheckInterval
	err := wait.PollUntilContextTimeout(
		t.Context(),
		50*time.Millisecond,
		observationDeadline,
		true,
		func(ctx context.Context) (bool, error) {
			blockedCurrent := &valkeyv1alpha1.ValkeyInstance{}
			if err := h.k8s.Get(ctx, client.ObjectKey{
				Name: blocked.slug, Namespace: blocked.namespace,
			}, blockedCurrent); err != nil {
				return false, err
			}
			currentProcess, found := nodeStatusAtOrdinal(blockedCurrent.Status.Nodes, blockedProcess.Ordinal)
			if !found || currentProcess.PodUID != blockedProcess.PodUID ||
				currentProcess.ContainerID != blockedProcess.ContainerID {
				return false, fmt.Errorf("недоступный процесс заменён во время CT-10")
			}
			if currentProcess.Observation != nil &&
				currentProcess.Observation.Kind == valkeyv1alpha1.ProcessObservationTransportError {
				sawTransportFailure = true
			}

			healthyCurrent := &valkeyv1alpha1.ValkeyInstance{}
			if err := h.k8s.Get(ctx, client.ObjectKey{
				Name: healthy.slug, Namespace: healthy.namespace,
			}, healthyCurrent); err != nil {
				return false, err
			}
			if healthyCurrent.Status.ObservedAt != nil &&
				healthyCurrent.Status.ObservedAt.After(lastObservedAt) {
				lastObservedAt = healthyCurrent.Status.ObservedAt.Time
				heartbeats = append(heartbeats, lastObservedAt)
			}

			return sawTransportFailure && len(heartbeats) >= 2, nil
		},
	)
	if err != nil {
		t.Fatalf(
			"CT-10 исправный инстанс не продолжил наблюдение за %s: %v; heartbeats=%v transport_failure=%t",
			observationDeadline,
			err,
			heartbeats,
			sawTransportFailure,
		)
	}
	maximumHeartbeatGap := 2*operatorconfig.ValkeyCommandTimeout + 4*operatorconfig.HealthCheckInterval
	previous := beforeObservedAt.Time
	for _, heartbeat := range heartbeats {
		if gap := heartbeat.Sub(previous); gap > maximumHeartbeatGap {
			t.Fatalf("CT-10 heartbeat исправного инстанса задержался на %s", gap)
		}
		previous = heartbeat
	}
	pingConnections(t, []*persistentConnection{connection})
	t.Logf(
		"CT-10: транспортный отказ и %d новых heartbeat исправного соседа подтверждены за %s",
		len(heartbeats),
		time.Since(startedAt),
	)

	h.signalValkeyProcess(t, blocked, blockedProcess, "CONT")
	continued = true
	h.waitForValkeyProcessState(t, blocked, blockedProcess, "S", "CT-10 SIGCONT недоступного инстанса")
	h.waitForSameProcessHealthy(t, blocked, blockedProcess, "CT-10 восстановления недоступного инстанса")
}

func Test_NT06_ApplyHAChanges_WhenEnvoyAdminAPIIsUnavailable_ContinuesObservationAndMutations(t *testing.T) {
	testStartedAt := time.Now()
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	phaseStartedAt := time.Now()
	instance := h.createHA(t, "ntenvoy")
	status := h.waitRunning(t, instance)
	t.Logf("NT-06 создание HA: %s", time.Since(phaseStartedAt))
	assertHAComposition(t, h, instance, status)
	servicePasswords := h.servicePasswords(t, instance)
	beforePrimaryUID := status.Status.PrimaryPodUID
	beforeObservedAt := status.Status.ObservedAt.DeepCopy()
	if beforeObservedAt == nil || status.Status.Network.VerifiedAt == nil {
		t.Fatalf("NT-06 не получил исходные времена: %+v", status.Status)
	}

	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "ha-key", "preserved")
	restoreEnvoyAdminAccess := h.removeOperatorEnvoyAdminAccess(t)
	h.recordFaultEvent(t, "envoy-admin-removed", "operator/envoy-admin")
	t.Cleanup(func() {
		if err := restoreEnvoyAdminAccess(); err != nil {
			t.Errorf("восстановить доступ оператора к admin API Envoy: %v", err)
		}
	})

	phaseStartedAt = time.Now()
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Network != nil &&
			current.Status.Network.VerificationStatus == valkeyv1alpha1.NetworkVerificationUnknown &&
			current.Status.ObservedAt != nil && current.Status.ObservedAt.After(beforeObservedAt.Time)
	}, "NT-06 ошибки admin API Envoy с новым heartbeat")
	t.Logf("NT-06 обнаружение недоступного admin API с heartbeat: %s", time.Since(phaseStartedAt))
	if status.Status.Failover != nil || status.Status.PrimaryPodUID != beforePrimaryUID ||
		status.Status.Phase == valkeyv1alpha1.InstancePhaseUnavailable {
		t.Fatalf("NT-06 запустил ложное восстановление: %+v", status.Status)
	}
	outageVerifiedAt := status.Status.Network.VerifiedAt.DeepCopy()
	if outageVerifiedAt == nil {
		t.Fatal("NT-06 потерял время последней успешной сверки")
	}
	pingConnections(t, []*persistentConnection{connection})

	oldPassword := instance.password
	newPassword := "nt06-" + mustUUIDv7(t)
	phaseStartedAt = time.Now()
	h.rotatePassword(t, instance, newPassword, 2)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.CredentialRotation == nil && current.Status.AppliedPasswordVersion == 2 &&
			current.Status.ObservedGeneration == 2 && current.Status.Network != nil &&
			current.Status.Network.VerificationStatus == valkeyv1alpha1.NetworkVerificationUnknown
	}, "NT-06 ротации при недоступном admin API Envoy")
	t.Logf("NT-06 ротация при недоступном admin API: %s", time.Since(phaseStartedAt))
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	instance.password = newPassword
	assertAuthenticationRejected(t, h, instance, oldPassword)
	assertPasswordsOnAllProcesses(t, h, instance, oldPassword, newPassword)
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("NT-06 изменила служебные пароли")
	}

	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "ha-key"); value != "preserved" {
		t.Fatalf("NT-06 потеряла ключ после ротации: %q", value)
	}
	phaseStartedAt = time.Now()
	connection = resizeHAAndAssertPreserved(t, h, instance, connection, 2, 2, 3, "preserved")
	t.Logf("NT-06 HA-ресайз при недоступном admin API: %s", time.Since(phaseStartedAt))
	defer closeConnections([]*persistentConnection{connection})
	status = h.getInstance(t, instance)
	if status.Status.Network == nil ||
		status.Status.Network.VerificationStatus != valkeyv1alpha1.NetworkVerificationUnknown ||
		status.Status.Failover != nil || !status.Status.Network.VerifiedAt.Equal(outageVerifiedAt) {
		t.Fatalf("NT-06 неверное состояние после ресайза: %+v", status.Status)
	}

	if err := restoreEnvoyAdminAccess(); err != nil {
		t.Fatalf("восстановить доступ оператора к admin API Envoy: %v", err)
	}
	phaseStartedAt = time.Now()
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Network != nil &&
			current.Status.Network.VerificationStatus == valkeyv1alpha1.NetworkVerificationVerified &&
			current.Status.Network.VerifiedAt != nil && current.Status.Network.VerifiedAt.After(outageVerifiedAt.Time)
	}, "NT-06 восстановления сверки Envoy")
	t.Logf("NT-06 восстановление сверки Envoy: %s", time.Since(phaseStartedAt))
	assertHAComposition(t, h, instance, status)
	pingConnections(t, []*persistentConnection{connection})
	phaseStartedAt = time.Now()
	h.deleteInstance(t, instance)
	t.Logf("NT-06 удаление HA: %s", time.Since(phaseStartedAt))
	t.Logf("NT-06 всего: %s", time.Since(testStartedAt))
}

func Test_NT01NT02NT03_RecoverAddressedNetworkPartitions_WhenOperatorOrAgentLinksFail_PreservesProcessesAndData(
	t *testing.T,
) {
	testStartedAt := time.Now()
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "ntlinks")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	servicePasswords := h.servicePasswords(t, instance)
	primary := primaryProcess(t, status)
	primaryPod := h.getPodOrdinal(t, instance, primary.Ordinal)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer func() { closeConnections([]*persistentConnection{connection}) }()
	setValue(t, connection, "network-key", "preserved")
	partitionWindow := max(
		operatorconfig.EmptyPrimaryTimeout,
		operatorconfig.ProcessUnresponsiveTimeout,
	) + 2*operatorconfig.HealthCheckInterval

	phaseStartedAt := time.Now()
	identities := processIdentities(status.Status.Nodes)
	operatorFault := h.addNodeNetworkFault(
		t,
		"nt01-operator-valkey",
		primary.NodeName,
		"FORWARD",
		podRouteSourceCIDR(t, h.k8s),
		hostCIDR(t, primaryPod.Status.PodIP),
		6379,
	)
	operatorFault.assertPresent(t)
	assertTCPBlocked(t, net.JoinHostPort(primaryPod.Status.PodIP, "6379"))
	observeSafetyWindow(t, partitionWindow, func() {
		current := h.getInstance(t, instance)
		currentPrimary := processAtOrdinal(t, current, primary.Ordinal)
		if current.Status.PrimaryPodUID != primary.PodUID || !sameNodeProcess(currentPrimary, primary) ||
			!sameProcessIdentityMap(identities, processIdentities(current.Status.Nodes)) {
			t.Fatalf("NT-01 заменила живой primary или другой процесс: %+v", current.Status.Nodes)
		}
		if current.Status.Failover != nil &&
			(current.Status.Failover.Stage != valkeyv1alpha1.FailoverStageFencing ||
				current.Status.Failover.Candidate != nil ||
				current.Status.Failover.Source != processIdentityForTest(primary)) {
			t.Fatalf("NT-01 продолжила переключение без fencing: %+v", current.Status.Failover)
		}
		pingConnections(t, []*persistentConnection{connection})
		for _, process := range current.Status.Nodes {
			if process.Role == valkeyv1alpha1.NodeRoleReplica && h.directValkeyRole(t, instance, process) != "slave" {
				t.Fatalf("NT-01 продвинула реплику ordinal %d", process.Ordinal)
			}
		}
	})
	current := h.getInstance(t, instance)
	if current.Status.Failover == nil ||
		current.Status.Failover.Stage != valkeyv1alpha1.FailoverStageFencing {
		t.Fatalf("NT-01 не дошла до ожидающего fencing: %+v", current.Status.Failover)
	}
	t.Logf("NT-01 отрицательное окно: %s", time.Since(phaseStartedAt))
	closeConnections([]*persistentConnection{connection})
	if err := operatorFault.restore(); err != nil {
		t.Fatalf("снять разрыв operator/Valkey: %v", err)
	}
	operatorFault.assertAbsent(t)
	status = h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "network-key"); value != "preserved" {
		t.Fatalf("NT-01 потеряла контрольный ключ: %q", value)
	}

	status = h.waitRunning(t, instance)
	phaseStartedAt = time.Now()
	var agentProcess valkeyv1alpha1.NodeStatus
	foundAgent := false
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica && process.NodeName != "k3s-server" {
			agentProcess = process
			foundAgent = true
			break
		}
	}
	if !foundAgent {
		t.Fatalf("NT-02 не нашла реплику на agent: %+v", status.Status.Nodes)
	}
	agentPod := h.getPodOrdinal(t, instance, agentProcess.Ordinal)
	agentAddress := h.nodeInternalAddress(t, agentProcess.NodeName)
	serverAddress := requiredEnv(t, "K3S_SERVER_IP")
	h.assertAgentControlPlane(t, agentProcess.NodeName, serverAddress, true)
	agentFault := h.addNodeNetworkFault(
		t,
		"nt02-agent-control-plane",
		agentProcess.NodeName,
		"OUTPUT",
		hostCIDR(t, agentAddress),
		hostCIDR(t, serverAddress),
		6443,
	)
	agentFault.assertPresent(t)
	h.assertAgentControlPlane(t, agentProcess.NodeName, serverAddress, false)
	agentNode := h.waitSameNodeReadyState(t, agentProcess.NodeName, types.UID(agentProcess.NodeUID), false)
	identities = processIdentities(status.Status.Nodes)
	observeSafetyWindow(t, max(
		operatorconfig.ProcessUnresponsiveTimeout,
		operatorconfig.PrimaryFailureMinDuration,
	)+2*operatorconfig.HealthCheckInterval, func() {
		current := h.getInstance(t, instance)
		if !sameProcessIdentityMap(identities, processIdentities(current.Status.Nodes)) ||
			current.Status.Failover != nil {
			t.Fatalf("NT-02 заменила процесс или начала failover: %+v", current.Status)
		}
		currentAgent := processAtOrdinal(t, current, agentProcess.Ordinal)
		if currentAgent.Recovery != nil {
			t.Fatalf("NT-02 начала удаление процесса на NotReady Node: %+v", currentAgent.Recovery)
		}
		observedNode := h.getNode(t, agentProcess.NodeName)
		if observedNode.UID != agentNode.UID || nodeReady(observedNode.Status.Conditions) {
			t.Fatalf("NT-02 потеряла NotReady Node или её UID: %+v", observedNode)
		}
		pingConnections(t, []*persistentConnection{connection})
	})
	if role := directValkeyRoleAtAddress(
		t,
		net.JoinHostPort(agentPod.Status.PodIP, "6379"),
		servicePasswords[0],
	); role != "slave" {
		t.Fatalf("NT-02 процесс на отделённом agent сменил роль: %s", role)
	}
	if err := agentFault.restore(); err != nil {
		t.Fatalf("снять разрыв agent/control-plane: %v", err)
	}
	agentFault.assertAbsent(t)
	h.assertAgentControlPlane(t, agentProcess.NodeName, serverAddress, true)
	h.waitSameNodeReadyState(t, agentProcess.NodeName, agentNode.UID, true)
	h.waitForSameProcessHealthy(t, instance, agentProcess, "NT-02 восстановления agent/control-plane")
	if value := getValue(t, connection, "network-key"); value != "preserved" {
		t.Fatalf("NT-01/NT-02 потеряли контрольный ключ: %q", value)
	}
	t.Logf("NT-02 разрыв и восстановление: %s", time.Since(phaseStartedAt))

	status = h.waitRunning(t, instance)
	phaseStartedAt = time.Now()
	identities = processIdentities(status.Status.Nodes)
	primary = primaryProcess(t, status)
	operatorOnlyFaults := make([]*nodeNetworkFault, 0, len(status.Status.Nodes))
	for _, process := range status.Status.Nodes {
		pod := h.getPodOrdinal(t, instance, process.Ordinal)
		fault := h.addNodeNetworkFault(
			t,
			fmt.Sprintf("nt03-operator-valkey-%d", process.Ordinal),
			process.NodeName,
			"FORWARD",
			podRouteSourceCIDR(t, h.k8s),
			hostCIDR(t, pod.Status.PodIP),
			6379,
		)
		fault.assertPresent(t)
		assertTCPBlocked(t, net.JoinHostPort(pod.Status.PodIP, "6379"))
		operatorOnlyFaults = append(operatorOnlyFaults, fault)
	}
	observeSafetyWindow(t, partitionWindow, func() {
		current := h.getInstance(t, instance)
		if !sameProcessIdentityMap(identities, processIdentities(current.Status.Nodes)) {
			t.Fatalf("NT-03 заменила процессы при отказе только сети оператора: %+v", current.Status.Nodes)
		}
		if current.Status.Failover != nil &&
			(current.Status.Failover.Stage != valkeyv1alpha1.FailoverStageFencing ||
				current.Status.Failover.Candidate != nil ||
				current.Status.Failover.Source != processIdentityForTest(primary)) {
			t.Fatalf("NT-03 продолжила failover без fencing: %+v", current.Status.Failover)
		}
		for _, process := range current.Status.Nodes {
			if process.Recovery != nil {
				t.Fatalf("NT-03 начала удаление Ready процесса ordinal %d: %+v", process.Ordinal, process.Recovery)
			}
			node := h.getNode(t, process.NodeName)
			if !nodeReady(node.Status.Conditions) || string(node.UID) != process.NodeUID {
				t.Fatalf("NT-03 изменила состояние Node процесса ordinal %d: %+v", process.Ordinal, node.Status)
			}
			expectedRole := "slave"
			if process.Ordinal == primary.Ordinal {
				expectedRole = "master"
			}
			if role := h.directValkeyRole(t, instance, process); role != expectedRole {
				t.Fatalf("NT-03 изменила роль ordinal %d: %s", process.Ordinal, role)
			}
		}
		pingConnections(t, []*persistentConnection{connection})
	})
	current = h.getInstance(t, instance)
	for _, process := range current.Status.Nodes {
		if process.Observation == nil ||
			process.Observation.Kind != valkeyv1alpha1.ProcessObservationTransportError {
			t.Fatalf("NT-03 не наблюдала транспортный отказ ordinal %d: %+v", process.Ordinal, process.Observation)
		}
	}
	closeConnections([]*persistentConnection{connection})
	for _, fault := range operatorOnlyFaults {
		if err := fault.restore(); err != nil {
			t.Fatalf("снять разрыв сети оператора: %v", err)
		}
		fault.assertAbsent(t)
	}
	status = h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "network-key"); value != "preserved" {
		t.Fatalf("NT-03 потеряла контрольный ключ: %q", value)
	}
	t.Logf("NT-03 разрыв и восстановление: %s", time.Since(phaseStartedAt))

	h.deleteInstance(t, instance)
	t.Logf("NT-01/NT-02/NT-03 всего: %s", time.Since(testStartedAt))
}

func observeSafetyWindow(t *testing.T, duration time.Duration, check func()) {
	t.Helper()
	startedAt := time.Now()
	err := wait.PollUntilContextTimeout(
		t.Context(), time.Second, duration+5*time.Second, true,
		func(context.Context) (bool, error) {
			check()
			return time.Since(startedAt) >= duration, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться отрицательного окна %s: %v", duration, err)
	}
}

func (h *harness) waitSameNodeReadyState(
	t *testing.T,
	name string,
	uid types.UID,
	ready bool,
) *corev1.Node {
	t.Helper()
	var result corev1.Node
	err := wait.PollUntilContextTimeout(
		t.Context(), 500*time.Millisecond, 90*time.Second, true,
		func(ctx context.Context) (bool, error) {
			node := &corev1.Node{}
			if err := h.k8s.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
				return false, err
			}
			if node.UID != uid {
				return false, fmt.Errorf("Node %s сменила UID %s на %s", name, uid, node.UID)
			}
			if nodeReady(node.Status.Conditions) != ready {
				return false, nil
			}
			result = *node.DeepCopy()
			return true, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться Ready=%t той же Node %s: %v", ready, name, err)
	}
	return result.DeepCopy()
}

func (h *harness) getNode(t *testing.T, name string) *corev1.Node {
	t.Helper()
	node := &corev1.Node{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{Name: name}, node); err != nil {
		t.Fatalf("прочитать Node %s: %v", name, err)
	}
	return node
}

func Test_OP04_ResumePasswordRotation_AfterValkeyCommandRestartsAndLostResponse_AvoidsDuplicateEffects(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createSingle(t, "opcommand", valkeyv1alpha1.WhitelistSpec{})
	status := h.waitRunning(t, instance)
	process := processAtOrdinal(t, status, 0)
	pod := h.getPodOrdinal(t, instance, process.Ordinal)
	address := net.JoinHostPort(pod.Status.PodIP, "6379")
	servicePasswords := h.servicePasswords(t, instance)

	beforePassword := instance.password
	passwordTwo := "op-before-" + mustUUIDv7(t)
	beforePoint := newValkeyCommandPoint(
		t,
		address,
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandBefore,
		false,
	)
	h.rotatePassword(t, instance, passwordTwo, 2)
	beforePoint.wait(t)
	assertRealProcessRoleACL(t, h, instance, process, "master", beforePassword, true)
	h.stopOperator(t)
	beforePoint.close()
	h.startOperator(t)
	status = h.waitForPasswordVersion(t, instance, 2)
	assertSameProcess(t, process, status.Status.Nodes[0], "OP-04 рестарт до ACL SETUSER")
	assertRealProcessRoleACL(t, h, instance, process, "master", passwordTwo, true)
	instance.password = passwordTwo

	passwordThree := "op-after-" + mustUUIDv7(t)
	afterPoint := newValkeyCommandPoint(
		t,
		address,
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandAfter,
		false,
	)
	h.rotatePassword(t, instance, passwordThree, 3)
	afterPoint.wait(t)
	assertRealProcessRoleACL(t, h, instance, process, "master", passwordThree, true)
	h.stopOperator(t)
	afterPoint.close()
	h.startOperator(t)
	status = h.waitForPasswordVersion(t, instance, 3)
	assertSameProcess(t, process, status.Status.Nodes[0], "OP-04 рестарт после ACL SETUSER")
	assertRealProcessRoleACL(t, h, instance, process, "master", passwordThree, true)
	instance.password = passwordThree

	passwordFour := "op-lost-" + mustUUIDv7(t)
	lostPoint := newValkeyCommandPoint(
		t,
		address,
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandAfter,
		true,
	)
	h.rotatePassword(t, instance, passwordFour, 4)
	lostPoint.wait(t)
	assertRealProcessRoleACL(t, h, instance, process, "master", passwordFour, true)
	lostPoint.close()
	status = h.waitForPasswordVersion(t, instance, 4)
	assertSameProcess(t, process, status.Status.Nodes[0], "OP-04 потерянный ответ ACL SETUSER")
	assertRealProcessRoleACL(t, h, instance, process, "master", passwordFour, true)
	instance.password = passwordFour
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("OP-04 изменила служебные пароли")
	}

	h.deleteInstance(t, instance)
}

func Test_NT04_ReconcileDuringKubernetesOutage_WithStaleState_BlocksActionsAndRecovers(t *testing.T) {
	h := newHarness(t)
	kubernetesProxy := newTCPFaultProxy(t, strings.TrimPrefix(h.operatorREST.Host, "https://"))
	h.operatorREST.Host = "https://" + kubernetesProxy.address()
	t.Cleanup(kubernetesProxy.close)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })
	t.Cleanup(func() { kubernetesProxy.setBlocked(false) })

	instance := h.createHA(t, "ntkube")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	beforeIdentities := processIdentities(status.Status.Nodes)
	primary := primaryProcess(t, status)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	setValue(t, connection, "nt04-key", "preserved")

	kubernetesProxy.setBlocked(true)
	h.recordFaultEvent(t, "network-applied", "operator/kubernetes")
	stableSince := time.Now()
	lastObservedAt := status.Status.ObservedAt.DeepCopy()
	if lastObservedAt == nil {
		t.Fatal("NT-04 не получил исходный heartbeat")
	}
	stableWindow := 2 * operatorconfig.HealthCheckInterval
	err := wait.PollUntilContextTimeout(
		t.Context(),
		50*time.Millisecond,
		2*operatorconfig.ValkeyCommandTimeout+4*operatorconfig.HealthCheckInterval,
		true,
		func(ctx context.Context) (bool, error) {
			current := &valkeyv1alpha1.ValkeyInstance{}
			if err := h.k8s.Get(ctx, client.ObjectKey{
				Name: instance.slug, Namespace: instance.namespace,
			}, current); err != nil {
				return false, err
			}
			if current.Status.ObservedAt != nil && current.Status.ObservedAt.After(lastObservedAt.Time) {
				lastObservedAt = current.Status.ObservedAt.DeepCopy()
				stableSince = time.Now()
			}
			return time.Since(stableSince) >= stableWindow, nil
		},
	)
	if err != nil {
		t.Fatalf("NT-04 heartbeat не остановился после разрыва Kubernetes: %v", err)
	}

	h.signalValkeyProcess(t, instance, primary, "STOP")
	continued := false
	defer func() {
		if !continued {
			h.signalValkeyProcess(t, instance, primary, "CONT")
		}
	}()
	h.waitForValkeyProcessState(t, instance, primary, "T", "NT-04 SIGSTOP primary")
	negativeWindow := operatorconfig.EmptyPrimaryTimeout +
		2*operatorconfig.HealthCheckInterval
	negativeDeadline := time.Now().Add(negativeWindow)
	err = wait.PollUntilContextTimeout(
		t.Context(),
		250*time.Millisecond,
		negativeWindow+time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			if !time.Now().Before(negativeDeadline) {
				return true, nil
			}
			current := &valkeyv1alpha1.ValkeyInstance{}
			if err := h.k8s.Get(ctx, client.ObjectKey{
				Name: instance.slug, Namespace: instance.namespace,
			}, current); err != nil {
				return false, err
			}
			if current.Status.ObservedAt == nil || !current.Status.ObservedAt.Equal(lastObservedAt) {
				return false, fmt.Errorf("observedAt изменился без свежего Kubernetes: %v", current.Status.ObservedAt)
			}
			if current.Status.Failover != nil || current.Status.PrimaryPodUID != primary.PodUID {
				return false, fmt.Errorf("начато действие по старому снимку: %+v", current.Status.Failover)
			}
			return false, nil
		},
	)
	if err != nil {
		t.Fatalf("NT-04 отрицательное окно завершилось раньше срока: %v", err)
	}
	for _, process := range status.Status.Nodes {
		pod := h.getPodOrdinal(t, instance, process.Ordinal)
		if string(pod.UID) != process.PodUID {
			t.Fatalf("NT-04 заменил Pod ordinal %d без Kubernetes API", process.Ordinal)
		}
		if process.Role == valkeyv1alpha1.NodeRoleReplica {
			role := h.directValkeyRole(t, instance, process)
			if role != "slave" {
				t.Fatalf("NT-04 продвинул реплику ordinal %d: %s", process.Ordinal, role)
			}
		}
	}
	if current := processIdentities(
		h.getInstance(t, instance).Status.Nodes,
	); !sameProcessIdentityMap(
		beforeIdentities,
		current,
	) {
		t.Fatalf("NT-04 изменил идентичности по старому снимку: before=%v after=%v", beforeIdentities, current)
	}

	h.signalValkeyProcess(t, instance, primary, "CONT")
	continued = true
	h.waitForValkeyProcessState(t, instance, primary, "S", "NT-04 SIGCONT primary")
	pingConnections(t, []*persistentConnection{connection})
	kubernetesProxy.setBlocked(false)
	h.recordFaultEvent(t, "network-removed", "operator/kubernetes")
	recovered := h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.ObservedAt != nil && current.Status.ObservedAt.After(lastObservedAt.Time) &&
			current.Status.Failover == nil && current.Status.PrimaryPodUID == primary.PodUID &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "NT-04 свежего наблюдения после восстановления Kubernetes")
	assertHAComposition(t, h, instance, recovered)
	if value := getValue(t, connection, "nt04-key"); value != "preserved" {
		t.Fatalf("NT-04 потерял ключ после восстановления Kubernetes: %q", value)
	}
	h.deleteInstance(t, instance)
}

func Test_NT05_FailOverDuringReplicationPartition_WithEmptyReplacement_ExcludesUnsynchronizedReplica(t *testing.T) {
	testStartedAt := time.Now()
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "ntreplication")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	primary := primaryProcess(t, status)
	replicas := make([]valkeyv1alpha1.NodeStatus, 0, 2)
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica {
			replicas = append(replicas, process)
		}
	}
	if len(replicas) != 2 {
		t.Fatalf("NT-05 ожидала две реплики: %+v", status.Status.Nodes)
	}
	target := replicas[0]
	survivor := replicas[1]
	targetNode := h.getNode(t, target.NodeName)
	if targetNode.Spec.PodCIDR == "" {
		t.Fatalf("NT-05 Node %s не содержит PodCIDR", target.NodeName)
	}
	h.useSurvivingEnvoy(t, target.NodeName)

	cordonedNodes := make([]string, 0)
	nodes := &corev1.NodeList{}
	if err := h.k8s.List(t.Context(), nodes); err != nil {
		t.Fatalf("NT-05 прочитать Node: %v", err)
	}
	for index := range nodes.Items {
		node := &nodes.Items[index]
		if node.Name == primary.NodeName || node.Name == target.NodeName || node.Spec.Unschedulable {
			continue
		}
		h.setNodeUnschedulable(t, node.Name, true)
		cordonedNodes = append(cordonedNodes, node.Name)
	}
	t.Cleanup(func() {
		for _, nodeName := range cordonedNodes {
			h.setNodeUnschedulable(t, nodeName, false)
		}
	})

	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer func() { closeConnections([]*persistentConnection{connection}) }()
	setValue(t, connection, "nt05-baseline", "preserved")
	h.waitForReplicaValue(t, instance, target, "nt05-baseline", "preserved")
	h.waitForReplicaValue(t, instance, survivor, "nt05-baseline", "preserved")

	phaseStartedAt := time.Now()
	replicationFault := h.addNodeNetworkFault(
		t,
		"nt05-replication",
		target.NodeName,
		"FORWARD",
		targetNode.Spec.PodCIDR,
		"0.0.0.0/0",
		6379,
	)
	replicationFault.assertPresent(t)
	h.disconnectReplicaClients(t, instance, primary)
	h.waitForConnectedReplicaCount(t, instance, primary, 1)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		observed, found := nodeStatusAtOrdinal(current.Status.Nodes, target.Ordinal)
		return found && sameNodeProcess(observed, target) && observed.Role == valkeyv1alpha1.NodeRoleReplica &&
			observed.Replication != nil && !observed.Replication.LinkUp &&
			current.Status.PrimaryPodUID == primary.PodUID && current.Status.Failover == nil &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseDegraded
	}, "NT-05 degraded после разрыва репликации")
	if role := h.directValkeyRole(t, instance, processAtOrdinal(t, status, target.Ordinal)); role != "slave" {
		t.Fatalf("NT-05 изолированная реплика сменила роль: %s", role)
	}
	setValue(t, connection, "nt05-after-partition", "preserved")
	h.waitForReplicaValue(t, instance, survivor, "nt05-after-partition", "preserved")
	t.Logf("NT-05 переход в degraded: %s", time.Since(phaseStartedAt))

	phaseStartedAt = time.Now()
	h.deletePod(t, instance, target.Ordinal)
	replacementPod := h.waitForReplacementOrdinal(t, instance, target.Ordinal, types.UID(target.PodUID))
	if replacementPod.Spec.NodeName != target.NodeName {
		t.Fatalf(
			"NT-05 пустая замена запущена на Node %s вместо изолированной %s",
			replacementPod.Spec.NodeName,
			target.NodeName,
		)
	}
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		replacement, found := nodeStatusAtOrdinal(current.Status.Nodes, target.Ordinal)
		return found && replacement.PodUID == string(replacementPod.UID) &&
			replacement.Role == valkeyv1alpha1.NodeRoleReplica && !replacement.AppEnabled &&
			replacement.Replication != nil && !replacement.Replication.LinkUp &&
			replacement.Replication.SyncedAt == nil && current.Status.PrimaryPodUID == primary.PodUID &&
			current.Status.Failover == nil && current.Status.Phase == valkeyv1alpha1.InstancePhaseDegraded
	}, "NT-05 пустой неподтверждённой замены")
	replacement := processAtOrdinal(t, status, target.Ordinal)
	if role := h.directValkeyRole(t, instance, replacement); role != "slave" {
		t.Fatalf("NT-05 пустая замена получила роль %s", role)
	}
	operatorPassword := h.servicePasswords(t, instance)[0]
	output, err := execInPod(t, h.adminREST, replacementPod.Namespace, replacementPod.Name, []string{
		"valkey-cli", "--user", "operator", "--pass", operatorPassword,
		"--no-auth-warning", "--raw", "EXISTS", "nt05-after-partition",
	})
	if err != nil || strings.TrimSpace(output) != "0" {
		t.Fatalf("NT-05 пустая замена неожиданно получила контрольный ключ: output=%q error=%v", output, err)
	}
	t.Logf("NT-05 пустая замена без истории: %s", time.Since(phaseStartedAt))

	phaseStartedAt = time.Now()
	h.deletePod(t, instance, primary.Ordinal)
	closeConnections([]*persistentConnection{connection})
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		empty, found := nodeStatusAtOrdinal(current.Status.Nodes, target.Ordinal)
		newPrimary, primaryFound := nodeStatusAtOrdinal(current.Status.Nodes, survivor.Ordinal)
		return found && empty.PodUID == replacement.PodUID && empty.Replication != nil &&
			empty.Replication.SyncedAt == nil && primaryFound &&
			newPrimary.Role == valkeyv1alpha1.NodeRolePrimary && newPrimary.AppEnabled &&
			current.Status.PrimaryPodUID == survivor.PodUID
	}, "NT-05 выбора синхронизированной реплики вместо пустой")
	if actual := primaryProcess(t, status); !sameNodeProcess(actual, survivor) {
		t.Fatalf("NT-05 выбрала primary без подтверждённой истории: %+v", actual)
	}
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "nt05-after-partition"); value != "preserved" {
		t.Fatalf("NT-05 потеряла ключ после выбора пригодной реплики: %q", value)
	}
	t.Logf("NT-05 переключение без пустой копии: %s", time.Since(phaseStartedAt))

	phaseStartedAt = time.Now()
	if err := replicationFault.restore(); err != nil {
		t.Fatalf("NT-05 восстановить репликацию: %v", err)
	}
	replicationFault.assertAbsent(t)
	status = h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	recovered := processAtOrdinal(t, status, target.Ordinal)
	if recovered.PodUID != replacement.PodUID || recovered.Replication == nil ||
		!recovered.Replication.LinkUp || recovered.Replication.SyncedAt == nil {
		t.Fatalf("NT-05 не восстановила ту же пустую замену: %+v", recovered)
	}
	h.waitForReplicaValue(t, instance, recovered, "nt05-after-partition", "preserved")
	if value := getValue(t, connection, "nt05-after-partition"); value != "preserved" {
		t.Fatalf("NT-05 потеряла ключ после восстановления репликации: %q", value)
	}
	t.Logf("NT-05 восстановление репликации: %s", time.Since(phaseStartedAt))

	closeConnections([]*persistentConnection{connection})
	h.deleteInstance(t, instance)
	t.Logf("NT-05 всего: %s", time.Since(testStartedAt))
}

func Test_FP06FP07OP08_RecoverHA_WhenProcessesReceiveSIGSTOPOrSIGCONT_PreservesDataAndRoles(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	ha := h.createHA(t, "sigstop")
	status := h.waitRunning(t, ha)
	for index, initial := range status.Status.Nodes {
		process := processAtOrdinal(t, h.getInstance(t, ha), initial.Ordinal)
		h.signalValkeyProcess(t, ha, process, "STOP")
		h.waitForValkeyProcessState(t, ha, process, "T", "SIGSTOP процесса HA")
		h.signalValkeyProcess(t, ha, process, "CONT")
		h.waitForValkeyProcessState(t, ha, process, "S", "SIGCONT процесса HA")
		status = h.waitForSameProcessHealthy(t, ha, process, "FP-07 восстановления после SIGCONT HA")
		assertHAComposition(t, h, ha, status)
		if index == 0 {
			h.stopOperator(t)
			h.startOperator(t)
			status = h.waitForSameProcessHealthy(t, ha, process, "OP-08 состояния после рестарта оператора")
		}
	}

	connection := openPersistentConnection(t, h.publicAddr, ha, h.caFile, false, func() {})
	setValue(t, connection, "sigstop-key", "preserved")
	primary := primaryProcess(t, status)
	startedAt := time.Now()
	h.signalValkeyProcess(t, ha, primary, "STOP")
	h.waitForTransportObservation(t, ha, primary)
	t.Logf("FP-06 primary: первая транспортная ошибка через %s", time.Since(startedAt))
	h.stopOperator(t)
	h.startOperator(t)
	h.waitForAcceptedProcessDeletion(t, ha, primary)
	t.Logf("FP-06 primary: DELETE принят через %s", time.Since(startedAt))
	h.signalValkeyProcess(t, ha, primary, "CONT")
	h.waitForReplacementOrdinal(t, ha, primary.Ordinal, types.UID(primary.PodUID))
	t.Logf("FP-06 primary: Pod заменён через %s", time.Since(startedAt))
	status = h.waitFor(t, ha, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Failover == nil && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning &&
			current.Status.PrimaryPodUID != "" && current.Status.PrimaryPodUID != primary.PodUID
	}, "FP-06 восстановления primary после SIGSTOP")
	t.Logf("FP-06 SIGSTOP primary: %s", time.Since(startedAt))
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	connection = openPersistentConnection(t, h.publicAddr, ha, h.caFile, false, func() {})
	if value := getValue(t, connection, "sigstop-key"); value != "preserved" {
		t.Fatalf("FP-06 потерял синхронизированный ключ: %q", value)
	}

	primaryUID := status.Status.PrimaryPodUID
	replicaOrdinals := make([]int32, 0, 2)
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica {
			replicaOrdinals = append(replicaOrdinals, process.Ordinal)
		}
	}
	if len(replicaOrdinals) != 2 {
		t.Fatalf("FP-06 ожидал две реплики: %+v", status.Status.Nodes)
	}
	for _, ordinal := range replicaOrdinals {
		replica := processAtOrdinal(t, h.getInstance(t, ha), ordinal)
		startedAt = time.Now()
		h.signalValkeyProcess(t, ha, replica, "STOP")
		h.waitForTransportObservation(t, ha, replica)
		t.Logf("FP-06 replica ordinal %d: первая транспортная ошибка через %s", ordinal, time.Since(startedAt))
		h.waitForAcceptedProcessDeletion(t, ha, replica)
		t.Logf("FP-06 replica ordinal %d: DELETE принят через %s", ordinal, time.Since(startedAt))
		h.waitForReplacementOrdinal(t, ha, ordinal, types.UID(replica.PodUID))
		t.Logf("FP-06 replica ordinal %d: Pod заменён через %s", ordinal, time.Since(startedAt))
		status = h.waitRunning(t, ha)
		t.Logf("FP-06 SIGSTOP replica ordinal %d: %s", ordinal, time.Since(startedAt))
		if status.Status.PrimaryPodUID != primaryUID {
			t.Fatalf("FP-06 восстановление реплики сменило primary: %s -> %s", primaryUID, status.Status.PrimaryPodUID)
		}
		assertHAComposition(t, h, ha, status)
	}
	if value := getValue(t, connection, "sigstop-key"); value != "preserved" {
		t.Fatalf("FP-06 восстановление реплик изменило ключ: %q", value)
	}
	closeConnections([]*persistentConnection{connection})
	h.deleteInstance(t, ha)
}

func Test_FP06FP07OP08_RecoverSingle_WhenProcessReceivesSIGSTOPOrSIGCONT_ReusesOrReplacesProcessSafely(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	single := h.createSingle(t, "stopone", valkeyv1alpha1.WhitelistSpec{})
	singleStatus := h.waitRunning(t, single)
	singleProcess := singleStatus.Status.Nodes[0]
	h.signalValkeyProcess(t, single, singleProcess, "STOP")
	h.waitForValkeyProcessState(t, single, singleProcess, "T", "SIGSTOP процесса single")
	h.signalValkeyProcess(t, single, singleProcess, "CONT")
	h.waitForValkeyProcessState(t, single, singleProcess, "S", "SIGCONT процесса single")
	singleStatus = h.waitForSameProcessHealthy(t, single, singleProcess, "FP-07 восстановления single после SIGCONT")
	singleProcess = singleStatus.Status.Nodes[0]
	singleConnection := openPersistentConnection(t, h.publicAddr, single, h.caFile, false, func() {})
	setValue(t, singleConnection, "sigstop-single-key", "discarded")
	startedAt := time.Now()
	h.signalValkeyProcess(t, single, singleProcess, "STOP")
	h.waitForTransportObservation(t, single, singleProcess)
	t.Logf("FP-06 single: первая транспортная ошибка через %s", time.Since(startedAt))
	h.waitForAcceptedProcessDeletion(t, single, singleProcess)
	t.Logf("FP-06 single: DELETE принят через %s", time.Since(startedAt))
	h.waitForReplacementOrdinal(t, single, singleProcess.Ordinal, types.UID(singleProcess.PodUID))
	t.Logf("FP-06 single: Pod заменён через %s", time.Since(startedAt))
	h.waitRunning(t, single)
	t.Logf("FP-06 SIGSTOP single: %s", time.Since(startedAt))
	assertConnectionClosed(t, singleConnection, false)
	closeConnections([]*persistentConnection{singleConnection})
	singleConnection = openPersistentConnection(t, h.publicAddr, single, h.caFile, false, func() {})
	writeRESP(t, singleConnection, "GET", "sigstop-single-key")
	if value := readRESP(t, singleConnection); value != nil {
		closeConnections([]*persistentConnection{singleConnection})
		t.Fatalf("FP-06 восстановил прежний кэш single: %#v", value)
	}
	closeConnections([]*persistentConnection{singleConnection})

	h.deleteInstance(t, single)
}

func Test_FP08FP09OP08_RecoverBusySingle_WhenCommandsAreKillableOrUnkillable_KillsCommandOrReplacesProcess(
	t *testing.T,
) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createSingle(t, "busy", valkeyv1alpha1.WhitelistSpec{})
	status := h.waitRunning(t, instance)
	process := status.Status.Nodes[0]
	operatorPassword := h.servicePasswords(t, instance)[0]

	short := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	startedAt := time.Now()
	h.startBusyCommand(
		t,
		short,
		"EVAL",
		"local value = 0; for i = 1, 38000000 do value = redis.call('EXISTS', 'missing') end; return value",
		"0",
	)
	h.waitForBusyObservation(t, instance, process)
	if value, err := readRESPResult(t, short); err != nil || value != int64(0) {
		t.Fatalf("FP-08 короткий Lua завершился неверно: value=%#v error=%v", value, err)
	}
	closeConnections([]*persistentConnection{short})
	status = h.waitForSameProcessHealthy(t, instance, process, "FP-08 завершения короткого Lua")
	t.Logf("FP-08 короткий Lua BUSY: %s", time.Since(startedAt))
	assertSameProcess(t, process, status.Status.Nodes[0], "короткий Lua")

	busyLua := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	authDuringBusy := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	startedAt = time.Now()
	h.startBusyCommand(t, busyLua, "EVAL", "while true do redis.call('GET', 'busy-read') end", "0")
	busyStatus := h.waitForBusyObservation(t, instance, process)
	busySince := busyStatus.Status.Nodes[0].Observation.BusySince.DeepCopy()
	writeRESP(t, authDuringBusy, "AUTH", "operator", operatorPassword)
	if response := readRESP(t, authDuringBusy); response != "OK" {
		t.Fatalf("FP-09 AUTH operator во время BUSY вернул %#v", response)
	}
	closeConnections([]*persistentConnection{authDuringBusy})
	h.stopOperator(t)
	h.startOperator(t)
	h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		observed, found := nodeStatusAtOrdinal(current.Status.Nodes, process.Ordinal)
		return found && sameNodeProcess(process, observed) && observed.Observation != nil &&
			observed.Observation.Kind == valkeyv1alpha1.ProcessObservationBusy &&
			observed.Observation.BusySince != nil && observed.Observation.BusySince.Equal(busySince)
	}, "OP-08 сохранения busySince после рестарта")
	status = h.waitForSameProcessHealthy(t, instance, process, "FP-08 остановки read-only Lua")
	assertBusyCommandKilled(t, busyLua, "read-only Lua")
	closeConnections([]*persistentConnection{busyLua})
	t.Logf("FP-08 read-only Lua KILL: %s", time.Since(startedAt))
	assertSameProcess(t, process, status.Status.Nodes[0], "read-only Lua")

	h.loadBusyFunctions(t, instance, operatorPassword)
	process = status.Status.Nodes[0]
	busyFunction := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	startedAt = time.Now()
	h.startBusyCommand(t, busyFunction, "FCALL", "busy_read", "0")
	h.waitForBusyObservation(t, instance, process)
	status = h.waitForSameProcessHealthy(t, instance, process, "FP-08 остановки read-only Function")
	assertBusyCommandKilled(t, busyFunction, "read-only Function")
	closeConnections([]*persistentConnection{busyFunction})
	t.Logf("FP-08 read-only Function KILL: %s", time.Since(startedAt))
	assertSameProcess(t, process, status.Status.Nodes[0], "read-only Function")

	process = status.Status.Nodes[0]
	busyLua = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	startedAt = time.Now()
	h.startBusyCommand(
		t,
		busyLua,
		"EVAL",
		"redis.call('SET', 'busy-written-lua', 'value'); while true do redis.call('GET', 'busy-written-lua') end",
		"0",
	)
	h.waitForBusyObservation(t, instance, process)
	h.waitForUnkillableBusyDeletion(t, instance, process)
	h.waitForAcceptedProcessDeletion(t, instance, process)
	h.waitForReplacementOrdinal(t, instance, process.Ordinal, types.UID(process.PodUID))
	status = h.waitRunning(t, instance)
	assertConnectionClosed(t, busyLua, true)
	closeConnections([]*persistentConnection{busyLua})
	t.Logf("FP-09 пишущий Lua и замена: %s", time.Since(startedAt))
	h.assertMissingKey(t, instance, "busy-written-lua")

	h.loadBusyFunctions(t, instance, operatorPassword)
	process = status.Status.Nodes[0]
	busyFunction = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	startedAt = time.Now()
	h.startBusyCommand(t, busyFunction, "FCALL", "busy_write", "0")
	h.waitForBusyObservation(t, instance, process)
	h.waitForUnkillableBusyDeletion(t, instance, process)
	h.waitForAcceptedProcessDeletion(t, instance, process)
	h.waitForReplacementOrdinal(t, instance, process.Ordinal, types.UID(process.PodUID))
	h.waitRunning(t, instance)
	assertConnectionClosed(t, busyFunction, true)
	closeConnections([]*persistentConnection{busyFunction})
	t.Logf("FP-09 пишущая Function и замена: %s", time.Since(startedAt))
	h.assertMissingKey(t, instance, "busy-written-function")

	h.deleteInstance(t, instance)
}

func Test_FP10_RecoverBothLostReplicas_WhenReplacementsAreSynchronizing_RemainsDegradedUntilSafe(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "tworeplicas")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	primary := primaryProcess(t, status)
	replicas := make([]valkeyv1alpha1.NodeStatus, 0, 2)
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica {
			replicas = append(replicas, process)
		}
	}
	if len(replicas) != 2 {
		t.Fatalf("FP-10 ожидала две реплики: %+v", status.Status.Nodes)
	}
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "fp10-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "fp10-key", "preserved")

	nodes := &corev1.NodeList{}
	if err := h.k8s.List(t.Context(), nodes); err != nil {
		t.Fatalf("FP-10 прочитать ноды стенда: %v", err)
	}
	cordonedNodes := make([]string, 0, len(nodes.Items)-1)
	for _, node := range nodes.Items {
		if node.Name == primary.NodeName {
			continue
		}
		h.setNodeUnschedulable(t, node.Name, true)
		cordonedNodes = append(cordonedNodes, node.Name)
	}
	t.Cleanup(func() {
		for _, nodeName := range cordonedNodes {
			h.setNodeUnschedulable(t, nodeName, false)
		}
	})

	h.stopOperator(t)
	for _, replica := range replicas {
		h.sigkillValkeyProcess(t, instance, replica)
	}
	for _, replica := range replicas {
		h.waitValkeyTerminatedWithoutReplacement(t, instance, replica)
	}
	h.startOperator(t)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		if current.Status.Failover != nil || current.Status.Phase != valkeyv1alpha1.InstancePhaseDegraded ||
			current.Status.PrimaryPodUID != primary.PodUID {
			return false
		}
		for _, replica := range replicas {
			observed, found := nodeStatusAtOrdinal(current.Status.Nodes, replica.Ordinal)
			if !found || observed.PodUID != replica.PodUID || observed.Termination == nil {
				return false
			}
		}
		return true
	}, "FP-10 degraded без обеих реплик")
	if value := getValue(t, connection, "fp10-key"); value != "preserved" {
		t.Fatalf("FP-10 primary потерял контрольный ключ: %q", value)
	}
	setValue(t, connection, "fp10-during-recovery", "available")

	followPoint := newValkeyCommandPoint(
		t,
		"",
		"REPLICAOF",
		operatorvalkey.IntegrationCommandBefore,
		false,
	)
	for _, nodeName := range cordonedNodes {
		h.setNodeUnschedulable(t, nodeName, false)
	}
	followPoint.waitFor(t, time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Failover != nil || status.Status.PrimaryPodUID != primary.PodUID ||
		status.Status.Phase != valkeyv1alpha1.InstancePhaseDegraded {
		t.Fatalf("FP-10 изменила primary до синхронизации замены: %+v", status.Status)
	}
	emptyReplacementFound := false
	for _, process := range status.Status.Nodes {
		if process.Ordinal == primary.Ordinal {
			continue
		}
		previous := replicas[0]
		if replicas[1].Ordinal == process.Ordinal {
			previous = replicas[1]
		}
		if process.PodUID != previous.PodUID && process.Role == valkeyv1alpha1.NodeRoleReplica &&
			process.Replication != nil && process.Replication.SyncedAt == nil {
			emptyReplacementFound = true
		}
	}
	if !emptyReplacementFound {
		t.Fatalf("FP-10 не наблюдала пустую неподтверждённую замену: %+v", status.Status.Nodes)
	}
	followPoint.close()

	status = h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	for _, replica := range replicas {
		replacement := processAtOrdinal(t, status, replica.Ordinal)
		if replacement.PodUID == replica.PodUID || replacement.Replication == nil ||
			replacement.Replication.SyncedAt == nil {
			t.Fatalf("FP-10 не синхронизировала замену ordinal %d: %+v", replica.Ordinal, replacement)
		}
	}
	if value := getValue(t, connection, "fp10-during-recovery"); value != "available" {
		t.Fatalf("FP-10 потеряла запись во время degraded: %q", value)
	}
	closeConnections([]*persistentConnection{connection})
	h.deleteInstance(t, instance)
}

func Test_FP05FP11_RecoverHA_WhenAllProcessesStopWhileOperatorIsDown_RebuildsClusterWithEmptyCache(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "allstop")
	status := h.waitRunning(t, instance)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "lost-key", "discarded")
	before := processIdentities(status.Status.Nodes)

	h.stopOperator(t)
	for _, process := range status.Status.Nodes {
		h.sigkillValkeyProcess(t, instance, process)
	}
	closeConnections([]*persistentConnection{connection})
	for _, process := range status.Status.Nodes {
		h.waitValkeyTerminatedWithoutReplacement(t, instance, process)
	}
	h.startOperator(t)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Initialized && current.Status.Failover == nil &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning && len(current.Status.Nodes) == 3 &&
			dataLossCondition(current.Status.Conditions)
	}, "FP-11 пустого восстановления всего HA")
	assertHAComposition(t, h, instance, status)
	assertAllProcessIdentitiesChanged(t, before, status.Status.Nodes)
	h.assertInstanceEvent(t, instance, "CacheEmptied")
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	writeRESP(t, connection, "GET", "lost-key")
	if value := readRESP(t, connection); value != nil {
		t.Fatalf("FP-11 восстановил потерянный кэш: %#v", value)
	}
	setValue(t, connection, "after-recovery", "available")
	h.deleteInstance(t, instance)
}

func Test_OP01_RecoverClusterOperator_AfterSIGKILL_ResumesWithNewLeaseHolderWithoutDataLoss(t *testing.T) {
	h := newHarness(t)
	h.startClusterOperator(t)
	t.Cleanup(func() {
		h.stopClusterOperator(t)
		h.close(t)
	})

	instance := h.createHA(t, "opkill")
	status := h.waitRunning(t, instance)
	beforeObservedAt := status.Status.ObservedAt.DeepCopy()
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	setValue(t, connection, "operator-key", "survives")

	pod := h.clusterOperatorPod(t)
	container := podContainer(t, pod, "operator")
	beforeHolder := h.operatorLease(t).Spec.HolderIdentity
	h.sigkillContainer(t, pod.Spec.NodeName, container.ContainerID)

	h.waitForClusterOperatorRestart(t, pod.UID, container.ContainerID, container.RestartCount)
	h.waitForLeaseHolderChange(t, beforeHolder)
	h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.ObservedAt != nil && beforeObservedAt != nil &&
			current.Status.ObservedAt.After(beforeObservedAt.Time) &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "наблюдения HA новым процессом оператора")
	if value := getValue(t, connection, "operator-key"); value != "survives" {
		t.Fatalf("рестарт оператора изменил данные: %q", value)
	}

	h.deleteInstance(t, instance)
}

func Test_CT06_ResumeFailover_AfterManualFencingConfirmsStoppedAgent_PreservesData(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })
	h.requireNodeCount(t, 4)

	instance := h.createHA(t, "manualfence")
	status := h.waitRunning(t, instance)
	primary := primaryProcess(t, status)
	if primary.NodeName == "k3s-server" {
		h.deletePod(t, instance, primary.Ordinal)
		status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
			return current.Status.Failover == nil && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning &&
				current.Status.PrimaryPodUID != "" && current.Status.PrimaryPodUID != primary.PodUID
		}, "CT-06 primary на agent")
		primary = primaryProcess(t, status)
	}
	if primary.NodeName == "k3s-server" {
		t.Fatal("CT-06 primary остался на server")
	}
	h.useSurvivingEnvoy(t, primary.NodeName)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "manual-fencing-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "manual-fencing-key", "preserved")

	node := &corev1.Node{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{Name: primary.NodeName}, node); err != nil {
		t.Fatalf("CT-06 прочитать Node primary: %v", err)
	}
	fault := h.newAgentFault(t, node)
	t.Cleanup(func() { fault.restore(t, h) })
	fault.kill(t, h)
	closeConnections([]*persistentConnection{connection})

	identity := processIdentityForTest(primary)
	h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Failover != nil &&
			current.Status.Failover.Stage == valkeyv1alpha1.FailoverStageFencing &&
			current.Status.Failover.Source == identity
	}, "CT-06 ожидания ручного fencing primary")
	h.setManualFencing(t, instance, identity)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return hasTerminationEvidence(current.Status.Nodes, identity, "manual_fencing") ||
			hasTerminationEvidence(current.Status.PreviousProcesses, identity, "manual_fencing")
	}, "CT-06 принятия точного ручного fencing")
	stoppedNode := &corev1.Node{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{Name: node.Name}, stoppedNode); err != nil {
		t.Fatalf("CT-06 прочитать остановленную Node: %v", err)
	}
	if stoppedNode.UID != node.UID || nodeReady(stoppedNode.Status.Conditions) {
		t.Fatalf("CT-06 приняла fencing до остановки прежней Node: %+v", stoppedNode.Status.Conditions)
	}

	status = h.waitForWithin(t, instance, 3*time.Minute, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		if current.Status.Failover != nil || current.Status.Phase != valkeyv1alpha1.InstancePhaseRunning ||
			current.Status.PrimaryPodUID == primary.PodUID || len(current.Status.Nodes) != 3 {
			return false
		}
		for _, process := range current.Status.Nodes {
			if process.NodeUID == string(node.UID) || process.Termination != nil || !process.Readiness {
				return false
			}
		}
		return true
	}, "CT-06 восстановления после ручного fencing")
	assertHAComposition(t, h, instance, status)
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "manual-fencing-key"); value != "preserved" {
		t.Fatalf("CT-06 потеряла контрольный ключ: %q", value)
	}
	closeConnections([]*persistentConnection{connection})
	current := h.getInstance(t, instance)
	if _, found := current.Annotations["valkey.h3llo-demo.com/manual-fencing"]; found {
		t.Fatal("CT-06 не удалила принятую аннотацию")
	}

	fault.restore(t, h)
	h.deleteInstance(t, instance)
}

func Test_ND04_RecoverHA_WhenOperatorAndPrimaryWorkerAreDestroyed_ReplacesOperatorAndFailsOver(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() {
		h.stopClusterOperator(t)
		h.close(t)
	})
	h.requireNodeCount(t, 4)

	instance := h.createHA(t, "ndopprimary")
	status := h.waitRunning(t, instance)
	primary := primaryProcess(t, status)
	if primary.NodeName == "k3s-server" {
		h.deletePod(t, instance, primary.Ordinal)
		status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
			return current.Status.Failover == nil && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning &&
				current.Status.PrimaryPodUID != "" && current.Status.PrimaryPodUID != primary.PodUID
		}, "ND-04 primary на worker")
		primary = primaryProcess(t, status)
	}
	if primary.NodeName == "k3s-server" {
		t.Fatal("ND-04 primary остался на server")
	}
	h.useSurvivingEnvoy(t, primary.NodeName)
	h.stopOperator(t)
	h.startClusterOperatorOnNode(t, primary.NodeName)

	operatorPod := h.clusterOperatorPod(t)
	if operatorPod.Spec.NodeName != primary.NodeName {
		t.Fatalf("оператор и primary размещены на разных нодах: %s != %s", operatorPod.Spec.NodeName, primary.NodeName)
	}
	operatorContainer := podContainer(t, operatorPod, "operator")
	beforeHolder := h.operatorLease(t).Spec.HolderIdentity
	beforeObservedAt := status.Status.ObservedAt.DeepCopy()
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	setValue(t, connection, "nd04-key", "preserved")

	node := &corev1.Node{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{Name: primary.NodeName}, node); err != nil {
		t.Fatalf("прочитать общую Node оператора и primary: %v", err)
	}
	fault := h.newAgentFault(t, node)
	t.Cleanup(func() { fault.restore(t, h) })
	fault.kill(t, h)
	h.deleteNode(t, node)

	replacementOperator := h.waitForClusterOperatorRestart(
		t,
		operatorPod.UID,
		operatorContainer.ContainerID,
		operatorContainer.RestartCount,
	)
	if replacementOperator.Spec.NodeName == primary.NodeName {
		t.Fatalf("замена оператора осталась на удалённой Node %s", primary.NodeName)
	}
	h.waitForLeaseHolderChange(t, beforeHolder)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		if current.Status.Failover != nil || current.Status.Phase != valkeyv1alpha1.InstancePhaseRunning ||
			current.Status.PrimaryPodUID == primary.PodUID || current.Status.ObservedAt == nil ||
			beforeObservedAt == nil || !current.Status.ObservedAt.After(beforeObservedAt.Time) {
			return false
		}
		for _, process := range current.Status.Nodes {
			if process.NodeUID == string(node.UID) || process.Termination != nil || !process.Readiness {
				return false
			}
		}
		return len(current.Status.Nodes) == 3
	}, "ND-04 восстановления оператором после общей потери ноды")
	assertHAComposition(t, h, instance, status)
	closeConnections([]*persistentConnection{connection})
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "nd04-key"); value != "preserved" {
		t.Fatalf("ND-04 потерял синхронизированный ключ: %q", value)
	}

	fault.restore(t, h)
	h.deleteInstance(t, instance)
}

func Test_ND05_RecoverHA_WhenPrimaryWorkerIsDestroyedWithoutSpare_WaitsForCleanAgentAndRestoresComposition(
	t *testing.T,
) {
	testStartedAt := time.Now()
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	nodes := &corev1.NodeList{}
	if err := h.k8s.List(t.Context(), nodes); err != nil {
		t.Fatalf("ND-05 прочитать Node: %v", err)
	}
	if len(nodes.Items) < 3 {
		t.Fatalf("ND-05 требует минимум три Node, получено %d", len(nodes.Items))
	}
	extraNodes := make([]string, 0)
	for index := range nodes.Items {
		node := &nodes.Items[index]
		if node.Name == "k3s-server" || node.Name == "k3s-agent-1" || node.Name == "k3s-agent-2" ||
			node.Spec.Unschedulable {
			continue
		}
		h.setNodeUnschedulable(t, node.Name, true)
		extraNodes = append(extraNodes, node.Name)
	}
	t.Cleanup(func() {
		for _, nodeName := range extraNodes {
			h.setNodeUnschedulable(t, nodeName, false)
		}
	})

	instance := h.createHA(t, "ndnospare")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	primary := primaryProcess(t, status)
	if primary.NodeName == "k3s-server" {
		h.deletePod(t, instance, primary.Ordinal)
		status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
			return current.Status.Failover == nil && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning &&
				current.Status.PrimaryPodUID != "" && current.Status.PrimaryPodUID != primary.PodUID
		}, "ND-05 primary на agent")
		primary = primaryProcess(t, status)
	}
	if primary.NodeName == "k3s-server" {
		t.Fatal("ND-05 primary остался на server")
	}
	h.useSurvivingEnvoy(t, primary.NodeName)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer func() { closeConnections([]*persistentConnection{connection}) }()
	setValue(t, connection, "nd05-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "nd05-key", "preserved")

	failedNode := h.getNode(t, primary.NodeName)
	fault := h.newAgentFault(t, failedNode)
	t.Cleanup(func() { fault.restore(t, h) })
	phaseStartedAt := time.Now()
	fault.kill(t, h)
	closeConnections([]*persistentConnection{connection})
	h.deleteNode(t, failedNode)

	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		if current.Status.Failover != nil || current.Status.Phase != valkeyv1alpha1.InstancePhaseDegraded ||
			current.Status.PrimaryPodUID == primary.PodUID {
			return false
		}
		primaries := 0
		replicas := 0
		for _, process := range current.Status.Nodes {
			if process.NodeUID == string(failedNode.UID) {
				if process.Termination == nil || process.Termination.Evidence != "node_deleted" {
					return false
				}
				continue
			}
			if process.Termination != nil || !process.Readiness {
				return false
			}
			switch process.Role {
			case valkeyv1alpha1.NodeRolePrimary:
				primaries++
			case valkeyv1alpha1.NodeRoleReplica:
				replicas++
				if process.Replication == nil || !process.Replication.LinkUp ||
					process.Replication.SyncedAt == nil {
					return false
				}
			}
		}
		return primaries == 1 && replicas == 1
	}, "ND-05 degraded без запасного хоста")
	h.waitForPendingPod(t, instance)
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "nd05-key"); value != "preserved" {
		t.Fatalf("ND-05 потеряла ключ в degraded: %q", value)
	}
	setValue(t, connection, "nd05-degraded", "available")
	t.Logf("ND-05 восстановление записи без запасной ноды: %s", time.Since(phaseStartedAt))

	phaseStartedAt = time.Now()
	fault.restore(t, h)
	restoredNode := h.getNode(t, failedNode.Name)
	if restoredNode.UID == failedNode.UID {
		t.Fatalf("ND-05 восстановила прежний UID Node %s", failedNode.UID)
	}
	status = h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	foundRestoredNode := false
	for _, process := range status.Status.Nodes {
		if process.NodeUID == string(failedNode.UID) {
			t.Fatalf("ND-05 сохранила процесс прежней Node: %+v", process)
		}
		if process.NodeUID == string(restoredNode.UID) {
			foundRestoredNode = true
		}
	}
	if !foundRestoredNode {
		t.Fatalf("ND-05 не разместила третью копию на чистом agent %s", restoredNode.Name)
	}
	closeConnections([]*persistentConnection{connection})
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "nd05-degraded"); value != "available" {
		t.Fatalf("ND-05 потеряла запись после возврата agent: %q", value)
	}
	t.Logf("ND-05 полный состав после чистого agent: %s", time.Since(phaseStartedAt))

	closeConnections([]*persistentConnection{connection})
	h.deleteInstance(t, instance)
	t.Logf("ND-05 всего: %s", time.Since(testStartedAt))
}

func Test_ND01_RecoverHA_WhenPrimaryWorkerIsDestroyed_FailsOverAndRestoresOnSpareNode(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })
	h.requireNodeCount(t, 4)

	instance := h.createHA(t, "ndprimary")
	status := h.waitRunning(t, instance)
	primary := primaryProcess(t, status)
	if primary.NodeName == "k3s-server" {
		h.deletePod(t, instance, primary.Ordinal)
		status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
			return current.Status.Failover == nil && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning &&
				current.Status.PrimaryPodUID != "" && current.Status.PrimaryPodUID != primary.PodUID
		}, "primary на worker после удаления Pod с server")
		primary = primaryProcess(t, status)
	}
	if primary.NodeName == "k3s-server" {
		t.Fatal("primary остался на server перед отказом worker")
	}
	h.useSurvivingEnvoy(t, primary.NodeName)

	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	setValue(t, connection, "node-key", "replicated")

	node := &corev1.Node{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{Name: primary.NodeName}, node); err != nil {
		t.Fatalf("прочитать целевой Node: %v", err)
	}
	fault := h.newAgentFault(t, node)
	t.Cleanup(func() { fault.restore(t, h) })
	fault.kill(t, h)

	h.deleteNode(t, node)

	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		if current.Status.Failover != nil || current.Status.Phase != valkeyv1alpha1.InstancePhaseRunning ||
			current.Status.PrimaryPodUID == primary.PodUID || len(current.Status.Nodes) != 3 {
			return false
		}
		for _, process := range current.Status.Nodes {
			if process.NodeUID == string(node.UID) || process.Termination != nil || !process.Readiness {
				return false
			}
		}
		return true
	}, "полного HA после уничтожения worker")
	assertHAComposition(t, h, instance, status)
	closeConnections([]*persistentConnection{connection})
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "node-key"); value != "replicated" {
		t.Fatalf("отказ worker потерял синхронизированный ключ: %q", value)
	}

	fault.restore(t, h)
	h.deleteInstance(t, instance)
}

func Test_ND02_RecoverHA_WhenReplicaWorkerIsDestroyed_ReplacesReplicaOnSpareNode(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })
	h.requireNodeCount(t, 4)

	instance := h.createHA(t, "ndreplica")
	status := h.waitRunning(t, instance)
	var replica valkeyv1alpha1.NodeStatus
	found := false
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica && process.NodeName != "k3s-server" {
			replica = process
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ND-02 не нашёл реплику на worker: %+v", status.Status.Nodes)
	}
	h.useSurvivingEnvoy(t, replica.NodeName)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	setValue(t, connection, "replica-node-key", "preserved")

	node := &corev1.Node{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{Name: replica.NodeName}, node); err != nil {
		t.Fatalf("прочитать Node реплики: %v", err)
	}
	fault := h.newAgentFault(t, node)
	t.Cleanup(func() { fault.restore(t, h) })
	fault.kill(t, h)
	h.deleteNode(t, node)

	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		process, exists := nodeStatusAtOrdinal(current.Status.Nodes, replica.Ordinal)
		return exists && process.PodUID != replica.PodUID && process.NodeUID != string(node.UID) &&
			process.Role == valkeyv1alpha1.NodeRoleReplica && process.Readiness &&
			process.Replication != nil && process.Replication.LinkUp && process.Replication.SyncedAt != nil &&
			current.Status.PrimaryPodUID == status.Status.PrimaryPodUID &&
			current.Status.Failover == nil && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "ND-02 замены реплики на запасной ноде")
	assertHAComposition(t, h, instance, status)
	if value := getValue(t, connection, "replica-node-key"); value != "preserved" {
		t.Fatalf("ND-02 изменила данные primary: %q", value)
	}

	fault.restore(t, h)
	h.deleteInstance(t, instance)
}

func Test_ND03_RecoverSingle_WhenWorkerIsDestroyed_StartsEmptyReplacementWithStableCredentials(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })
	h.requireNodeCount(t, 2)
	h.setNodeUnschedulable(t, "k3s-server", true)
	t.Cleanup(func() { h.setNodeUnschedulable(t, "k3s-server", false) })

	instance := h.createSingle(t, "ndsingle", valkeyv1alpha1.WhitelistSpec{})
	status := h.waitRunning(t, instance)
	h.setNodeUnschedulable(t, "k3s-server", false)
	if len(status.Status.Nodes) != 1 || status.Status.Nodes[0].NodeName == "k3s-server" {
		t.Fatalf("ND-03 single не размещён на worker: %+v", status.Status.Nodes)
	}
	process := status.Status.Nodes[0]
	servicePasswords := h.servicePasswords(t, instance)
	h.useSurvivingEnvoy(t, process.NodeName)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "single-node-key", "discarded")

	node := &corev1.Node{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{Name: process.NodeName}, node); err != nil {
		t.Fatalf("прочитать Node single: %v", err)
	}
	fault := h.newAgentFault(t, node)
	t.Cleanup(func() { fault.restore(t, h) })
	fault.kill(t, h)
	h.deleteNode(t, node)

	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Initialized && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning &&
			len(current.Status.Nodes) == 1 && current.Status.Nodes[0].PodUID != process.PodUID &&
			current.Status.Nodes[0].NodeUID != string(node.UID)
	}, "ND-03 замены single на запасной ноде")
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("ND-03 изменила служебные пароли")
	}
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	writeRESP(t, connection, "GET", "single-node-key")
	if value := readRESP(t, connection); value != nil {
		t.Fatalf("ND-03 сохранила кэш single: %#v", value)
	}

	fault.restore(t, h)
	h.deleteInstance(t, instance)
}

func Test_ND06_RecoverHA_WhenWorkerWithEnvoyIsDestroyed_PreservesPublicIngressAndData(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })
	h.requireNodeCount(t, 4)

	beforeEnvoy := h.readyEnvoyPods(t)
	targetNode := ""
	for _, pod := range beforeEnvoy {
		if pod.Spec.NodeName != "k3s-server" {
			targetNode = pod.Spec.NodeName
			break
		}
	}
	if targetNode == "" {
		t.Fatalf("ND-06 не нашла Envoy на worker: %+v", beforeEnvoy)
	}
	cordonedNode := ""
	for _, candidate := range []string{"k3s-agent-3", "k3s-agent-2", "k3s-agent-1"} {
		if candidate != targetNode {
			cordonedNode = candidate
			break
		}
	}
	if cordonedNode == "" {
		t.Fatal("ND-06 не нашла запасную ноду")
	}
	h.setNodeUnschedulable(t, cordonedNode, true)
	t.Cleanup(func() { h.setNodeUnschedulable(t, cordonedNode, false) })

	instance := h.createHA(t, "ndenvoy")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	h.setNodeUnschedulable(t, cordonedNode, false)
	var failedProcess valkeyv1alpha1.NodeStatus
	foundProcess := false
	for _, process := range status.Status.Nodes {
		if process.NodeName == targetNode {
			failedProcess = process
			foundProcess = true
			break
		}
	}
	if !foundProcess {
		t.Fatalf("ND-06 не разместила Valkey вместе с Envoy на %s: %+v", targetNode, status.Status.Nodes)
	}

	originalConnections := h.openHAEnvoyConnections(t, instance)
	pingConnections(t, originalConnections)
	closeConnections(originalConnections)
	h.useSurvivingEnvoy(t, targetNode)
	survivingAddress := h.publicAddr
	assertWrongSNIRejected(t, h, instance)
	connection := openPersistentConnection(t, survivingAddress, instance, h.caFile, false, func() {})
	setValue(t, connection, "nd06-key", "preserved")

	node := &corev1.Node{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{Name: targetNode}, node); err != nil {
		t.Fatalf("прочитать Node с Envoy и Valkey: %v", err)
	}
	fault := h.newAgentFault(t, node)
	t.Cleanup(func() { fault.restore(t, h) })
	fault.kill(t, h)
	h.deleteNode(t, node)

	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		if current.Status.Failover != nil || current.Status.Phase != valkeyv1alpha1.InstancePhaseRunning ||
			len(current.Status.Nodes) != 3 {
			return false
		}
		for _, process := range current.Status.Nodes {
			if process.NodeUID == string(node.UID) || process.PodUID == failedProcess.PodUID ||
				process.Termination != nil || !process.Readiness {
				return false
			}
		}
		return true
	}, "ND-06 восстановления Valkey после общей потери ноды")
	assertHAComposition(t, h, instance, status)
	h.waitForEnvoyReplacement(t, beforeEnvoy, targetNode)
	closeConnections([]*persistentConnection{connection})
	connection = openPersistentConnection(t, survivingAddress, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	if value := getValue(t, connection, "nd06-key"); value != "preserved" {
		t.Fatalf("ND-06 потеряла синхронизированный ключ: %q", value)
	}
	assertWrongSNIRejected(t, h, instance)
	recoveredConnections := h.openHAEnvoyConnections(t, instance)
	pingConnections(t, recoveredConnections)
	closeConnections(recoveredConnections)
	if h.publicAddr != survivingAddress {
		t.Fatalf("ND-06 изменила сохранённый адрес живого Envoy: %s != %s", h.publicAddr, survivingAddress)
	}

	fault.restore(t, h)
	h.deleteInstance(t, instance)
}

func Test_CT14ND07ND08ND09_ResumeProcessRecovery_WhenNodeIsDestroyedDuringStopAndOperatorRestarts_UsesCurrentNodeIdentity(
	t *testing.T,
) {
	testStartedAt := time.Now()
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })
	h.requireNodeCount(t, 4)

	instance := h.createHA(t, "ndrestart")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	var target valkeyv1alpha1.NodeStatus
	targetFound := false
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica && process.NodeName != "k3s-server" {
			target = process
			targetFound = true
			break
		}
	}
	if !targetFound {
		t.Fatalf("ND-08 не нашла реплику на agent: %+v", status.Status.Nodes)
	}
	h.useSurvivingEnvoy(t, target.NodeName)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer func() { closeConnections([]*persistentConnection{connection}) }()
	setValue(t, connection, "ndrestart-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "ndrestart-key", "preserved")

	deletePoint := newProcessActionPoint(t, "request-delete", target.PodUID, "")
	phaseStartedAt := time.Now()
	h.signalValkeyProcess(t, instance, target, "STOP")
	nodeDestroyed := false
	t.Cleanup(func() {
		if !nodeDestroyed {
			h.signalValkeyProcess(t, instance, target, "CONT")
		}
	})
	h.waitForValkeyProcessState(t, instance, target, "T", "ND-08 SIGSTOP реплики")
	deletePoint.waitFor(t, 2*time.Minute)
	status = h.getInstance(t, instance)
	stopping := processAtOrdinal(t, status, target.Ordinal)
	if stopping.Recovery == nil || stopping.Recovery.Stage != valkeyv1alpha1.ProcessRecoveryStageDeleting {
		t.Fatalf("ND-08 не сохранила штатную остановку до DELETE: %+v", stopping.Recovery)
	}
	targetPod := h.getPodOrdinal(t, instance, target.Ordinal)
	if !targetPod.DeletionTimestamp.IsZero() {
		t.Fatalf("ND-08 дошла до DELETE до управляемой границы: %s", targetPod.DeletionTimestamp)
	}
	failedNode := h.getNode(t, target.NodeName)
	fault := h.newAgentFault(t, failedNode)
	t.Cleanup(func() { fault.restore(t, h) })
	fault.kill(t, h)
	nodeDestroyed = true
	closeConnections([]*persistentConnection{connection})
	h.deleteNode(t, failedNode)
	releasePoint := newProcessActionPoint(t, "release-terminated", target.PodUID, "node_deleted")
	deletePoint.close()
	t.Logf("ND-08 уничтожение agent во время принятой остановки: %s", time.Since(phaseStartedAt))

	releasePoint.waitFor(t, 2*time.Minute)
	status = h.getInstance(t, instance)
	terminated := processAtOrdinal(t, status, target.Ordinal)
	if terminated.Termination == nil || terminated.Termination.Evidence != "node_deleted" ||
		terminated.NodeUID != string(failedNode.UID) {
		t.Fatalf("ND-09 не сохранила точное доказательство node_deleted: %+v", terminated)
	}
	heldPod := h.getPodOrdinal(t, instance, target.Ordinal)
	if !slices.Contains(heldPod.Finalizers, "valkey.h3llo-demo.com/process-stopped") {
		t.Fatalf("ND-09 сняла finalizer до управляемого рестарта: %v", heldPod.Finalizers)
	}
	h.stopOperator(t)
	releasePoint.close()
	h.startOperator(t)

	replacementPod := h.waitForReplacementOrdinal(t, instance, target.Ordinal, types.UID(target.PodUID))
	status = h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	replacement := processAtOrdinal(t, status, target.Ordinal)
	if replacement.PodUID != string(replacementPod.UID) || replacement.NodeUID == string(failedNode.UID) ||
		replacement.Termination != nil {
		t.Fatalf("ND-08/ND-09 не продолжила замену после рестарта: %+v", replacement)
	}
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "ndrestart-key"); value != "preserved" {
		t.Fatalf("ND-08/ND-09 потеряла ключ после восстановления: %q", value)
	}
	t.Logf("ND-09 восстановление после рестарта на node_deleted: %s", time.Since(phaseStartedAt))

	phaseStartedAt = time.Now()
	fault.restore(t, h)
	restoredNode := h.getNode(t, failedNode.Name)
	if restoredNode.UID == failedNode.UID {
		t.Fatalf("ND-07 чистая Node сохранила прежний UID %s", failedNode.UID)
	}
	status = h.waitRunning(t, instance)
	var moving valkeyv1alpha1.NodeStatus
	movingFound := false
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica && process.NodeName != restoredNode.Name {
			moving = process
			movingFound = true
			break
		}
	}
	if !movingFound {
		t.Fatalf("ND-07 не нашла реплику для размещения на чистой Node: %+v", status.Status.Nodes)
	}
	h.setNodeUnschedulable(t, moving.NodeName, true)
	t.Cleanup(func() { h.setNodeUnschedulable(t, moving.NodeName, false) })
	h.deletePod(t, instance, moving.Ordinal)
	movedPod := h.waitForReplacementOrdinal(t, instance, moving.Ordinal, types.UID(moving.PodUID))
	if movedPod.Spec.NodeName != restoredNode.Name {
		t.Fatalf("ND-07 замена размещена на %s вместо %s", movedPod.Spec.NodeName, restoredNode.Name)
	}
	status = h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	moved := processAtOrdinal(t, status, moving.Ordinal)
	if moved.PodUID != string(movedPod.UID) || moved.NodeName != restoredNode.Name ||
		moved.NodeUID != string(restoredNode.UID) || moved.Termination != nil || !moved.Readiness ||
		moved.Replication == nil || !moved.Replication.LinkUp || moved.Replication.SyncedAt == nil {
		t.Fatalf("ND-07 применила старое доказательство к новому процессу: %+v", moved)
	}
	if value := getValue(t, connection, "ndrestart-key"); value != "preserved" {
		t.Fatalf("ND-07 потеряла ключ после возврата Node с тем же именем: %q", value)
	}
	h.setNodeUnschedulable(t, moving.NodeName, false)
	t.Logf("ND-07 процесс на Node с новым UID: %s", time.Since(phaseStartedAt))

	closeConnections([]*persistentConnection{connection})
	h.deleteInstance(t, instance)
	t.Logf("ND-07/ND-08/ND-09 всего: %s", time.Since(testStartedAt))
}

func (h *harness) startClusterOperator(t *testing.T) {
	h.startClusterOperatorOnNode(t, "")
}

func (h *harness) removeOperatorEnvoyAdminAccess(t *testing.T) func() error {
	t.Helper()
	key := client.ObjectKey{Name: "managed-valkey-operator-portforward", Namespace: "envoy-gateway-system"}
	binding := &rbacv1.RoleBinding{}
	if err := h.k8s.Get(t.Context(), key, binding); err != nil {
		t.Fatalf("прочитать RoleBinding admin API Envoy: %v", err)
	}
	replacement := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: binding.Name, Namespace: binding.Namespace},
		RoleRef:    binding.RoleRef,
		Subjects:   append([]rbacv1.Subject(nil), binding.Subjects...),
	}
	if err := h.k8s.Delete(t.Context(), binding); err != nil {
		t.Fatalf("удалить RoleBinding admin API Envoy: %v", err)
	}

	restored := false
	return func() error {
		if restored {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := h.k8s.Create(ctx, replacement); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		restored = true
		return nil
	}
}

func (h *harness) directValkeyRole(
	t *testing.T,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
) string {
	t.Helper()
	pod := h.getPodOrdinal(t, instance, process.Ordinal)
	operatorPassword := h.servicePasswords(t, instance)[0]
	output, err := execInPod(t, h.adminREST, pod.Namespace, pod.Name, []string{
		"valkey-cli", "--user", "operator", "--pass", operatorPassword,
		"--no-auth-warning", "--raw", "ROLE",
	})
	if err != nil {
		t.Fatalf("прочитать роль Valkey ordinal %d: output=%q error=%v", process.Ordinal, output, err)
	}
	return strings.Split(strings.TrimSpace(output), "\n")[0]
}

func (h *harness) waitForReplicaValue(
	t *testing.T,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
	key string,
	expected string,
) {
	t.Helper()
	pod := h.getPodOrdinal(t, instance, process.Ordinal)
	err := wait.PollUntilContextTimeout(
		t.Context(),
		50*time.Millisecond,
		5*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			output, err := execInPodContext(ctx, h.adminREST, pod.Namespace, pod.Name, []string{
				"valkey-cli", "--user", "app", "--pass", instance.password,
				"--no-auth-warning", "--raw", "GET", key,
			})
			return err == nil && strings.TrimSpace(output) == expected, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться ключа %s на реплике ordinal %d: %v", key, process.Ordinal, err)
	}
}

func (h *harness) directReplicationPosition(
	t *testing.T,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
) (string, int64) {
	t.Helper()
	values := h.directReplicationInfo(t, instance, process)
	offsetName := "master_repl_offset"
	if values["role"] == "slave" {
		offsetName = "slave_repl_offset"
	}
	offset, err := strconv.ParseInt(values[offsetName], 10, 64)
	if err != nil {
		t.Fatalf("разобрать offset ordinal %d: %v", process.Ordinal, err)
	}
	return values["master_replid"], offset
}

func (h *harness) disconnectReplicaClients(
	t *testing.T,
	instance *testInstance,
	primary valkeyv1alpha1.NodeStatus,
) {
	t.Helper()
	pod := h.getPodOrdinal(t, instance, primary.Ordinal)
	operatorPassword := h.servicePasswords(t, instance)[0]
	output, err := execInPod(t, h.adminREST, pod.Namespace, pod.Name, []string{
		"valkey-cli", "--user", "operator", "--pass", operatorPassword,
		"--no-auth-warning", "--raw", "CLIENT", "KILL", "TYPE", "replica",
	})
	if err != nil {
		t.Fatalf("разорвать соединения реплик на ordinal %d: output=%q error=%v", primary.Ordinal, output, err)
	}
}

func (h *harness) waitForConnectedReplicaCount(
	t *testing.T,
	instance *testInstance,
	primary valkeyv1alpha1.NodeStatus,
	expected int,
) {
	t.Helper()
	err := wait.PollUntilContextTimeout(
		t.Context(),
		50*time.Millisecond,
		5*time.Second,
		true,
		func(context.Context) (bool, error) {
			values := h.directReplicationInfo(t, instance, primary)
			count, parseErr := strconv.Atoi(values["connected_slaves"])
			return parseErr == nil && count == expected, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться %d подключённых реплик на ordinal %d: %v", expected, primary.Ordinal, err)
	}
}

func (h *harness) directReplicationInfo(
	t *testing.T,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
) map[string]string {
	t.Helper()
	pod := h.getPodOrdinal(t, instance, process.Ordinal)
	operatorPassword := h.servicePasswords(t, instance)[0]
	output, err := execInPod(t, h.adminREST, pod.Namespace, pod.Name, []string{
		"valkey-cli", "--user", "operator", "--pass", operatorPassword,
		"--no-auth-warning", "--raw", "INFO", "replication",
	})
	if err != nil {
		t.Fatalf("прочитать INFO replication ordinal %d: %v", process.Ordinal, err)
	}
	values := make(map[string]string)
	for line := range strings.SplitSeq(output, "\n") {
		name, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if found {
			values[name] = value
		}
	}
	return values
}

func processForIdentity(
	t *testing.T,
	processes []valkeyv1alpha1.NodeStatus,
	identity valkeyv1alpha1.ProcessIdentity,
) valkeyv1alpha1.NodeStatus {
	t.Helper()
	for _, process := range processes {
		if process.PodUID == identity.PodUID && process.ContainerID == identity.ContainerID &&
			process.RunID == identity.RunID && process.NodeName == identity.NodeName &&
			process.NodeUID == identity.NodeUID {
			return process
		}
	}
	t.Fatalf("процесс с идентичностью %+v отсутствует", identity)
	return valkeyv1alpha1.NodeStatus{}
}

func processIdentityForTest(process valkeyv1alpha1.NodeStatus) valkeyv1alpha1.ProcessIdentity {
	return valkeyv1alpha1.ProcessIdentity{
		PodUID: process.PodUID, ContainerID: process.ContainerID, RunID: process.RunID,
		NodeName: process.NodeName, NodeUID: process.NodeUID,
	}
}

func hasTerminationEvidence(
	processes []valkeyv1alpha1.NodeStatus,
	identity valkeyv1alpha1.ProcessIdentity,
	evidence string,
) bool {
	for _, process := range processes {
		if processIdentityForTest(process) == identity && process.Termination != nil &&
			process.Termination.Evidence == evidence {
			return true
		}
	}
	return false
}

func (h *harness) openReadOnlyConnectionForRunID(
	t *testing.T,
	instance *testInstance,
	runID string,
) *persistentConnection {
	t.Helper()
	for range 12 {
		connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
		writeRESP(t, connection, "INFO", "server")
		response, ok := readRESP(t, connection).(string)
		if ok && replicationInfoValue(response, "run_id") == runID {
			return connection
		}
		closeConnections([]*persistentConnection{connection})
	}
	t.Fatalf("маршрут чтения не подключился к процессу %s", runID)
	return nil
}

func replicationInfoValue(info, name string) string {
	for line := range strings.SplitSeq(info, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if found && key == name {
			return value
		}
	}
	return ""
}

func sameProcessIdentityMap(
	left map[int32]valkeyv1alpha1.ProcessIdentity,
	right map[int32]valkeyv1alpha1.ProcessIdentity,
) bool {
	if len(left) != len(right) {
		return false
	}
	for ordinal, identity := range left {
		if right[ordinal] != identity {
			return false
		}
	}
	return true
}

func matrixScenarioID(t *testing.T) string {
	t.Helper()
	name := strings.TrimPrefix(t.Name(), "Test")
	if len(name) < 4 || name[0] < 'A' || name[0] > 'Z' || name[1] < 'A' || name[1] > 'Z' ||
		name[2] < '0' || name[2] > '9' || name[3] < '0' || name[3] > '9' {
		t.Fatalf("имя теста %s не начинается с ID матрицы", t.Name())
	}
	return name[:2] + "-" + name[2:4]
}

func (h *harness) recordFaultEvent(t *testing.T, action, target string) {
	t.Helper()
	h.recordScenarioEvent(t, matrixScenarioID(t), action, target)
}

func (h *harness) setManualFencing(
	t *testing.T,
	instance *testInstance,
	identity valkeyv1alpha1.ProcessIdentity,
) {
	t.Helper()
	encoded, err := json.Marshal(identity)
	if err != nil {
		t.Fatalf("закодировать идентичность ручного fencing: %v", err)
	}
	key := client.ObjectKey{Name: instance.slug, Namespace: instance.namespace}
	err = retry.RetryOnConflict(wait.Backoff{
		Duration: 100 * time.Millisecond, Factor: 2, Steps: 8,
	}, func() error {
		current := &valkeyv1alpha1.ValkeyInstance{}
		if getErr := h.k8s.Get(t.Context(), key, current); getErr != nil {
			return getErr
		}
		if current.Annotations == nil {
			current.Annotations = make(map[string]string)
		}
		current.Annotations["valkey.h3llo-demo.com/manual-fencing"] = string(encoded)
		return h.k8s.Update(t.Context(), current)
	})
	if err != nil {
		t.Fatalf("сохранить ручное fencing: %v", err)
	}
	h.recordFaultEvent(t, "manual-fencing", identity.NodeName)
}

func (h *harness) recordScenarioEvent(t *testing.T, scenario, action, target string) {
	t.Helper()
	script := filepath.Join(requiredEnv(t, "MANAGED_VALKEY_REPO_ROOT"), "scripts", "record_operator_fault.sh")
	output, err := exec.Command(
		script,
		requiredEnv(t, "MV_STATE_DIR"),
		scenario,
		action,
		target,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("записать событие отказа %s: output=%q error=%v", scenario, output, err)
	}
}

type tcpFaultProxy struct {
	listener    net.Listener
	target      string
	mu          sync.Mutex
	blocked     bool
	connections map[net.Conn]struct{}
}

type nodeNetworkFault struct {
	script   string
	stateDir string
	id       string
	restored bool
}

type valkeyCommandPoint struct {
	address   string
	name      string
	stage     operatorvalkey.IntegrationCommandStage
	loseReply bool
	match     uint32
	reached   chan struct{}
	release   chan struct{}
	seen      atomic.Uint32
	triggered atomic.Bool
	closeOnce sync.Once
	uninstall func()
}

type processActionPoint struct {
	reached   chan struct{}
	release   chan struct{}
	triggered atomic.Bool
	uninstall func()
}

type rolloutActionPoint struct {
	reached   chan struct{}
	release   chan struct{}
	triggered atomic.Bool
	uninstall func()
}

func newProcessActionPoint(
	t *testing.T,
	name string,
	podUID string,
	evidence string,
) *processActionPoint {
	t.Helper()
	point := &processActionPoint{reached: make(chan struct{}), release: make(chan struct{})}
	point.uninstall = operatorcontroller.InstallIntegrationProcessActionControl(func(
		ctx context.Context,
		event operatorcontroller.IntegrationProcessActionEvent,
	) error {
		if event.Name != name || podUID != "" && event.PodUID != podUID ||
			(evidence != "" && event.Evidence != evidence) || !point.triggered.CompareAndSwap(false, true) {
			return nil
		}
		close(point.reached)
		select {
		case <-point.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	t.Cleanup(point.close)
	return point
}

func (p *processActionPoint) waitFor(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-p.reached:
	case <-time.After(timeout):
		t.Fatal("управляемая граница действия с процессом не достигнута")
	}
}

func (p *processActionPoint) close() {
	p.uninstall()
	select {
	case <-p.release:
	default:
		close(p.release)
	}
}

func newRolloutActionPoint(
	t *testing.T,
	stage valkeyv1alpha1.RolloutStage,
	desiredReplicas int32,
) *rolloutActionPoint {
	t.Helper()
	return newRolloutEventPoint(t, func(event operatorcontroller.IntegrationRolloutActionEvent) bool {
		return event.Name == "before-statefulset-update" && event.Stage == stage &&
			event.DesiredReplicas == desiredReplicas
	})
}

func newNamedRolloutActionPoint(
	t *testing.T,
	name string,
	stage valkeyv1alpha1.RolloutStage,
	desiredReplicas int32,
) *rolloutActionPoint {
	t.Helper()
	return newRolloutEventPoint(t, func(event operatorcontroller.IntegrationRolloutActionEvent) bool {
		return event.Name == name && event.Stage == stage && event.DesiredReplicas == desiredReplicas
	})
}

func newRolloutStatusActionPoint(
	t *testing.T,
	name string,
	stage valkeyv1alpha1.RolloutStage,
) *rolloutActionPoint {
	t.Helper()
	return newRolloutEventPoint(t, func(event operatorcontroller.IntegrationRolloutActionEvent) bool {
		return event.Name == name && event.Stage == stage
	})
}

func newRolloutEventPoint(
	t *testing.T,
	matches func(operatorcontroller.IntegrationRolloutActionEvent) bool,
) *rolloutActionPoint {
	t.Helper()
	point := &rolloutActionPoint{reached: make(chan struct{}), release: make(chan struct{})}
	point.uninstall = operatorcontroller.InstallIntegrationRolloutActionControl(func(
		ctx context.Context,
		event operatorcontroller.IntegrationRolloutActionEvent,
	) error {
		if !matches(event) || !point.triggered.CompareAndSwap(false, true) {
			return nil
		}
		close(point.reached)
		select {
		case <-point.release:
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	})
	t.Cleanup(point.close)
	return point
}

func (p *rolloutActionPoint) waitFor(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-p.reached:
	case <-time.After(timeout):
		t.Fatal("управляемая граница StatefulSet не достигнута")
	}
}

func (p *rolloutActionPoint) close() {
	p.uninstall()
	select {
	case <-p.release:
	default:
		close(p.release)
	}
}

func newValkeyCommandPoint(
	t *testing.T,
	address string,
	name string,
	stage operatorvalkey.IntegrationCommandStage,
	loseReply bool,
) *valkeyCommandPoint {
	t.Helper()
	return newValkeyCommandPointAtMatch(t, address, name, stage, loseReply, 1)
}

func newValkeyCommandPointAtMatch(
	t *testing.T,
	address string,
	name string,
	stage operatorvalkey.IntegrationCommandStage,
	loseReply bool,
	match uint32,
) *valkeyCommandPoint {
	t.Helper()
	point := &valkeyCommandPoint{
		address: address, name: name, stage: stage, loseReply: loseReply, match: match,
		reached: make(chan struct{}), release: make(chan struct{}),
	}
	point.uninstall = operatorvalkey.InstallIntegrationCommandControl(func(
		ctx context.Context,
		event operatorvalkey.IntegrationCommandEvent,
	) error {
		if (point.address != "" && event.Address != point.address) || event.Name != point.name ||
			event.Stage != point.stage {
			return nil
		}
		if point.seen.Add(1) != point.match ||
			!point.triggered.CompareAndSwap(false, true) {
			return nil
		}
		close(point.reached)
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-point.release:
			if point.loseReply {
				return operatorvalkey.ErrTransportFailure
			}
			return nil
		}
	})
	t.Cleanup(point.close)
	return point
}

func (p *valkeyCommandPoint) wait(t *testing.T) {
	t.Helper()
	p.waitFor(t, 15*time.Second)
}

func (p *valkeyCommandPoint) waitFor(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-p.reached:
	case <-time.After(timeout):
		t.Fatalf("команда %s не достигла точки %s", p.name, p.stage)
	}
}

func (p *valkeyCommandPoint) close() {
	p.closeOnce.Do(func() {
		p.uninstall()
		close(p.release)
	})
}

func (h *harness) waitForPasswordVersion(
	t *testing.T,
	instance *testInstance,
	version int64,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()
	return h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.CredentialRotation == nil &&
			current.Status.AppliedPasswordVersion == version &&
			current.Status.ObservedGeneration == version &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, fmt.Sprintf("OP-04 завершения версии пароля %d", version))
}

func assertRealProcessRoleACL(
	t *testing.T,
	h *harness,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
	expectedRole string,
	password string,
	expectedEnabled bool,
) {
	t.Helper()
	pod := h.getPodOrdinal(t, instance, process.Ordinal)
	operatorPassword := h.servicePasswords(t, instance)[0]
	roleOutput, err := execInPod(t, h.adminREST, pod.Namespace, pod.Name, []string{
		"valkey-cli", "--user", "operator", "--pass", operatorPassword,
		"--no-auth-warning", "--raw", "ROLE",
	})
	if err != nil || strings.Split(strings.TrimSpace(roleOutput), "\n")[0] != expectedRole {
		t.Fatalf("реальный процесс ordinal %d не подтвердил роль %s: %v", process.Ordinal, expectedRole, err)
	}
	aclOutput, err := execInPod(t, h.adminREST, pod.Namespace, pod.Name, []string{
		"valkey-cli", "--user", "operator", "--pass", operatorPassword,
		"--no-auth-warning", "--json", "ACL", "GETUSER", "app",
	})
	if err != nil {
		t.Fatalf("прочитать ACL реального процесса ordinal %d: %v", process.Ordinal, err)
	}
	acl := struct {
		Flags     []string `json:"flags"`
		Passwords []string `json:"passwords"`
	}{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(aclOutput)), &acl); err != nil {
		t.Fatalf("разобрать ACL реального процесса ordinal %d: %v", process.Ordinal, err)
	}
	digest := sha256.Sum256([]byte(password))
	expectedHash := hex.EncodeToString(digest[:])
	enabled := slices.Contains(acl.Flags, "on") && !slices.Contains(acl.Flags, "off")
	if enabled != expectedEnabled || len(acl.Passwords) != 1 || acl.Passwords[0] != expectedHash {
		t.Fatalf("реальный процесс ordinal %d не подтвердил ожидаемый ACL", process.Ordinal)
	}
}

func (h *harness) addNodeNetworkFault(
	t *testing.T,
	id string,
	nodeName string,
	chain string,
	source string,
	destination string,
	port int,
) *nodeNetworkFault {
	t.Helper()
	script := filepath.Join(requiredEnv(t, "MANAGED_VALKEY_REPO_ROOT"), "scripts", "operator_network_fault.sh")
	fault := &nodeNetworkFault{script: script, stateDir: requiredEnv(t, "MV_STATE_DIR"), id: id}
	output, err := exec.Command(
		script,
		"add",
		fault.stateDir,
		id,
		nodeName,
		chain,
		source,
		destination,
		strconv.Itoa(port),
	).CombinedOutput()
	if err != nil {
		t.Fatalf("установить сетевое ограничение %s: output=%q error=%v", id, output, err)
	}
	t.Cleanup(func() {
		if err := fault.restore(); err != nil {
			t.Errorf("снять сетевое ограничение %s: %v", id, err)
		}
	})
	return fault
}

func (f *nodeNetworkFault) restore() error {
	if f.restored {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, f.script, "remove", f.stateDir, f.id).CombinedOutput()
	if err != nil {
		return fmt.Errorf("output=%q: %w", output, err)
	}
	f.restored = true
	return nil
}

func (f *nodeNetworkFault) assertPresent(t *testing.T) {
	t.Helper()
	f.assertState(t, "present")
}

func (f *nodeNetworkFault) assertAbsent(t *testing.T) {
	t.Helper()
	f.assertState(t, "absent")
}

func (f *nodeNetworkFault) assertState(t *testing.T, expected string) {
	t.Helper()
	output, err := exec.Command(f.script, "check", f.stateDir, f.id, expected).CombinedOutput()
	if err != nil {
		t.Fatalf("проверить сетевое ограничение %s: output=%q error=%v", f.id, output, err)
	}
}

func assertTCPBlocked(t *testing.T, address string) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		_ = connection.Close()
		t.Fatalf("адресный TCP-разрыв не закрыл %s", address)
	}
	if netError, ok := err.(net.Error); !ok || !netError.Timeout() {
		t.Fatalf("TCP-разрыв %s вернул не timeout: %v", address, err)
	}
}

func directValkeyRoleAtAddress(t *testing.T, address, password string) string {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		t.Fatalf("подключиться к Valkey %s: %v", address, err)
	}
	client := &persistentConnection{conn: connection, read: bufio.NewReader(connection), stop: func() {}}
	defer closeConnections([]*persistentConnection{client})
	writeRESP(t, client, "AUTH", "operator", password)
	if response := readRESP(t, client); response != "OK" {
		t.Fatalf("AUTH operator к %s вернул %#v", address, response)
	}
	writeRESP(t, client, "ROLE")
	response := readRESP(t, client)
	values, ok := response.([]any)
	if !ok || len(values) == 0 {
		t.Fatalf("ROLE %s вернул %#v", address, response)
	}
	role, ok := values[0].(string)
	if !ok {
		t.Fatalf("ROLE %s не содержит роль: %#v", address, response)
	}
	return role
}

func (h *harness) nodeInternalAddress(t *testing.T, nodeName string) string {
	t.Helper()
	node := &corev1.Node{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{Name: nodeName}, node); err != nil {
		t.Fatalf("прочитать Node %s: %v", nodeName, err)
	}
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			return address.Address
		}
	}
	t.Fatalf("Node %s не содержит InternalIP", nodeName)
	return ""
}

func (h *harness) assertAgentControlPlane(
	t *testing.T,
	nodeName string,
	serverAddress string,
	reachable bool,
) {
	t.Helper()
	container := h.composeNodeContainer(t, nodeName, true)
	output, err := exec.Command(
		"docker",
		"exec",
		container,
		"timeout",
		"2",
		"wget",
		"-O-",
		"http://"+net.JoinHostPort(serverAddress, "6443")+"/readyz",
	).CombinedOutput()
	if reachable {
		if !strings.Contains(string(output), "400 Bad Request") {
			t.Fatalf("agent %s не подтвердил доступ к control-plane: output=%q error=%v", nodeName, output, err)
		}
		return
	}
	exitError, ok := err.(*exec.ExitError)
	if !ok || exitError.ExitCode() != 124 {
		t.Fatalf("agent %s сохранил доступ к control-plane: output=%q error=%v", nodeName, output, err)
	}
}

func newTCPFaultProxy(t *testing.T, target string) *tcpFaultProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("запустить TCP-прокси: %v", err)
	}
	proxy := &tcpFaultProxy{
		listener: listener, target: target, connections: make(map[net.Conn]struct{}),
	}
	go proxy.accept()
	return proxy
}

func (p *tcpFaultProxy) address() string {
	return p.listener.Addr().String()
}

func (p *tcpFaultProxy) accept() {
	for {
		downstream, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.forward(downstream)
	}
}

func (p *tcpFaultProxy) forward(downstream net.Conn) {
	p.mu.Lock()
	if p.blocked {
		p.mu.Unlock()
		_ = downstream.Close()
		return
	}
	upstream, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		p.mu.Unlock()
		_ = downstream.Close()
		return
	}
	p.connections[downstream] = struct{}{}
	p.connections[upstream] = struct{}{}
	p.mu.Unlock()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, downstream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(downstream, upstream)
		done <- struct{}{}
	}()
	<-done
	_ = downstream.Close()
	_ = upstream.Close()
	<-done

	p.mu.Lock()
	delete(p.connections, downstream)
	delete(p.connections, upstream)
	p.mu.Unlock()
}

func (p *tcpFaultProxy) setBlocked(blocked bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blocked = blocked
	if !blocked {
		return
	}
	for connection := range p.connections {
		_ = connection.Close()
	}
	clear(p.connections)
}

func (p *tcpFaultProxy) close() {
	_ = p.listener.Close()
	p.setBlocked(true)
}

func (h *harness) startClusterOperatorOnNode(t *testing.T, nodeName string) {
	t.Helper()
	h.mu.Lock()
	running := h.operator != nil
	h.mu.Unlock()
	if running {
		t.Fatal("хостовый оператор запущен перед кластерным")
	}
	labels := map[string]string{"app.kubernetes.io/name": clusterOperatorName}
	replicas := int32(1)
	var nodeSelector map[string]string
	if nodeName != "" {
		nodeSelector = map[string]string{corev1.LabelHostname: nodeName}
	}
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: clusterOperatorName, Namespace: systemNamespace},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:    clusterOperatorName,
			Replicas:       &replicas,
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			Selector:       &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:            "managed-valkey-operator",
					TerminationGracePeriodSeconds: ptr.To[int64](0),
					NodeSelector:                  nodeSelector,
					Containers: []corev1.Container{{
						Name:            "operator",
						Image:           requiredEnv(t, "MANAGED_VALKEY_OPERATOR_IMAGE"),
						ImagePullPolicy: corev1.PullIfNotPresent,
						Env: []corev1.EnvVar{
							{Name: operatorconfig.EnvSystemNamespace, Value: systemNamespace},
							{Name: operatorconfig.EnvValkeyImage, Value: operatorconfig.DefaultValkeyImage},
							{Name: operatorconfig.EnvBaseDomain, Value: h.baseDomain},
							{Name: operatorconfig.EnvOperatorCIDRs, Value: strings.Join(h.clusterPodCIDRs(t), ",")},
							{Name: operatorconfig.EnvEnvoyProcesses, Value: strconv.Itoa(envoyProcessCount(t))},
						},
					}},
				},
			},
		},
	}
	if err := h.k8s.Create(t.Context(), statefulSet); err != nil {
		t.Fatalf("создать StatefulSet оператора: %v", err)
	}
	pod := h.waitForClusterOperatorRestart(t, "", "", -1)
	if nodeName != "" && pod.Spec.NodeName != nodeName {
		t.Fatalf("оператор размещён на %s вместо %s", pod.Spec.NodeName, nodeName)
	}
	h.waitForLeaseHolderChange(t, nil)
	if nodeName == "" {
		return
	}
	current := &appsv1.StatefulSet{}
	key := client.ObjectKey{Name: clusterOperatorName, Namespace: systemNamespace}
	if err := h.k8s.Get(t.Context(), key, current); err != nil {
		t.Fatalf("прочитать StatefulSet оператора: %v", err)
	}
	before := current.DeepCopy()
	current.Spec.Template.Spec.NodeSelector = nil
	if err := h.k8s.Patch(t.Context(), current, client.MergeFrom(before)); err != nil {
		t.Fatalf("разрешить перенос оператора после потери ноды: %v", err)
	}
}

func (h *harness) stopClusterOperator(t *testing.T) {
	t.Helper()
	statefulSet := &appsv1.StatefulSet{}
	key := client.ObjectKey{Name: clusterOperatorName, Namespace: systemNamespace}
	if err := h.k8s.Get(context.Background(), key, statefulSet); err == nil {
		if err := h.k8s.Delete(context.Background(), statefulSet); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("удалить StatefulSet оператора: %v", err)
		}
		_ = wait.PollUntilContextTimeout(
			context.Background(), time.Second, 2*time.Minute, true,
			func(ctx context.Context) (bool, error) {
				current := &appsv1.StatefulSet{}
				err := h.k8s.Get(ctx, key, current)
				return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
			},
		)
	} else if !apierrors.IsNotFound(err) {
		t.Errorf("прочитать StatefulSet оператора при очистке: %v", err)
	}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: operatorconfig.LeaderElectionID, Namespace: systemNamespace,
	}}
	if err := h.k8s.Delete(context.Background(), lease); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("удалить Lease оператора: %v", err)
	}
}

func (h *harness) clusterOperatorPod(t *testing.T) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	key := client.ObjectKey{Name: clusterOperatorName + "-0", Namespace: systemNamespace}
	if err := h.k8s.Get(t.Context(), key, pod); err != nil {
		t.Fatalf("прочитать Pod оператора: %v", err)
	}
	return pod
}

func (h *harness) waitForClusterOperatorRestart(
	t *testing.T,
	podUID types.UID,
	containerID string,
	restartCount int32,
) *corev1.Pod {
	t.Helper()
	return h.waitForClusterOperatorPod(
		t,
		clusterOperatorName+"-0",
		podUID,
		containerID,
		restartCount,
	)
}

func (h *harness) waitForClusterOperatorPod(
	t *testing.T,
	podName string,
	podUID types.UID,
	containerID string,
	restartCount int32,
) *corev1.Pod {
	t.Helper()
	var result corev1.Pod
	err := wait.PollUntilContextTimeout(
		t.Context(), time.Second, 3*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			pod := &corev1.Pod{}
			key := client.ObjectKey{Name: podName, Namespace: systemNamespace}
			if err := h.k8s.Get(ctx, key, pod); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			container := namedContainerStatus(pod.Status.ContainerStatuses, "operator")
			if container == nil || container.State.Running == nil || container.ContainerID == "" {
				return false, nil
			}
			if podUID != "" && pod.UID == podUID && container.ContainerID == containerID {
				return false, nil
			}
			if restartCount >= 0 && pod.UID == podUID && container.RestartCount <= restartCount {
				return false, nil
			}
			result = *pod.DeepCopy()
			return true, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться процесса оператора: %v", err)
	}
	return result.DeepCopy()
}

func (h *harness) scaleClusterOperator(t *testing.T, replicas int32) {
	t.Helper()
	key := client.ObjectKey{Name: clusterOperatorName, Namespace: systemNamespace}
	if err := retry.RetryOnConflict(wait.Backoff{
		Duration: 100 * time.Millisecond, Factor: 2, Steps: 8,
	}, func() error {
		statefulSet := &appsv1.StatefulSet{}
		if err := h.k8s.Get(t.Context(), key, statefulSet); err != nil {
			return err
		}
		statefulSet.Spec.Replicas = ptr.To(replicas)
		return h.k8s.Update(t.Context(), statefulSet)
	}); err != nil {
		t.Fatalf("изменить число процессов оператора до %d: %v", replicas, err)
	}
}

func (h *harness) forceOperatorLeaseHolder(t *testing.T, holder string) {
	t.Helper()
	key := client.ObjectKey{Name: operatorconfig.LeaderElectionID, Namespace: systemNamespace}
	if err := retry.RetryOnConflict(wait.Backoff{
		Duration: 100 * time.Millisecond, Factor: 2, Steps: 8,
	}, func() error {
		lease := &coordinationv1.Lease{}
		if err := h.k8s.Get(t.Context(), key, lease); err != nil {
			return err
		}
		now := metav1.NewMicroTime(time.Now())
		lease.Spec.HolderIdentity = ptr.To(holder)
		lease.Spec.RenewTime = &now
		return h.k8s.Update(t.Context(), lease)
	}); err != nil {
		t.Fatalf("передать Lease фиктивному владельцу: %v", err)
	}
}

func (h *harness) waitForOperatorLeaseHolder(t *testing.T, excluded ...string) string {
	t.Helper()
	excludedHolders := make(map[string]struct{}, len(excluded))
	for _, holder := range excluded {
		excludedHolders[holder] = struct{}{}
	}
	var result string
	err := wait.PollUntilContextTimeout(
		t.Context(),
		time.Second,
		2*time.Minute,
		true,
		func(ctx context.Context) (bool, error) {
			lease := &coordinationv1.Lease{}
			key := client.ObjectKey{Name: operatorconfig.LeaderElectionID, Namespace: systemNamespace}
			if err := h.k8s.Get(ctx, key, lease); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
				return false, nil
			}
			if _, found := excludedHolders[*lease.Spec.HolderIdentity]; found {
				return false, nil
			}
			result = *lease.Spec.HolderIdentity
			return true, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться нового фактического владельца Lease: %v", err)
	}
	return result
}

func (h *harness) operatorLease(t *testing.T) *coordinationv1.Lease {
	t.Helper()
	lease := &coordinationv1.Lease{}
	key := client.ObjectKey{Name: operatorconfig.LeaderElectionID, Namespace: systemNamespace}
	if err := h.k8s.Get(t.Context(), key, lease); err != nil {
		t.Fatalf("прочитать Lease оператора: %v", err)
	}
	return lease
}

func (h *harness) waitForLeaseHolderChange(t *testing.T, previous *string) *coordinationv1.Lease {
	t.Helper()
	var result coordinationv1.Lease
	err := wait.PollUntilContextTimeout(
		t.Context(), time.Second, 2*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			lease := &coordinationv1.Lease{}
			key := client.ObjectKey{Name: operatorconfig.LeaderElectionID, Namespace: systemNamespace}
			if err := h.k8s.Get(ctx, key, lease); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" ||
				previous != nil && *lease.Spec.HolderIdentity == *previous {
				return false, nil
			}
			result = *lease.DeepCopy()
			return true, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться нового владельца Lease: %v", err)
	}
	return result.DeepCopy()
}

func (h *harness) clusterPodCIDRs(t *testing.T) []string {
	t.Helper()
	nodes := &corev1.NodeList{}
	if err := h.k8s.List(t.Context(), nodes); err != nil {
		t.Fatalf("прочитать Pod CIDR нод: %v", err)
	}
	result := make([]string, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		if node.Spec.PodCIDR == "" {
			t.Fatalf("у Node %s отсутствует Pod CIDR", node.Name)
		}
		result = append(result, node.Spec.PodCIDR)
	}
	return result
}

func (h *harness) sigkillContainer(t *testing.T, nodeName, containerID string) {
	h.signalContainer(t, nodeName, containerID, "KILL")
}

func (h *harness) signalContainer(t *testing.T, nodeName, containerID, signal string) {
	t.Helper()
	nodeContainer, id, pid := h.containerProcess(t, nodeName, containerID)
	if output, err := exec.Command(
		"docker", "exec", nodeContainer, "kill", "-"+signal, strconv.Itoa(pid),
	).CombinedOutput(); err != nil {
		t.Fatalf("отправить SIG%s процессу %s: output=%q error=%v", signal, id, output, err)
	}
	h.recordFaultEvent(t, "container-signal", nodeName+"/"+signal)
}

func (h *harness) containerProcess(t *testing.T, nodeName, containerID string) (string, string, int) {
	t.Helper()
	nodeContainer := h.composeNodeContainer(t, nodeName, true)
	id := strings.TrimPrefix(containerID, "containerd://")
	if id == containerID || id == "" {
		t.Fatalf("неизвестный container ID %q", containerID)
	}
	output, err := exec.Command("docker", "exec", nodeContainer, "crictl", "inspect", id).Output()
	if err != nil {
		t.Fatalf("прочитать процесс контейнера %s: %v", id, err)
	}
	inspection := struct {
		Info struct {
			PID int `json:"pid"`
		} `json:"info"`
		Status struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"status"`
	}{}
	if err := json.Unmarshal(output, &inspection); err != nil {
		t.Fatalf("разобрать состояние контейнера %s: %v", id, err)
	}
	if inspection.Status.ID != id || inspection.Status.State != "CONTAINER_RUNNING" || inspection.Info.PID <= 1 {
		t.Fatalf("контейнер %s не является работающей целью: %+v", id, inspection)
	}
	return nodeContainer, id, inspection.Info.PID
}

func (h *harness) valkeyProcessPID(t *testing.T, nodeName, containerID string) (string, string, int) {
	t.Helper()
	nodeContainer, id, initPID := h.containerProcess(t, nodeName, containerID)
	command := `init_pid=$1
if [ "$(cat /proc/$init_pid/comm)" = valkey-server ]; then
  printf '%s\n' "$init_pid"
  exit 0
fi
for child_pid in $(cat /proc/$init_pid/task/$init_pid/children); do
  if [ "$(cat /proc/$child_pid/comm)" = valkey-server ]; then
    printf '%s\n' "$child_pid"
    exit 0
  fi
done
exit 1`
	output, err := exec.Command(
		"docker", "exec", nodeContainer, "sh", "-c", command, "find-valkey", strconv.Itoa(initPID),
	).CombinedOutput()
	if err != nil {
		t.Fatalf("найти Valkey в контейнере %s: output=%q error=%v", id, output, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil || pid <= 1 {
		t.Fatalf("неверный PID Valkey в контейнере %s: %q", id, output)
	}
	return nodeContainer, id, pid
}

func (h *harness) sigkillValkeyProcess(
	t *testing.T,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
) {
	t.Helper()
	h.signalValkeyProcess(t, instance, process, "KILL")
}

func (h *harness) signalValkeyProcess(
	t *testing.T,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
	signal string,
) {
	t.Helper()
	pod := h.getPodOrdinal(t, instance, process.Ordinal)
	if string(pod.UID) != process.PodUID || pod.Spec.NodeName != process.NodeName {
		t.Fatalf("process status не совпал с Pod %s: %+v", pod.Name, process)
	}
	container := podContainer(t, pod, "valkey")
	if container.ContainerID != process.ContainerID {
		t.Fatalf("container status не совпал с process status: %s != %s", container.ContainerID, process.ContainerID)
	}
	nodeContainer, id, pid := h.valkeyProcessPID(t, pod.Spec.NodeName, container.ContainerID)
	if output, err := exec.Command(
		"docker", "exec", nodeContainer, "kill", "-"+signal, strconv.Itoa(pid),
	).CombinedOutput(); err != nil {
		t.Fatalf("отправить SIG%s процессу Valkey %s: output=%q error=%v", signal, id, output, err)
	}
	h.recordFaultEvent(
		t,
		"valkey-signal",
		pod.Spec.NodeName+"/"+strconv.FormatInt(int64(process.Ordinal), 10)+"/"+signal,
	)
}

func (h *harness) waitForValkeyProcessState(
	t *testing.T,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
	expected string,
	description string,
) {
	t.Helper()
	err := wait.PollUntilContextTimeout(
		t.Context(),
		50*time.Millisecond,
		5*time.Second,
		true,
		func(context.Context) (bool, error) {
			pod := h.getPodOrdinal(t, instance, process.Ordinal)
			if string(pod.UID) != process.PodUID {
				return false, fmt.Errorf("процесс %s заменён до проверки состояния", process.PodUID)
			}
			container := podContainer(t, pod, "valkey")
			nodeContainer, _, pid := h.valkeyProcessPID(t, pod.Spec.NodeName, container.ContainerID)
			output, err := exec.Command(
				"docker",
				"exec",
				nodeContainer,
				"sh",
				"-c",
				"awk '{print $3}' /proc/$1/stat",
				"process-state",
				strconv.Itoa(pid),
			).CombinedOutput()
			if err != nil {
				return false, fmt.Errorf("прочитать состояние Valkey: output=%q error=%w", output, err)
			}
			state := strings.TrimSpace(string(output))
			if expected == "S" {
				return state != "T" && state != "t", nil
			}
			return state == expected, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться %s: %v", description, err)
	}
}

func (h *harness) waitForTransportObservation(
	t *testing.T,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
) {
	t.Helper()
	h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		observed, found := nodeStatusAtOrdinal(current.Status.Nodes, process.Ordinal)
		return found && observed.PodUID == process.PodUID && observed.ContainerID == process.ContainerID &&
			observed.Observation != nil &&
			observed.Observation.Kind == valkeyv1alpha1.ProcessObservationTransportError &&
			observed.Observation.ConsecutiveTransportErrors > 0 && observed.Recovery == nil
	}, "первой транспортной ошибки после SIGSTOP")
}

func (h *harness) startBusyCommand(t *testing.T, connection *persistentConnection, command ...string) {
	t.Helper()
	writeRESP(t, connection, command...)
	if err := connection.conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("установить срок BUSY-команды: %v", err)
	}
}

func (h *harness) waitForBusyObservation(
	t *testing.T,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()
	return h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		observed, found := nodeStatusAtOrdinal(current.Status.Nodes, process.Ordinal)
		return found && sameNodeProcess(process, observed) && observed.Observation != nil &&
			observed.Observation.Kind == valkeyv1alpha1.ProcessObservationBusy &&
			observed.Observation.BusySince != nil && observed.Recovery == nil
	}, "наблюдения BUSY")
}

func (h *harness) waitForUnkillableBusyDeletion(
	t *testing.T,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
) {
	t.Helper()
	h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		observed, found := nodeStatusAtOrdinal(current.Status.Nodes, process.Ordinal)
		return found && sameNodeProcess(process, observed) && observed.Recovery != nil &&
			observed.Recovery.Reason == valkeyv1alpha1.ProcessRecoveryBusy &&
			(observed.Recovery.Stage == valkeyv1alpha1.ProcessRecoveryStageDeleting ||
				observed.Recovery.Stage == valkeyv1alpha1.ProcessRecoveryStageWaitingForTermination)
	}, "FP-09 решения остановить UNKILLABLE")
}

func (h *harness) loadBusyFunctions(t *testing.T, instance *testInstance, operatorPassword string) {
	t.Helper()
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	writeRESP(t, connection, "AUTH", "operator", operatorPassword)
	if response := readRESP(t, connection); response != "OK" {
		t.Fatalf("AUTH operator перед FUNCTION LOAD вернул %#v", response)
	}
	library := `#!lua name=operator_busy
redis.register_function('busy_read', function(keys, args)
  while true do redis.call('GET', 'busy-read-function') end
end)
redis.register_function('busy_write', function(keys, args)
  redis.call('SET', 'busy-written-function', 'value')
  while true do redis.call('GET', 'busy-written-function') end
end)`
	writeRESP(t, connection, "FUNCTION", "LOAD", "REPLACE", library)
	if response := readRESP(t, connection); response != "operator_busy" {
		t.Fatalf("FUNCTION LOAD вернул %#v", response)
	}
}

func (h *harness) assertMissingKey(t *testing.T, instance *testInstance, key string) {
	t.Helper()
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	writeRESP(t, connection, "GET", key)
	if value := readRESP(t, connection); value != nil {
		t.Fatalf("FP-09 восстановил ключ %s из остановленного процесса: %#v", key, value)
	}
}

func assertBusyCommandKilled(t *testing.T, connection *persistentConnection, description string) {
	t.Helper()
	value, err := readRESPResult(t, connection)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "killed") {
		t.Fatalf("FP-08 %s не получила ответ KILL: value=%#v error=%v", description, value, err)
	}
}

func assertSameProcess(
	t *testing.T,
	expected valkeyv1alpha1.NodeStatus,
	observed valkeyv1alpha1.NodeStatus,
	description string,
) {
	t.Helper()
	if !sameNodeProcess(expected, observed) {
		t.Fatalf("FP-08 %s заменил процесс: before=%+v after=%+v", description, expected, observed)
	}
}

func sameNodeProcess(left, right valkeyv1alpha1.NodeStatus) bool {
	return left.Ordinal == right.Ordinal && left.PodUID == right.PodUID &&
		left.ContainerID == right.ContainerID && left.RunID == right.RunID &&
		left.NodeName == right.NodeName && left.NodeUID == right.NodeUID
}

func (h *harness) waitForAcceptedProcessDeletion(
	t *testing.T,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
) {
	t.Helper()
	h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		observed, found := nodeStatusAtOrdinal(current.Status.Nodes, process.Ordinal)
		return found && observed.PodUID == process.PodUID && observed.ContainerID == process.ContainerID &&
			observed.Recovery != nil && observed.Recovery.DeleteRequestedAt != nil &&
			observed.Recovery.Stage == valkeyv1alpha1.ProcessRecoveryStageWaitingForTermination
	}, "FP-07 принятого DELETE после SIGSTOP")
}

func (h *harness) waitForSameProcessHealthy(
	t *testing.T,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
	description string,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()
	return h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		observed, found := nodeStatusAtOrdinal(current.Status.Nodes, process.Ordinal)
		return found && observed.PodUID == process.PodUID && observed.ContainerID == process.ContainerID &&
			observed.Readiness && observed.Observation == nil && observed.Recovery == nil &&
			current.Status.Failover == nil && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, description)
}

func nodeStatusAtOrdinal(
	statuses []valkeyv1alpha1.NodeStatus,
	ordinal int32,
) (valkeyv1alpha1.NodeStatus, bool) {
	for _, status := range statuses {
		if status.Ordinal == ordinal {
			return status, true
		}
	}
	return valkeyv1alpha1.NodeStatus{}, false
}

func dataLossCondition(conditions []metav1.Condition) bool {
	for _, condition := range conditions {
		if condition.Type == "DataLoss" && condition.Status == metav1.ConditionTrue &&
			condition.Reason == "AllProcessesStopped" {
			return true
		}
	}
	return false
}

func (h *harness) assertInstanceEvent(t *testing.T, instance *testInstance, reason string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(
		t.Context(),
		250*time.Millisecond,
		10*time.Second,
		true,
		func(ctx context.Context) (bool, error) {
			events := &corev1.EventList{}
			if err := h.k8s.List(ctx, events, client.InNamespace(instance.namespace)); err != nil {
				return false, err
			}
			for _, event := range events.Items {
				if event.InvolvedObject.Name == instance.slug && event.Reason == reason {
					return true, nil
				}
			}
			return false, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться Event %s для %s: %v", reason, instance.slug, err)
	}
}

func processAtOrdinal(
	t *testing.T,
	instance *valkeyv1alpha1.ValkeyInstance,
	ordinal int32,
) valkeyv1alpha1.NodeStatus {
	t.Helper()
	process, found := nodeStatusAtOrdinal(instance.Status.Nodes, ordinal)
	if !found {
		t.Fatalf("ordinal %d отсутствует в status: %+v", ordinal, instance.Status.Nodes)
	}
	return process
}

func (h *harness) setNodeUnschedulable(t *testing.T, name string, unschedulable bool) {
	t.Helper()
	ctx := context.Background()
	node := &corev1.Node{}
	if err := h.k8s.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
		t.Fatalf("прочитать Node %s: %v", name, err)
	}
	if node.Spec.Unschedulable == unschedulable {
		return
	}
	before := node.DeepCopy()
	node.Spec.Unschedulable = unschedulable
	if err := h.k8s.Patch(ctx, node, client.MergeFrom(before)); err != nil {
		t.Fatalf("изменить schedulable Node %s: %v", name, err)
	}
}

func (h *harness) waitForPendingPod(t *testing.T, instance *testInstance) {
	t.Helper()
	err := wait.PollUntilContextTimeout(
		t.Context(), time.Second, 2*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			pods := &corev1.PodList{}
			if err := h.k8s.List(
				ctx,
				pods,
				client.InNamespace(instance.namespace),
				client.MatchingLabels{instanceLabel: instance.slug},
			); err != nil {
				return false, err
			}
			for _, pod := range pods.Items {
				if pod.Spec.NodeName == "" && pod.Status.Phase == corev1.PodPending {
					return true, nil
				}
			}
			return false, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться Pending Pod HA-03: %v", err)
	}
}

func (h *harness) waitPodReady(
	t *testing.T,
	instance *testInstance,
	ordinal int32,
	expected bool,
) {
	t.Helper()
	err := wait.PollUntilContextTimeout(
		t.Context(), time.Second, time.Minute, true,
		func(ctx context.Context) (bool, error) {
			pod := &corev1.Pod{}
			key := client.ObjectKey{
				Name:      instance.slug + "-" + strconv.FormatInt(int64(ordinal), 10),
				Namespace: instance.namespace,
			}
			if err := h.k8s.Get(ctx, key, pod); err != nil {
				return false, err
			}
			return podReady(pod.Status.Conditions) == expected, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться readiness=%t Pod ordinal %d: %v", expected, ordinal, err)
	}
}

func (h *harness) waitValkeyTerminatedWithoutReplacement(
	t *testing.T,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
) {
	t.Helper()
	err := wait.PollUntilContextTimeout(
		t.Context(), time.Second, time.Minute, true,
		func(ctx context.Context) (bool, error) {
			pod := &corev1.Pod{}
			key := client.ObjectKey{
				Name:      instance.slug + "-" + strconv.FormatInt(int64(process.Ordinal), 10),
				Namespace: instance.namespace,
			}
			if err := h.k8s.Get(ctx, key, pod); err != nil {
				return false, err
			}
			container := namedContainerStatus(pod.Status.ContainerStatuses, "valkey")
			return string(pod.UID) == process.PodUID && container != nil &&
				container.ContainerID == process.ContainerID && container.State.Terminated != nil, nil
		},
	)
	if err != nil {
		t.Fatalf("FP-05 дождаться остановленного ordinal %d без оператора: %v", process.Ordinal, err)
	}
}

func (h *harness) waitForPDBDisruptions(t *testing.T, instance *testInstance, expected int32) {
	t.Helper()
	err := wait.PollUntilContextTimeout(
		t.Context(), time.Second, time.Minute, true,
		func(ctx context.Context) (bool, error) {
			pdb := &policyv1.PodDisruptionBudget{}
			key := client.ObjectKey{Name: instance.slug, Namespace: instance.namespace}
			if err := h.k8s.Get(ctx, key, pdb); err != nil {
				return false, err
			}
			return pdb.Status.DisruptionsAllowed == expected, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться disruptionsAllowed=%d: %v", expected, err)
	}
}

func (h *harness) requireNodeCount(t *testing.T, expected int) {
	t.Helper()
	nodes := &corev1.NodeList{}
	if err := h.k8s.List(t.Context(), nodes); err != nil {
		t.Fatalf("прочитать Node: %v", err)
	}
	if len(nodes.Items) != expected {
		t.Fatalf("ожидалось Node: %d, получено: %d", expected, len(nodes.Items))
	}
	for _, node := range nodes.Items {
		if !nodeReady(node.Status.Conditions) {
			t.Fatalf("Node %s не Ready", node.Name)
		}
	}
}

func (h *harness) addOperatorAgent(t *testing.T, service string) {
	t.Helper()
	if service != "k3s-agent-3" {
		t.Fatalf("добавление тестовой ноды не разрешено для %s", service)
	}
	script := filepath.Join(requiredEnv(t, "MANAGED_VALKEY_REPO_ROOT"), "scripts", "test_environment.sh")
	output, err := exec.Command(
		script,
		"expand-operator",
		requiredEnv(t, "MV_STATE_DIR"),
		requiredEnv(t, "MANAGED_VALKEY_OPERATOR_IMAGE"),
		requiredEnv(t, "MANAGED_VALKEY_VALKEY_IMAGE"),
	).CombinedOutput()
	if err != nil {
		t.Fatalf("добавить чистый agent %s: output=%q error=%v", service, output, err)
	}
	h.recordFaultEvent(t, "node-added", service)
}

func (h *harness) removeOperatorAgent(t *testing.T, service string) {
	t.Helper()
	if service != "k3s-agent-3" {
		t.Fatalf("удаление тестовой ноды не разрешено для %s", service)
	}
	node := h.getNode(t, service)
	containerID := h.composeNodeContainer(t, service, true)
	if output, err := exec.Command("docker", "stop", containerID).CombinedOutput(); err != nil {
		t.Fatalf("остановить временный agent %s: output=%q error=%v", service, output, err)
	}
	h.deleteNode(t, node)
	passwordSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: service + ".node-password.k3s", Namespace: "kube-system",
	}}
	if err := h.k8s.Delete(t.Context(), passwordSecret); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("удалить пароль временного Node %s: %v", service, err)
	}
	if output, err := exec.Command("docker", "rm", containerID).CombinedOutput(); err != nil {
		t.Fatalf("удалить контейнер временного agent %s: output=%q error=%v", service, output, err)
	}
	volumeName := h.composeVolume(t, service+"-node")
	if output, err := exec.Command("docker", "volume", "rm", volumeName).CombinedOutput(); err != nil {
		t.Fatalf("удалить состояние временного agent %s: output=%q error=%v", service, output, err)
	}
	h.recordFaultEvent(t, "node-removed", service)
}

func nodeReady(conditions []corev1.NodeCondition) bool {
	for _, condition := range conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podReady(conditions []corev1.PodCondition) bool {
	for _, condition := range conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func primaryProcess(t *testing.T, instance *valkeyv1alpha1.ValkeyInstance) valkeyv1alpha1.NodeStatus {
	t.Helper()
	if instance.Status.PrimaryOrdinal == nil {
		t.Fatal("primary ordinal отсутствует")
	}
	for _, process := range instance.Status.Nodes {
		if process.Ordinal == *instance.Status.PrimaryOrdinal &&
			process.Role == valkeyv1alpha1.NodeRolePrimary {
			return process
		}
	}
	t.Fatal("primary process отсутствует")
	return valkeyv1alpha1.NodeStatus{}
}

func (h *harness) useSurvivingEnvoy(t *testing.T, failedNode string) {
	t.Helper()
	pods := &corev1.PodList{}
	if err := h.k8s.List(
		t.Context(),
		pods,
		client.InNamespace("envoy-gateway-system"),
		client.MatchingLabels{"gateway.envoyproxy.io/owning-gateway-name": "valkey"},
	); err != nil {
		t.Fatalf("прочитать Pod Envoy: %v", err)
	}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == failedNode || !podReady(pod.Status.Conditions) {
			continue
		}
		output, err := h.composeCommand("port", pod.Spec.NodeName, "31379").CombinedOutput()
		if err != nil {
			t.Fatalf("прочитать публичный порт %s: output=%q error=%v", pod.Spec.NodeName, output, err)
		}
		h.publicAddr = strings.TrimSpace(string(output))
		h.dockerHost = pod.Spec.NodeName
		return
	}
	t.Fatalf("нет живого Envoy вне Node %s", failedNode)
}

func (h *harness) readyEnvoyPods(t *testing.T) []corev1.Pod {
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
		t.Fatalf("прочитать Pod Envoy: %v", err)
	}
	result := make([]corev1.Pod, 0, len(pods.Items))
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp.IsZero() && podReady(pod.Status.Conditions) {
			result = append(result, *pod.DeepCopy())
		}
	}
	if len(result) != 2 {
		t.Fatalf("ожидалось два готовых Envoy, получено %d", len(result))
	}
	return result
}

func (h *harness) waitForEnvoyReplacement(
	t *testing.T,
	before []corev1.Pod,
	failedNode string,
) []corev1.Pod {
	t.Helper()
	previous := make(map[types.UID]struct{}, len(before))
	for _, pod := range before {
		previous[pod.UID] = struct{}{}
	}
	var result []corev1.Pod
	err := wait.PollUntilContextTimeout(
		t.Context(),
		time.Second,
		3*time.Minute,
		true,
		func(ctx context.Context) (bool, error) {
			pods := &corev1.PodList{}
			if err := h.k8s.List(
				ctx,
				pods,
				client.InNamespace("envoy-gateway-system"),
				client.MatchingLabels{
					"gateway.envoyproxy.io/owning-gateway-name":      "valkey",
					"gateway.envoyproxy.io/owning-gateway-namespace": systemNamespace,
				},
			); err != nil {
				return false, err
			}
			if len(pods.Items) != 2 {
				return false, nil
			}
			newProcess := false
			for _, pod := range pods.Items {
				if !pod.DeletionTimestamp.IsZero() || !podReady(pod.Status.Conditions) ||
					pod.Spec.NodeName == failedNode {
					return false, nil
				}
				if _, found := previous[pod.UID]; !found {
					newProcess = true
				}
			}
			if !newProcess {
				return false, nil
			}
			result = make([]corev1.Pod, len(pods.Items))
			for index := range pods.Items {
				result[index] = *pods.Items[index].DeepCopy()
			}
			return true, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться замены Envoy с ноды %s: %v", failedNode, err)
	}
	return result
}

type agentFault struct {
	service      string
	scenario     string
	nodeUID      types.UID
	containerID  string
	killed       bool
	restored     bool
	imageArchive string
}

func (h *harness) newAgentFault(t *testing.T, node *corev1.Node) *agentFault {
	t.Helper()
	if node.Name != "k3s-agent-1" && node.Name != "k3s-agent-2" && node.Name != "k3s-agent-3" {
		t.Fatalf("отказ разрешён только для agent текущего стенда, получен %s", node.Name)
	}
	return &agentFault{
		service: node.Name, scenario: matrixScenarioID(t), nodeUID: node.UID,
		containerID:  h.composeNodeContainer(t, node.Name, true),
		imageArchive: requiredEnv(t, "MANAGED_VALKEY_K3S_IMAGE_ARCHIVE"),
	}
}

func (f *agentFault) kill(t *testing.T, h *harness) {
	t.Helper()
	if output, err := exec.Command("docker", "kill", "--signal=KILL", f.containerID).CombinedOutput(); err != nil {
		t.Fatalf("аварийно остановить %s: output=%q error=%v", f.service, output, err)
	}
	f.killed = true
	format := "{{.State.Running}} {{.State.Pid}}"
	output, err := exec.Command("docker", "inspect", f.containerID, "--format", format).CombinedOutput()
	if err != nil {
		t.Fatalf("проверить остановку %s: output=%q error=%v", f.service, output, err)
	}
	if strings.TrimSpace(string(output)) != "false 0" {
		t.Fatalf("agent %s сохранил вложенные процессы: %q", f.service, output)
	}
	h.recordScenarioEvent(t, f.scenario, "agent-killed", f.service)
}

func (f *agentFault) restore(t *testing.T, h *harness) {
	t.Helper()
	if f.restored || !f.killed {
		return
	}
	node := &corev1.Node{}
	if err := h.k8s.Get(context.Background(), client.ObjectKey{Name: f.service}, node); err == nil {
		if node.UID == f.nodeUID {
			uid := node.UID
			resourceVersion := node.ResourceVersion
			if err := h.k8s.Delete(context.Background(), node, &client.DeleteOptions{Raw: &metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
			}}); err != nil && !apierrors.IsNotFound(err) {
				t.Fatalf("удалить Node %s при восстановлении: %v", f.service, err)
			}
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("прочитать Node %s при восстановлении: %v", f.service, err)
	}
	h.waitNodeDeleted(context.Background(), t, f.service)
	passwordSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: f.service + ".node-password.k3s", Namespace: "kube-system",
	}}
	if err := h.k8s.Delete(context.Background(), passwordSecret); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("удалить пароль прежнего Node %s: %v", f.service, err)
	}
	if output, err := exec.Command("docker", "rm", f.containerID).CombinedOutput(); err != nil {
		t.Fatalf("удалить остановленный контейнер %s: output=%q error=%v", f.service, output, err)
	}
	volumeName := h.composeVolume(t, f.service+"-node")
	if output, err := exec.Command("docker", "volume", "rm", volumeName).CombinedOutput(); err != nil {
		t.Fatalf("удалить состояние прежнего Node %s: output=%q error=%v", f.service, output, err)
	}
	if output, err := h.composeCommand("up", "-d", "--wait", f.service).CombinedOutput(); err != nil {
		t.Fatalf("создать чистый agent %s: output=%q error=%v", f.service, output, err)
	}
	loadScript := filepath.Join(requiredEnv(t, "MANAGED_VALKEY_REPO_ROOT"), "scripts", "load_k3s_image.sh")
	if output, err := exec.Command(
		loadScript, requiredEnv(t, "MANAGED_VALKEY_COMPOSE_PROJECT"), "--archive", f.imageArchive, f.service,
	).CombinedOutput(); err != nil {
		t.Fatalf("загрузить образы в новый agent %s: output=%q error=%v", f.service, output, err)
	}
	node = h.waitNodeReadyWithNewUID(t, f.service, f.nodeUID)
	h.ensureNodeRoute(t, node)
	refreshScript := filepath.Join(requiredEnv(t, "MANAGED_VALKEY_REPO_ROOT"), "scripts", "test_environment.sh")
	output, err := exec.Command(
		refreshScript, "refresh-operator-public", requiredEnv(t, "MV_STATE_DIR"),
	).CombinedOutput()
	if err != nil {
		t.Fatalf("обновить публичный адрес после восстановления %s: output=%q error=%v", f.service, output, err)
	}
	publicEndpoint := strings.Fields(string(output))
	if len(publicEndpoint) != 2 {
		t.Fatalf("разобрать публичный адрес после восстановления %s: %q", f.service, output)
	}
	h.publicAddr = publicEndpoint[0]
	h.dockerHost = publicEndpoint[1]
	if err := os.Setenv("MANAGED_VALKEY_PUBLIC_ADDRESS", h.publicAddr); err != nil {
		t.Fatalf("сохранить публичный адрес после восстановления %s: %v", f.service, err)
	}
	if err := os.Setenv("MANAGED_VALKEY_DOCKER_HOST", h.dockerHost); err != nil {
		t.Fatalf("сохранить ноду публичного адреса после восстановления %s: %v", f.service, err)
	}
	h.recordScenarioEvent(t, f.scenario, "agent-restored", f.service)
	f.restored = true
}

func (h *harness) waitNodeDeleted(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(
		ctx, time.Second, 2*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			node := &corev1.Node{}
			err := h.k8s.Get(ctx, client.ObjectKey{Name: name}, node)
			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		},
	)
	if err != nil {
		t.Fatalf("дождаться удаления Node %s: %v", name, err)
	}
}

func (h *harness) deleteNode(t *testing.T, node *corev1.Node) {
	t.Helper()
	uid := node.UID
	resourceVersion := node.ResourceVersion
	if err := h.k8s.Delete(t.Context(), node, &client.DeleteOptions{Raw: &metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
	}}); err != nil {
		t.Fatalf("удалить остановленный Node %s: %v", node.Name, err)
	}
	h.waitNodeDeleted(t.Context(), t, node.Name)
	h.recordFaultEvent(t, "node-deleted", node.Name)
}

func (h *harness) waitNodeReadyWithNewUID(t *testing.T, name string, oldUID types.UID) *corev1.Node {
	t.Helper()
	var result corev1.Node
	err := wait.PollUntilContextTimeout(
		context.Background(), time.Second, 3*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			node := &corev1.Node{}
			if err := h.k8s.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			if node.UID == oldUID || !nodeReady(node.Status.Conditions) || node.Spec.PodCIDR == "" {
				return false, nil
			}
			result = *node.DeepCopy()
			return true, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться чистого Node %s: %v", name, err)
	}
	return result.DeepCopy()
}

func (h *harness) ensureNodeRoute(t *testing.T, node *corev1.Node) {
	t.Helper()
	internalIP := ""
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			internalIP = address.Address
			break
		}
	}
	if node.Spec.PodCIDR == "" || internalIP == "" {
		t.Fatalf("у Node %s нет Pod CIDR или InternalIP", node.Name)
	}
	if output, err := exec.Command(
		"sudo", "ip", "route", "replace", node.Spec.PodCIDR, "via", internalIP,
	).CombinedOutput(); err != nil {
		t.Fatalf("обновить маршрут Node %s: output=%q error=%v", node.Name, output, err)
	}
	routesFile := requiredEnv(t, "ROUTES_FILE")
	file, err := os.OpenFile(routesFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("открыть журнал маршрутов: %v", err)
	}
	if _, err := fmt.Fprintf(file, "%s\t%s\n", node.Spec.PodCIDR, internalIP); err != nil {
		_ = file.Close()
		t.Fatalf("сохранить маршрут Node %s: %v", node.Name, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("закрыть журнал маршрутов: %v", err)
	}
}

func (h *harness) composeNodeContainer(t *testing.T, service string, running bool) string {
	t.Helper()
	project := requiredEnv(t, "MANAGED_VALKEY_COMPOSE_PROJECT")
	arguments := []string{
		"ps", "-aq",
		"--filter", "label=com.docker.compose.project=" + project,
		"--filter", "label=com.docker.compose.service=" + service,
	}
	output, err := exec.Command("docker", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("найти контейнер %s: output=%q error=%v", service, output, err)
	}
	containers := strings.Fields(string(output))
	if len(containers) != 1 {
		t.Fatalf("для %s/%s найдено контейнеров: %d", project, service, len(containers))
	}
	format := "{{index .Config.Labels \"com.docker.compose.project\"}} " +
		"{{index .Config.Labels \"com.docker.compose.service\"}} {{.State.Running}}"
	output, err = exec.Command("docker", "inspect", containers[0], "--format", format).CombinedOutput()
	if err != nil {
		t.Fatalf("проверить контейнер %s: output=%q error=%v", service, output, err)
	}
	expected := fmt.Sprintf("%s %s %t", project, service, running)
	if strings.TrimSpace(string(output)) != expected {
		t.Fatalf("контейнер %s не совпал с целью: %q", service, output)
	}
	return containers[0]
}

func (h *harness) composeVolume(t *testing.T, logicalName string) string {
	t.Helper()
	project := requiredEnv(t, "MANAGED_VALKEY_COMPOSE_PROJECT")
	output, err := exec.Command(
		"docker", "volume", "ls", "-q",
		"--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.volume="+logicalName,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("найти volume %s: output=%q error=%v", logicalName, output, err)
	}
	volumes := strings.Fields(string(output))
	if len(volumes) != 1 {
		t.Fatalf("для %s/%s найдено volumes: %d", project, logicalName, len(volumes))
	}
	return volumes[0]
}

func (h *harness) composeCommand(arguments ...string) *exec.Cmd {
	root := os.Getenv("MANAGED_VALKEY_REPO_ROOT")
	project := os.Getenv("MANAGED_VALKEY_COMPOSE_PROJECT")
	base := []string{
		"compose",
		"-f", filepath.Join(root, "docker-compose.dev.yml"),
		"-f", filepath.Join(root, "docker-compose.test.yml"),
		"-p", project,
	}
	return exec.Command("docker", append(base, arguments...)...)
}

func podContainer(t *testing.T, pod *corev1.Pod, name string) corev1.ContainerStatus {
	t.Helper()
	container := namedContainerStatus(pod.Status.ContainerStatuses, name)
	if container == nil {
		t.Fatalf("контейнер %s отсутствует в Pod %s", name, pod.Name)
	}
	return *container
}

func namedContainerStatus(statuses []corev1.ContainerStatus, name string) *corev1.ContainerStatus {
	for index := range statuses {
		if statuses[index].Name == name {
			return &statuses[index]
		}
	}
	return nil
}

func containerHasStarted(status *corev1.ContainerStatus) bool {
	if status == nil {
		return false
	}
	if status.State.Running != nil {
		return true
	}
	if terminated := status.State.Terminated; terminated != nil && !terminated.StartedAt.IsZero() {
		return true
	}
	terminated := status.LastTerminationState.Terminated
	return terminated != nil && !terminated.StartedAt.IsZero()
}
