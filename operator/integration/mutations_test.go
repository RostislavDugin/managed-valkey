//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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

	envoyv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	operatorconfig "github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorcontroller "github.com/RostislavDugin/managed-valkey/operator/internal/operator"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

type credentialRotationActionPoint struct {
	reached   chan operatorcontroller.IntegrationCredentialRotationActionEvent
	release   chan struct{}
	triggered atomic.Bool
	closeOnce sync.Once
	uninstall func()
}

type appAccessActionPoint struct {
	reached   chan operatorcontroller.IntegrationAppAccessActionEvent
	release   chan struct{}
	triggered atomic.Bool
	closeOnce sync.Once
	uninstall func()
}

type deletionActionPoint struct {
	reached   chan operatorcontroller.IntegrationDeletionActionEvent
	release   chan struct{}
	triggered atomic.Bool
	closeOnce sync.Once
	uninstall func()
}

func newDeletionActionPoint(
	t *testing.T,
	namespace string,
	name string,
	stage valkeyv1alpha1.DeletionStage,
) *deletionActionPoint {
	t.Helper()
	point := &deletionActionPoint{
		reached: make(chan operatorcontroller.IntegrationDeletionActionEvent, 1),
		release: make(chan struct{}),
	}
	point.uninstall = operatorcontroller.InstallIntegrationDeletionActionControl(func(
		ctx context.Context,
		event operatorcontroller.IntegrationDeletionActionEvent,
	) error {
		if event.Namespace != namespace || event.Name != name || stage != "" && event.Stage != stage ||
			!point.triggered.CompareAndSwap(false, true) {
			return nil
		}
		point.reached <- event
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

func (p *deletionActionPoint) waitFor(
	t *testing.T,
	timeout time.Duration,
) operatorcontroller.IntegrationDeletionActionEvent {
	t.Helper()
	select {
	case event := <-p.reached:
		return event
	case <-time.After(timeout):
		t.Fatal("управляемая граница удаления не достигнута")
		return operatorcontroller.IntegrationDeletionActionEvent{}
	}
}

func (p *deletionActionPoint) close() {
	p.closeOnce.Do(func() {
		p.uninstall()
		close(p.release)
	})
}

func newAppAccessActionPoint(
	t *testing.T,
	match func(operatorcontroller.IntegrationAppAccessActionEvent) bool,
) *appAccessActionPoint {
	t.Helper()
	point := &appAccessActionPoint{
		reached: make(chan operatorcontroller.IntegrationAppAccessActionEvent, 1),
		release: make(chan struct{}),
	}
	point.uninstall = operatorcontroller.InstallIntegrationAppAccessActionControl(func(
		ctx context.Context,
		event operatorcontroller.IntegrationAppAccessActionEvent,
	) error {
		if !match(event) || !point.triggered.CompareAndSwap(false, true) {
			return nil
		}
		point.reached <- event
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

func (p *appAccessActionPoint) waitFor(
	t *testing.T,
	timeout time.Duration,
) operatorcontroller.IntegrationAppAccessActionEvent {
	t.Helper()
	event, reached := p.wait(timeout)
	if reached {
		return event
	}
	t.Fatal("управляемая граница клиентского допуска не достигнута")
	return operatorcontroller.IntegrationAppAccessActionEvent{}
}

func (p *appAccessActionPoint) wait(
	timeout time.Duration,
) (operatorcontroller.IntegrationAppAccessActionEvent, bool) {
	select {
	case event := <-p.reached:
		return event, true
	case <-time.After(timeout):
		return operatorcontroller.IntegrationAppAccessActionEvent{}, false
	}
}

func (p *appAccessActionPoint) close() {
	p.closeOnce.Do(func() {
		p.uninstall()
		close(p.release)
	})
}

func newCredentialRotationActionPoint(
	t *testing.T,
	match func(operatorcontroller.IntegrationCredentialRotationActionEvent) bool,
) *credentialRotationActionPoint {
	t.Helper()
	point := &credentialRotationActionPoint{
		reached: make(chan operatorcontroller.IntegrationCredentialRotationActionEvent, 1),
		release: make(chan struct{}),
	}
	point.uninstall = operatorcontroller.InstallIntegrationCredentialRotationActionControl(func(
		ctx context.Context,
		event operatorcontroller.IntegrationCredentialRotationActionEvent,
	) error {
		if !match(event) || !point.triggered.CompareAndSwap(false, true) {
			return nil
		}
		point.reached <- event
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

func (p *credentialRotationActionPoint) waitFor(
	t *testing.T,
	timeout time.Duration,
) operatorcontroller.IntegrationCredentialRotationActionEvent {
	t.Helper()
	select {
	case event := <-p.reached:
		return event
	case <-time.After(timeout):
		t.Fatal("управляемая граница ротации не достигнута")
		return operatorcontroller.IntegrationCredentialRotationActionEvent{}
	}
}

func (p *credentialRotationActionPoint) close() {
	p.closeOnce.Do(func() {
		p.uninstall()
		close(p.release)
	})
}

func assertCredentialRotationStage(
	t *testing.T,
	h *harness,
	instance *testInstance,
	stage valkeyv1alpha1.CredentialRotationStage,
	version int64,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()
	current := h.getInstance(t, instance)
	if current.Status.CredentialRotation == nil ||
		current.Status.CredentialRotation.TargetVersion != version ||
		current.Status.CredentialRotation.Stage != stage {
		t.Fatalf("ротация версии %d не сохранила стадию %s: %+v", version, stage, current.Status.CredentialRotation)
	}
	return current
}

func assertCredentialRotationUnconfirmed(
	t *testing.T,
	h *harness,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
	version int64,
) {
	t.Helper()
	current := h.getInstance(t, instance)
	if current.Status.CredentialRotation == nil {
		t.Fatal("ротация завершилась до проверки подтверждения процесса")
	}
	expected := processIdentityForTest(process)
	for _, confirmation := range current.Status.CredentialRotation.Confirmations {
		if confirmation.Version == version && confirmation.Process == expected {
			t.Fatalf("процесс %s подтверждён раньше управляемой границы", process.PodUID)
		}
	}
}

func assertCredentialRotationConfirmed(
	t *testing.T,
	h *harness,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
	version int64,
) {
	t.Helper()
	current := h.getInstance(t, instance)
	if current.Status.CredentialRotation == nil {
		t.Fatal("ротация завершилась до проверки подтверждения процесса")
	}
	expected := processIdentityForTest(process)
	for _, confirmation := range current.Status.CredentialRotation.Confirmations {
		if confirmation.Version == version && confirmation.Process == expected {
			return
		}
	}
	t.Fatalf("процесс %s не получил точное подтверждение версии %d", process.PodUID, version)
}

func credentialRotationConfirmed(
	rotation *valkeyv1alpha1.CredentialRotationStatus,
	process valkeyv1alpha1.NodeStatus,
	version int64,
) bool {
	if rotation == nil {
		return false
	}
	expected := processIdentityForTest(process)
	for _, confirmation := range rotation.Confirmations {
		if confirmation.Version == version && confirmation.Process == expected {
			return true
		}
	}
	return false
}

func Test_PW01RZ01_RotatePasswordAndResizeSingle_PreservesDataAndCredentials(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createSingle(t, "mutate", valkeyv1alpha1.WhitelistSpec{})
	status := h.waitRunning(t, instance)
	servicePasswords := h.servicePasswords(t, instance)
	beforeRotation := processIdentities(status.Status.Nodes)
	beforeACL := readRealAppACLs(t, h, instance, status.Status.Nodes)

	regular := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	pubsub := openPersistentConnection(t, h.publicAddr, instance, h.caFile, true, func() {})
	blocking := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, regular, "rotation-key", "preserved")
	writeRESP(t, blocking, "BLPOP", "never-pushed", "0")

	oldPassword := instance.password
	newPassword := "rotated-" + mustUUIDv7(t)
	h.rotatePassword(t, instance, newPassword, 2)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.CredentialRotation == nil && current.Status.AppliedPasswordVersion == 2 &&
			current.Status.ObservedGeneration == 2 && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "PW-01 завершения ротации single")
	assertConnectionClosed(t, regular, false)
	assertConnectionClosed(t, pubsub, false)
	assertConnectionClosed(t, blocking, true)
	closeConnections([]*persistentConnection{regular, pubsub, blocking})
	instance.password = newPassword
	assertAuthenticationRejected(t, h, instance, oldPassword)
	assertProcessIdentities(t, beforeRotation, status.Status.Nodes)
	assertRealAppACLsChangedOnlyPassword(t, h, instance, status.Status.Nodes, beforeACL, newPassword, true)
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("PW-01 изменила служебные пароли")
	}
	assertPasswordsOnAllProcesses(t, h, instance, oldPassword, newPassword)

	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "rotation-key"); value != "preserved" {
		t.Fatalf("PW-01 потеряла ключ: %q", value)
	}

	resizeSingleAndAssertEmpty(t, h, instance, connection, 2, 2, 3)
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	resizeSingleAndAssertEmpty(t, h, instance, connection, 1, 2, 4)
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	resizeSingleAndAssertEmpty(t, h, instance, connection, 1, 1, 5)

	h.deleteInstance(t, instance)
}

func Test_PW08_RotatePassword_WithFencedProcess_KeepsApplicationAccessDisabledUntilRecovery(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "pwfenced")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	identities := processIdentities(status.Status.Nodes)
	servicePasswords := h.servicePasswords(t, instance)
	var target valkeyv1alpha1.NodeStatus
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica {
			target = process
			break
		}
	}
	if target.PodUID == "" {
		t.Fatalf("PW-08 не нашла реплику: %+v", status.Status.Nodes)
	}
	targetPod := h.getPodOrdinal(t, instance, target.Ordinal)
	beforeACLs := readRealAppACLs(t, h, instance, status.Status.Nodes)
	before := beforeACLs[target.Ordinal]

	h.stopOperator(t)
	fenceRealProcessApp(t, targetPod.Status.PodIP, servicePasswords[0], instance.password)
	fenced := readRealAppACL(t, h, instance, target)
	assertRealAppACLState(t, fenced, instance.password, false)
	assertRealAppACLFieldsEqual(t, before, fenced, "flags", "passwords")

	newPassword := "fenced-" + mustUUIDv7(t)
	point := newProcessActionPoint(t, "password-rotation-applied", target.PodUID, "")
	h.rotatePassword(t, instance, newPassword, 2)
	h.startOperator(t)
	point.waitFor(t, 2*time.Minute)
	duringRotation := readRealAppACL(t, h, instance, target)
	assertRealAppACLState(t, duringRotation, newPassword, false)
	assertRealAppACLFieldsEqual(t, fenced, duringRotation, "passwords")
	point.close()

	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.CredentialRotation == nil && current.Status.AppliedPasswordVersion == 2 &&
			current.Status.ObservedGeneration == 2 && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "PW-08 завершения ротации ограждённого процесса")
	assertProcessIdentities(t, identities, status.Status.Nodes)
	assertRealAppACLsChangedOnlyPassword(t, h, instance, status.Status.Nodes, beforeACLs, newPassword, true)
	finalTarget := readRealAppACL(t, h, instance, target)
	assertRealAppACLState(t, finalTarget, newPassword, true)
	assertRealAppACLFieldsEqual(t, before, finalTarget, "passwords")
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("PW-08 изменила служебные пароли")
	}
	assertPasswordsOnAllProcesses(t, h, instance, instance.password, newPassword)
	instance.password = newPassword

	h.deleteInstance(t, instance)
}

func Test_PW03_ResumePasswordRotation_AfterRestartsAndUnknownResults_CompletesEveryStage(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "pwrestart")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	identities := processIdentities(status.Status.Nodes)
	servicePasswords := h.servicePasswords(t, instance)
	beforeACLs := readRealAppACLs(t, h, instance, status.Status.Nodes)
	oldPassword := instance.password
	newPassword := "restart-" + mustUUIDv7(t)
	var target valkeyv1alpha1.NodeStatus
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica {
			target = process
			break
		}
	}
	if target.PodUID == "" {
		t.Fatalf("PW-03 не нашла реплику: %+v", status.Status.Nodes)
	}
	targetPod := h.getPodOrdinal(t, instance, target.Ordinal)
	targetAddress := net.JoinHostPort(targetPod.Status.PodIP, "6379")
	oldConnection := openDirectAppConnection(t, targetAddress, oldPassword, false)
	defer closeConnections([]*persistentConnection{oldConnection})

	preparing := newCredentialRotationActionPoint(
		t,
		func(event operatorcontroller.IntegrationCredentialRotationActionEvent) bool {
			return event.Name == "stage-saved" && event.TargetVersion == 2 &&
				event.Stage == valkeyv1alpha1.CredentialRotationStagePreparing
		},
	)
	h.rotatePassword(t, instance, newPassword, 2)
	preparing.waitFor(t, time.Minute)
	assertCredentialRotationStage(t, h, instance, valkeyv1alpha1.CredentialRotationStagePreparing, 2)
	h.stopOperator(t)
	preparing.close()

	replicasStage := newCredentialRotationActionPoint(
		t,
		func(event operatorcontroller.IntegrationCredentialRotationActionEvent) bool {
			return event.Name == "stage-saved" && event.TargetVersion == 2 &&
				event.Stage == valkeyv1alpha1.CredentialRotationStageUpdatingReplicas
		},
	)
	h.startOperator(t)
	replicasStage.waitFor(t, time.Minute)
	assertCredentialRotationStage(t, h, instance, valkeyv1alpha1.CredentialRotationStageUpdatingReplicas, 2)
	h.stopOperator(t)
	replicasStage.close()

	afterACL := newValkeyCommandPoint(
		t,
		targetAddress,
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandAfter,
		false,
	)
	h.startOperator(t)
	afterACL.waitFor(t, time.Minute)
	afterACLState := readRealAppACL(t, h, instance, target)
	assertRealAppACLState(t, afterACLState, newPassword, true)
	assertRealAppACLFieldsEqual(t, beforeACLs[target.Ordinal], afterACLState, "passwords")
	pingConnections(t, []*persistentConnection{oldConnection})
	assertCredentialRotationUnconfirmed(t, h, instance, target, 2)
	h.stopOperator(t)
	afterACL.close()

	afterKill := newValkeyCommandPoint(
		t,
		targetAddress,
		"CLIENT KILL app",
		operatorvalkey.IntegrationCommandAfter,
		false,
	)
	h.startOperator(t)
	afterKill.waitFor(t, time.Minute)
	assertConnectionClosed(t, oldConnection, false)
	closeConnections([]*persistentConnection{oldConnection})
	assertCredentialRotationUnconfirmed(t, h, instance, target, 2)
	h.stopOperator(t)
	afterKill.close()

	afterHashVerification := newValkeyCommandPointAtMatch(
		t,
		targetAddress,
		"ACL GETUSER",
		operatorvalkey.IntegrationCommandAfter,
		false,
		2,
	)
	h.startOperator(t)
	afterHashVerification.waitFor(t, time.Minute)
	verifiedACL := readRealAppACL(t, h, instance, target)
	assertRealAppACLState(t, verifiedACL, newPassword, true)
	assertCredentialRotationUnconfirmed(t, h, instance, target, 2)
	h.stopOperator(t)
	afterHashVerification.close()

	afterConfirmation := newCredentialRotationActionPoint(
		t,
		func(event operatorcontroller.IntegrationCredentialRotationActionEvent) bool {
			return event.Name == "process-confirmed" && event.TargetVersion == 2 && event.PodUID == target.PodUID
		},
	)
	h.startOperator(t)
	afterConfirmation.waitFor(t, time.Minute)
	assertCredentialRotationConfirmed(t, h, instance, target, 2)
	h.stopOperator(t)
	afterConfirmation.close()

	primaryStage := newCredentialRotationActionPoint(
		t,
		func(event operatorcontroller.IntegrationCredentialRotationActionEvent) bool {
			return event.Name == "stage-saved" && event.TargetVersion == 2 &&
				event.Stage == valkeyv1alpha1.CredentialRotationStageUpdatingPrimary
		},
	)
	h.startOperator(t)
	primaryStage.waitFor(t, time.Minute)
	assertCredentialRotationStage(t, h, instance, valkeyv1alpha1.CredentialRotationStageUpdatingPrimary, 2)
	h.stopOperator(t)
	primaryStage.close()

	cleaningStage := newCredentialRotationActionPoint(
		t,
		func(event operatorcontroller.IntegrationCredentialRotationActionEvent) bool {
			return event.Name == "stage-saved" && event.TargetVersion == 2 &&
				event.Stage == valkeyv1alpha1.CredentialRotationStageCleaningSecret
		},
	)
	h.startOperator(t)
	cleaningStage.waitFor(t, time.Minute)
	assertCredentialRotationStage(t, h, instance, valkeyv1alpha1.CredentialRotationStageCleaningSecret, 2)
	h.stopOperator(t)
	cleaningStage.close()

	afterSecretCleanup := newCredentialRotationActionPoint(
		t,
		func(event operatorcontroller.IntegrationCredentialRotationActionEvent) bool {
			return event.Name == "secret-cleaned" && event.TargetVersion == 2
		},
	)
	h.startOperator(t)
	afterSecretCleanup.waitFor(t, time.Minute)
	status = assertCredentialRotationStage(
		t,
		h,
		instance,
		valkeyv1alpha1.CredentialRotationStageCleaningSecret,
		2,
	)
	if status.Status.AppliedPasswordVersion != 1 {
		t.Fatalf("PW-03 подтвердила версию до записи финального status: %+v", status.Status)
	}
	secret := assertSecretACLForPassword(t, h, instance, newPassword, 2, servicePasswords)
	if _, found := secret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"1"]; found {
		t.Fatal("PW-03 сохранила предыдущий хеш после очистки Secret")
	}
	h.stopOperator(t)
	afterSecretCleanup.close()

	h.startOperator(t)
	status = h.waitForPasswordVersion(t, instance, 2)
	assertProcessIdentities(t, identities, status.Status.Nodes)
	assertRealAppACLsChangedOnlyPassword(t, h, instance, status.Status.Nodes, beforeACLs, newPassword, true)
	assertPasswordsOnAllProcesses(t, h, instance, oldPassword, newPassword)
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("PW-03 изменила служебные пароли")
	}
	instance.password = newPassword

	h.deleteInstance(t, instance)
}

func Test_PW05_RotatePassword_WhenReplicaFailsBeforeOrAfterConfirmation_RecoversWithoutDataLoss(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "pwreplica")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	servicePasswords := h.servicePasswords(t, instance)
	primary := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, primary, "pw05-key", "preserved")
	closeConnections([]*persistentConnection{primary})
	assertKeyOnAllReplicas(t, h, instance, status, "pw05-key", "preserved")

	var target valkeyv1alpha1.NodeStatus
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica {
			target = process
			break
		}
	}
	if target.PodUID == "" {
		t.Fatalf("PW-05 не нашла реплику: %+v", status.Status.Nodes)
	}
	targetPod := h.getPodOrdinal(t, instance, target.Ordinal)
	targetAddress := net.JoinHostPort(targetPod.Status.PodIP, "6379")
	passwordTwo := "before-confirmation-" + mustUUIDv7(t)
	beforeConfirmation := newValkeyCommandPoint(
		t,
		targetAddress,
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandBefore,
		false,
	)
	h.rotatePassword(t, instance, passwordTwo, 2)
	beforeConfirmation.waitFor(t, time.Minute)
	assertCredentialRotationUnconfirmed(t, h, instance, target, 2)
	h.sigkillValkeyProcess(t, instance, target)
	beforeConfirmation.close()

	replacementPod := h.waitForReplacementOrdinal(t, instance, target.Ordinal, types.UID(target.PodUID))
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		process, found := nodeStatusAtOrdinal(current.Status.Nodes, target.Ordinal)
		return found && process.PodUID == string(replacementPod.UID) && process.AppEnabled &&
			process.AppPasswordVersion == 2 && current.Status.CredentialRotation == nil &&
			current.Status.AppliedPasswordVersion == 2 && current.Status.ObservedGeneration == 2 &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "PW-05 замены реплики, убитой до подтверждения")
	firstReplacement := processAtOrdinal(t, status, target.Ordinal)
	assertRealProcessRoleACL(t, h, instance, firstReplacement, "slave", passwordTwo, true)
	assertPasswordsOnAllProcesses(t, h, instance, instance.password, passwordTwo)
	instance.password = passwordTwo
	primary = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, primary, "pw05-key"); value != "preserved" {
		closeConnections([]*persistentConnection{primary})
		t.Fatalf("PW-05 потеряла ключ после отказа до подтверждения: %q", value)
	}
	closeConnections([]*persistentConnection{primary})

	passwordThree := "after-confirmation-" + mustUUIDv7(t)
	afterConfirmation := newCredentialRotationActionPoint(
		t,
		func(event operatorcontroller.IntegrationCredentialRotationActionEvent) bool {
			return event.Name == "process-confirmed" && event.TargetVersion == 3 &&
				event.PodUID == firstReplacement.PodUID
		},
	)
	h.rotatePassword(t, instance, passwordThree, 3)
	afterConfirmation.waitFor(t, time.Minute)
	assertCredentialRotationConfirmed(t, h, instance, firstReplacement, 3)
	assertRealProcessRoleACL(t, h, instance, firstReplacement, "slave", passwordThree, true)
	h.sigkillValkeyProcess(t, instance, firstReplacement)
	afterConfirmation.close()

	secondReplacementPod := h.waitForReplacementOrdinal(
		t,
		instance,
		firstReplacement.Ordinal,
		types.UID(firstReplacement.PodUID),
	)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		process, found := nodeStatusAtOrdinal(current.Status.Nodes, firstReplacement.Ordinal)
		return found && process.PodUID == string(secondReplacementPod.UID) && process.AppEnabled &&
			process.AppPasswordVersion == 3 && current.Status.CredentialRotation == nil &&
			current.Status.AppliedPasswordVersion == 3 && current.Status.ObservedGeneration == 3 &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "PW-05 замены реплики, убитой после подтверждения")
	secondReplacement := processAtOrdinal(t, status, firstReplacement.Ordinal)
	if secondReplacement.PodUID == firstReplacement.PodUID ||
		secondReplacement.ContainerID == firstReplacement.ContainerID ||
		secondReplacement.RunID == firstReplacement.RunID {
		t.Fatalf("PW-05 перенесла идентичность прежней реплики: %+v", secondReplacement)
	}
	assertRealProcessRoleACL(t, h, instance, secondReplacement, "slave", passwordThree, true)
	assertPasswordsOnAllProcesses(t, h, instance, passwordTwo, passwordThree)
	instance.password = passwordThree
	primary = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, primary, "pw05-key"); value != "preserved" {
		closeConnections([]*persistentConnection{primary})
		t.Fatalf("PW-05 потеряла ключ после отказа после подтверждения: %q", value)
	}
	closeConnections([]*persistentConnection{primary})
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("PW-05 изменила служебные пароли")
	}

	h.deleteInstance(t, instance)
}

func Test_PW06_RotatePassword_WithLiveIsolatedReplica_WaitsForReplicaReturn(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "pwisolated")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	identities := processIdentities(status.Status.Nodes)
	servicePasswords := h.servicePasswords(t, instance)
	var target valkeyv1alpha1.NodeStatus
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica && process.Ordinal > target.Ordinal {
			target = process
		}
	}
	if target.PodUID == "" {
		t.Fatalf("PW-06 не нашла реплику: %+v", status.Status.Nodes)
	}
	targetPod := h.getPodOrdinal(t, instance, target.Ordinal)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	setValue(t, connection, "pw06-key", "preserved")

	fault := h.addNodeNetworkFault(
		t,
		"pw06-operator-replica",
		target.NodeName,
		"FORWARD",
		podRouteSourceCIDR(t, h.k8s),
		hostCIDR(t, targetPod.Status.PodIP),
		6379,
	)
	t.Cleanup(func() {
		if err := fault.restore(); err != nil {
			t.Errorf("снять разрыв PW-06: %v", err)
		}
	})
	fault.assertPresent(t)
	oldPassword := instance.password
	newPassword := "isolated-" + mustUUIDv7(t)
	h.rotatePassword(t, instance, newPassword, 2)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		process, found := nodeStatusAtOrdinal(current.Status.Nodes, target.Ordinal)
		if !found || process.Observation == nil || current.Status.CredentialRotation == nil ||
			current.Status.CredentialRotation.Stage != valkeyv1alpha1.CredentialRotationStageUpdatingReplicas ||
			current.Status.AppliedPasswordVersion != 1 || current.Status.ObservedGeneration != 1 {
			return false
		}
		for _, available := range current.Status.Nodes {
			if available.Ordinal != target.Ordinal &&
				(available.AppPasswordVersion != 2 ||
					!credentialRotationConfirmed(current.Status.CredentialRotation, available, 2)) {
				return false
			}
		}
		return true
	}, "PW-06 ожидания живой изолированной реплики")
	assertProcessIdentities(t, identities, status.Status.Nodes)
	assertRealProcessRoleACL(t, h, instance, target, "slave", oldPassword, true)
	for _, available := range status.Status.Nodes {
		if available.Ordinal == target.Ordinal {
			continue
		}
		role := "slave"
		if available.Role == valkeyv1alpha1.NodeRolePrimary {
			role = "master"
		}
		assertRealProcessRoleACL(t, h, instance, available, role, newPassword, true)
	}
	assertConnectionClosed(t, connection, false)
	rotatedInstance := *instance
	rotatedInstance.password = newPassword
	newConnection := openPersistentConnection(t, h.publicAddr, &rotatedInstance, h.caFile, false, func() {})
	if value := getValue(t, newConnection, "pw06-key"); value != "preserved" {
		closeConnections([]*persistentConnection{newConnection})
		t.Fatalf("PW-06 потеряла ключ до возврата реплики: %q", value)
	}
	closeConnections([]*persistentConnection{newConnection})
	if node := h.getNode(t, target.NodeName); string(node.UID) != target.NodeUID || !nodeReady(node.Status.Conditions) {
		t.Fatalf("PW-06 потеряла живую Ready Node реплики: %+v", node.Status)
	}
	if err := fault.restore(); err != nil {
		t.Fatalf("снять разрыв PW-06: %v", err)
	}
	fault.assertAbsent(t)

	status = h.waitForPasswordVersion(t, instance, 2)
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	assertProcessIdentities(t, identities, status.Status.Nodes)
	assertPasswordsOnAllProcesses(t, h, instance, oldPassword, newPassword)
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("PW-06 изменила служебные пароли")
	}
	instance.password = newPassword
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "pw06-key"); value != "preserved" {
		t.Fatalf("PW-06 потеряла ключ после возврата реплики: %q", value)
	}

	h.deleteInstance(t, instance)
}

func Test_PW07_RotatePassword_WhenAgentIsDestroyedWithoutReplacementCapacity_CompletesInDegradedState(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })
	h.requireNodeCount(t, 3)

	instance := h.createHA(t, "pwnode")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	servicePasswords := h.servicePasswords(t, instance)
	var target valkeyv1alpha1.NodeStatus
	for _, process := range status.Status.Nodes {
		if target.PodUID == "" && process.Role == valkeyv1alpha1.NodeRoleReplica && process.NodeName != "k3s-server" {
			target = process
		}
	}
	if target.PodUID == "" {
		t.Fatalf("PW-07 не нашла реплику на agent: %+v", status.Status.Nodes)
	}
	h.useSurvivingEnvoy(t, target.NodeName)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "pw07-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "pw07-key", "preserved")
	targetPod := h.getPodOrdinal(t, instance, target.Ordinal)
	targetAddress := net.JoinHostPort(targetPod.Status.PodIP, "6379")
	oldPassword := instance.password
	newPassword := "node-loss-" + mustUUIDv7(t)
	beforeTargetRotation := newValkeyCommandPoint(
		t,
		targetAddress,
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandBefore,
		false,
	)
	h.rotatePassword(t, instance, newPassword, 2)
	beforeTargetRotation.waitFor(t, time.Minute)
	assertCredentialRotationUnconfirmed(t, h, instance, target, 2)

	failedNode := h.getNode(t, target.NodeName)
	fault := h.newAgentFault(t, failedNode)
	t.Cleanup(func() { fault.restore(t, h) })
	fault.kill(t, h)
	closeConnections([]*persistentConnection{connection})
	h.deleteNode(t, failedNode)
	beforeTargetRotation.close()

	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.CredentialRotation == nil && current.Status.AppliedPasswordVersion == 2 &&
			current.Status.ObservedGeneration == 2 && current.Status.Phase == valkeyv1alpha1.InstancePhaseDegraded &&
			hasTerminationEvidence(current.Status.Nodes, processIdentityForTest(target), "node_deleted")
	}, "PW-07 завершения ротации после node_deleted без места для замены")
	h.waitForPendingPod(t, instance)
	secret := assertSecretACLForPassword(t, h, instance, newPassword, 2, servicePasswords)
	if _, found := secret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"1"]; found {
		t.Fatal("PW-07 сохранила предыдущий хеш после завершения")
	}
	liveProcesses := 0
	for _, process := range status.Status.Nodes {
		if process.Termination != nil || process.Observation != nil {
			continue
		}
		liveProcesses++
		expectedRole := "slave"
		if status.Status.PrimaryOrdinal != nil && process.Ordinal == *status.Status.PrimaryOrdinal {
			expectedRole = "master"
		}
		assertRealProcessRoleACL(t, h, instance, process, expectedRole, newPassword, true)
	}
	if liveProcesses != 2 {
		t.Fatalf(
			"PW-07 ожидала два работающих процесса без замены, получила %d: %+v",
			liveProcesses,
			status.Status.Nodes,
		)
	}

	instance.password = newPassword
	fault.restore(t, h)
	status = h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	assertPasswordsOnAllProcesses(t, h, instance, oldPassword, newPassword)
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "pw07-key"); value != "preserved" {
		closeConnections([]*persistentConnection{connection})
		t.Fatalf("PW-07 потеряла ключ после возврата ноды: %q", value)
	}
	closeConnections([]*persistentConnection{connection})
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("PW-07 изменила служебные пароли")
	}

	h.deleteInstance(t, instance)
}

func Test_PW04_RotatePassword_WhenPrimaryFails_UsesTargetPasswordDuringFailover(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "pwfailover")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	servicePasswords := h.servicePasswords(t, instance)
	source := primaryProcess(t, status)
	var firstReplica valkeyv1alpha1.NodeStatus
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica &&
			(firstReplica.PodUID == "" || process.Ordinal < firstReplica.Ordinal) {
			firstReplica = process
		}
	}
	if firstReplica.PodUID == "" {
		t.Fatalf("PW-04 не нашла первую реплику: %+v", status.Status.Nodes)
	}
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "pw04-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "pw04-key", "preserved")
	oldPassword := instance.password
	newPassword := "failover-" + mustUUIDv7(t)
	afterFirstReplica := newCredentialRotationActionPoint(
		t,
		func(event operatorcontroller.IntegrationCredentialRotationActionEvent) bool {
			return event.Name == "process-confirmed" && event.TargetVersion == 2 &&
				event.PodUID == firstReplica.PodUID
		},
	)
	h.rotatePassword(t, instance, newPassword, 2)
	afterFirstReplica.waitFor(t, time.Minute)
	current := assertCredentialRotationStage(
		t,
		h,
		instance,
		valkeyv1alpha1.CredentialRotationStageUpdatingReplicas,
		2,
	)
	if len(current.Status.CredentialRotation.Confirmations) != 1 {
		t.Fatalf("PW-04 обновила больше одной реплики до отказа primary: %+v", current.Status.CredentialRotation)
	}
	assertCredentialRotationConfirmed(t, h, instance, firstReplica, 2)

	newPrimaryAdmission := newAppAccessActionPoint(
		t,
		func(event operatorcontroller.IntegrationAppAccessActionEvent) bool {
			return event.Enabled
		},
	)
	h.sigkillValkeyProcess(t, instance, source)
	closeConnections([]*persistentConnection{connection})
	afterFirstReplica.close()
	event, reached := newPrimaryAdmission.wait(2 * time.Minute)
	current = h.getInstance(t, instance)
	if !reached {
		t.Fatalf("PW-04 не достигла допуска нового основного процесса: %+v", current.Status)
	}
	if current.Status.Failover == nil || current.Status.PrimaryOrdinal == nil ||
		current.Status.PrimaryPodUID == source.PodUID {
		t.Fatalf("PW-04 допустила app вне аварийного нового primary: %+v", current.Status)
	}
	candidate := processAtOrdinal(t, current, *current.Status.PrimaryOrdinal)
	candidatePod := h.getPodOrdinal(t, instance, candidate.Ordinal)
	if event.Address != net.JoinHostPort(candidatePod.Status.PodIP, "6379") {
		t.Fatalf("PW-04 перехватила допуск не нового primary: event=%+v primary=%s", event, candidatePod.Status.PodIP)
	}
	assertRealProcessRoleACL(t, h, instance, candidate, "master", newPassword, true)
	assertProcessPasswordAcceptedAndRejected(t, h, instance, candidate, newPassword, oldPassword)
	newPrimaryAdmission.close()

	status = h.waitForPasswordVersion(t, instance, 2)
	if status.Status.PrimaryPodUID == source.PodUID || status.Status.Failover != nil {
		t.Fatalf("PW-04 не завершила failover: %+v", status.Status)
	}
	assertPasswordsOnAllProcesses(t, h, instance, oldPassword, newPassword)
	instance.password = newPassword
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "pw04-key"); value != "preserved" {
		closeConnections([]*persistentConnection{connection})
		t.Fatalf("PW-04 потеряла ключ при failover: %q", value)
	}
	closeConnections([]*persistentConnection{connection})
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("PW-04 изменила служебные пароли")
	}

	h.deleteInstance(t, instance)
}

func Test_PW09_ObserveCompletedPasswordRotation_WithoutFurtherChanges_KeepsConnectionsAndUsesCurrentPasswordForReplacement(
	t *testing.T,
) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "pwstable")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	identities := processIdentities(status.Status.Nodes)
	servicePasswords := h.servicePasswords(t, instance)
	oldPassword := instance.password
	newPassword := "stable-" + mustUUIDv7(t)
	h.rotatePassword(t, instance, newPassword, 2)
	status = h.waitForPasswordVersion(t, instance, 2)
	assertProcessIdentities(t, identities, status.Status.Nodes)
	instance.password = newPassword

	publicConnection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	directConnections := make([]*persistentConnection, 0, len(status.Status.Nodes))
	for _, process := range status.Status.Nodes {
		pod := h.getPodOrdinal(t, instance, process.Ordinal)
		directConnections = append(
			directConnections,
			openDirectAppConnection(t, net.JoinHostPort(pod.Status.PodIP, "6379"), newPassword, false),
		)
	}
	lastObservedAt := status.Status.ObservedAt.DeepCopy()
	if lastObservedAt == nil {
		t.Fatal("PW-09 завершённая ротация не содержит observedAt")
	}
	heartbeats := 0
	status = h.waitForWithin(t, instance, 20*time.Second, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		if current.Status.ObservedAt == nil || !current.Status.ObservedAt.After(lastObservedAt.Time) {
			return false
		}
		lastObservedAt = current.Status.ObservedAt.DeepCopy()
		pingConnections(t, append([]*persistentConnection{publicConnection}, directConnections...))
		heartbeats++
		return heartbeats == 4
	}, "PW-09 четырёх новых наблюдений без повторного разрыва соединений")
	assertProcessIdentities(t, identities, status.Status.Nodes)

	setValue(t, publicConnection, "pw09-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "pw09-key", "preserved")
	var target valkeyv1alpha1.NodeStatus
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica && process.Ordinal > target.Ordinal {
			target = process
		}
	}
	if target.PodUID == "" {
		t.Fatalf("PW-09 не нашла реплику для будущей замены: %+v", status.Status.Nodes)
	}
	closeConnections(directConnections)
	h.deletePod(t, instance, target.Ordinal)
	replacementPod := h.waitForReplacementOrdinal(t, instance, target.Ordinal, types.UID(target.PodUID))
	status = h.waitRunning(t, instance)
	replacement := processAtOrdinal(t, status, target.Ordinal)
	if replacement.PodUID != string(replacementPod.UID) || replacement.PodUID == target.PodUID ||
		replacement.ContainerID == target.ContainerID {
		t.Fatalf("PW-09 не получила новую идентичность процесса: before=%+v after=%+v", target, replacement)
	}
	assertRealProcessRoleACL(t, h, instance, replacement, "slave", newPassword, true)
	assertProcessPasswordAcceptedAndRejected(t, h, instance, replacement, newPassword, oldPassword)
	pingConnections(t, []*persistentConnection{publicConnection})
	if value := getValue(t, publicConnection, "pw09-key"); value != "preserved" {
		t.Fatalf("PW-09 потеряла контрольный ключ после замены реплики: %q", value)
	}
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("PW-09 изменила служебные пароли")
	}
	closeConnections([]*persistentConnection{publicConnection})

	h.deleteInstance(t, instance)
}

func Test_PW10_ApplyPasswordResizeAndDeletion_WhenRequestedTogether_OrdersAndPreemptsOperationsSafely(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "pworder")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	identities := processIdentities(status.Status.Nodes)
	servicePasswords := h.servicePasswords(t, instance)
	oldPassword := instance.password
	newPassword := "ordered-" + mustUUIDv7(t)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "pw10-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "pw10-key", "preserved")

	beforeTemplate := newRolloutActionPoint(t, valkeyv1alpha1.RolloutStageUpdatingTemplate, 3)
	h.deliverPasswordHash(t, instance, newPassword, 2)
	h.requestPasswordAndSize(t, instance, 2, 2, 2, 2)
	beforeTemplate.waitFor(t, 2*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.AcceptedConfiguration == nil ||
		status.Status.AcceptedConfiguration.DesiredGeneration != 2 ||
		status.Status.AcceptedConfiguration.PasswordVersion != 2 ||
		status.Status.AcceptedConfiguration.VCPU != 2 ||
		status.Status.AcceptedConfiguration.RAMGB != 2 ||
		status.Status.CredentialRotation != nil || status.Status.AppliedPasswordVersion != 2 ||
		status.Status.Rollout == nil ||
		status.Status.Rollout.Stage != valkeyv1alpha1.RolloutStageUpdatingTemplate ||
		status.Status.Rollout.DesiredGeneration != 2 || status.Status.Applied == nil ||
		status.Status.Applied.VCPU != 1 || status.Status.Applied.RAMGB != 1 ||
		status.Status.ObservedGeneration != 1 {
		t.Fatalf("PW-10 не разделила подтверждения пароля и размера: %+v", status.Status)
	}
	assertProcessIdentities(t, identities, status.Status.Nodes)
	assertPasswordsOnAllProcesses(t, h, instance, oldPassword, newPassword)
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	secret := assertSecretACLForPassword(t, h, instance, newPassword, 2, servicePasswords)
	if _, found := secret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"1"]; found {
		t.Fatal("PW-10 начала ресайз до очистки предыдущего хеша")
	}
	for _, process := range status.Status.Nodes {
		assertPodResourcesAndMaxmemory(t, h, instance, process, 1, 1)
	}

	beforeTemplate.close()
	instance.password = newPassword
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.CredentialRotation == nil && current.Status.Rollout == nil &&
			current.Status.AppliedPasswordVersion == 2 && current.Status.Applied != nil &&
			current.Status.Applied.VCPU == 2 && current.Status.Applied.RAMGB == 2 &&
			current.Status.ObservedGeneration == 2 &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "PW-10 завершения общего поколения пароля и размера")
	assertHAComposition(t, h, instance, status)
	assertPasswordsOnAllProcesses(t, h, instance, oldPassword, newPassword)
	for _, process := range status.Status.Nodes {
		assertPodResourcesAndMaxmemory(t, h, instance, process, 2, 2)
	}
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "pw10-key"); value != "preserved" {
		closeConnections([]*persistentConnection{connection})
		t.Fatalf("PW-10 потеряла ключ при последовательном применении: %q", value)
	}
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("PW-10 изменила служебные пароли")
	}

	verifyPW10DeletionPreemption(t, h, instance, status, connection)
}

func verifyPW10DeletionPreemption(
	t *testing.T,
	h *harness,
	instance *testInstance,
	status *valkeyv1alpha1.ValkeyInstance,
	connection *persistentConnection,
) {
	t.Helper()
	newPassword := "deleted-" + mustUUIDv7(t)
	var firstReplica valkeyv1alpha1.NodeStatus
	for _, process := range status.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica && firstReplica.PodUID == "" {
			firstReplica = process
		}
	}
	if firstReplica.PodUID == "" {
		t.Fatalf("PW-10 не нашла реплику перед удалением: %+v", status.Status.Nodes)
	}
	afterFirstReplica := newCredentialRotationActionPoint(
		t,
		func(event operatorcontroller.IntegrationCredentialRotationActionEvent) bool {
			return event.Name == "process-confirmed" && event.TargetVersion == 3 &&
				event.PodUID == firstReplica.PodUID
		},
	)
	h.rotatePassword(t, instance, newPassword, 3)
	afterFirstReplica.waitFor(t, time.Minute)
	status = assertCredentialRotationStage(
		t,
		h,
		instance,
		valkeyv1alpha1.CredentialRotationStageUpdatingReplicas,
		3,
	)
	knownPods := make(map[types.UID]struct{}, len(status.Status.Nodes))
	for _, process := range status.Status.Nodes {
		knownPods[types.UID(process.PodUID)] = struct{}{}
	}
	var enabledAfterDeletion atomic.Bool
	uninstall := operatorcontroller.InstallIntegrationAppAccessActionControl(func(
		_ context.Context,
		event operatorcontroller.IntegrationAppAccessActionEvent,
	) error {
		if event.Enabled {
			enabledAfterDeletion.Store(true)
		}
		return nil
	})
	t.Cleanup(uninstall)

	h.requestDeletion(t, instance)
	status = h.getInstance(t, instance)
	if status.DeletionTimestamp.IsZero() || status.Status.CredentialRotation == nil ||
		status.Status.AppliedPasswordVersion != 2 || status.Status.ObservedGeneration != 2 {
		t.Fatalf("PW-10 завершила ротацию перед принятием удаления: %+v", status)
	}
	afterFirstReplica.close()
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Deletion != nil
	}, "PW-10 начала удаления поверх ротации")
	if status.Status.CredentialRotation == nil || status.Status.AppliedPasswordVersion != 2 {
		t.Fatalf("PW-10 ожидала завершения ротации перед удалением: %+v", status.Status)
	}
	secret := &corev1.Secret{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{
		Name: valkeyv1alpha1.AuthSecretName(instance.slug), Namespace: instance.namespace,
	}, secret); err != nil {
		t.Fatalf("PW-10 не сохранила Secret при начале удаления: %v", err)
	}

	err := wait.PollUntilContextTimeout(
		t.Context(), 100*time.Millisecond, 5*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			current := &valkeyv1alpha1.ValkeyInstance{}
			err := h.k8s.Get(
				ctx,
				client.ObjectKey{Name: instance.slug, Namespace: instance.namespace},
				current,
			)
			if err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
			pods := &corev1.PodList{}
			if listErr := h.k8s.List(
				ctx,
				pods,
				client.InNamespace(instance.namespace),
				client.MatchingLabels{valkeyv1alpha1.InstanceLabelKey: instance.slug},
			); listErr != nil {
				return false, listErr
			}
			for _, pod := range pods.Items {
				if _, found := knownPods[pod.UID]; !found {
					return false, fmt.Errorf("PW-10 создала Pod %s с UID %s после запроса удаления", pod.Name, pod.UID)
				}
			}
			if apierrors.IsNotFound(err) {
				return len(pods.Items) == 0, nil
			}
			return false, nil
		},
	)
	if err != nil {
		t.Fatalf("PW-10 дождаться удаления без возрождения workload: %v", err)
	}
	if enabledAfterDeletion.Load() {
		t.Fatal("PW-10 повторно включила app после запроса удаления")
	}
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	h.waitDeleted(t, instance)
}

func Test_DL01_DeleteInstance_WhenFailoverCandidateOutcomeIsUnknown_WaitsForTerminationEvidence(t *testing.T) {
	testDL01DeletionDuringOperation(t, "unknown-candidate")
}

func Test_DL01_DeleteInstance_WhenHAShrinkIsInProgress_WaitsForTerminationEvidence(t *testing.T) {
	testDL01DeletionDuringOperation(t, "shrink")
}

func Test_DL01_DeleteInstance_WhenPasswordRotationIsInProgress_WaitsForTerminationEvidence(t *testing.T) {
	testDL01DeletionDuringOperation(t, "password-rotation")
}

func testDL01DeletionDuringOperation(t *testing.T, scenario string) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	if scenario == "unknown-candidate" {
		instance := h.createHA(t, "dlunknown")
		status := h.waitRunning(t, instance)
		assertHAComposition(t, h, instance, status)
		source := primaryProcess(t, status)
		promotionPoint := newValkeyCommandPoint(
			t,
			"",
			"REPLICAOF NO ONE",
			operatorvalkey.IntegrationCommandAfter,
			true,
		)
		h.sigkillValkeyProcess(t, instance, source)
		promotionPoint.waitFor(t, 45*time.Second)
		status = h.getInstance(t, instance)
		if status.Status.Failover == nil || status.Status.Failover.Candidate == nil ||
			!status.Status.Failover.CandidateMayBePrimary {
			t.Fatalf("DL-01 не сохранила потенциальный primary: %+v", status.Status.Failover)
		}
		candidate := processForIdentity(t, status.Status.Nodes, *status.Status.Failover.Candidate)
		assertRealProcessRoleACL(t, h, instance, candidate, "master", instance.password, false)
		known := knownProcessIdentities(status)
		finalizerPoint := newDeletionActionPoint(t, instance.namespace, "before-finalizer-removal", "")
		h.requestDeletion(t, instance)
		promotionPoint.close()
		assertDL01FinalizerBoundary(t, h, instance, finalizerPoint, known)
	}

	if scenario == "shrink" {
		instance := h.createHAWithSize(t, "dlshrink", 2, 2)
		status := h.waitRunning(t, instance)
		assertHAComposition(t, h, instance, status)
		beforeZero := newRolloutActionPoint(t, valkeyv1alpha1.RolloutStageStopping, 0)
		h.resize(t, instance, 1, 1, 2)
		beforeZero.waitFor(t, 3*time.Minute)
		status = h.getInstance(t, instance)
		if status.Status.Rollout == nil || status.Status.Rollout.Stage != valkeyv1alpha1.RolloutStageStopping ||
			!status.Status.Rollout.AccessClosed {
			t.Fatalf("DL-01 не достигла полного останова shrink: %+v", status.Status.Rollout)
		}
		known := knownProcessIdentities(status)
		finalizerPoint := newDeletionActionPoint(t, instance.namespace, "before-finalizer-removal", "")
		h.requestDeletion(t, instance)
		beforeZero.close()
		assertDL01FinalizerBoundary(t, h, instance, finalizerPoint, known)
	}

	if scenario == "password-rotation" {
		instance := h.createHA(t, "dlpassword")
		status := h.waitRunning(t, instance)
		assertHAComposition(t, h, instance, status)
		var replica valkeyv1alpha1.NodeStatus
		for _, process := range status.Status.Nodes {
			if process.Role == valkeyv1alpha1.NodeRoleReplica {
				replica = process
				break
			}
		}
		if replica.PodUID == "" {
			t.Fatalf("DL-01 не нашла реплику перед ротацией: %+v", status.Status.Nodes)
		}
		confirmationPoint := newCredentialRotationActionPoint(
			t,
			func(event operatorcontroller.IntegrationCredentialRotationActionEvent) bool {
				return event.Name == "process-confirmed" && event.TargetVersion == 2 &&
					event.PodUID == replica.PodUID
			},
		)
		h.rotatePassword(t, instance, "dl-"+mustUUIDv7(t), 2)
		confirmationPoint.waitFor(t, time.Minute)
		status = h.getInstance(t, instance)
		if status.Status.CredentialRotation == nil || status.Status.AppliedPasswordVersion != 1 {
			t.Fatalf("DL-01 не остановилась во время ротации: %+v", status.Status)
		}
		known := knownProcessIdentities(status)
		finalizerPoint := newDeletionActionPoint(t, instance.namespace, "before-finalizer-removal", "")
		h.requestDeletion(t, instance)
		confirmationPoint.close()
		assertDL01FinalizerBoundary(t, h, instance, finalizerPoint, known)
	}
}

func knownProcessIdentities(instance *valkeyv1alpha1.ValkeyInstance) []valkeyv1alpha1.ProcessIdentity {
	seen := make(map[valkeyv1alpha1.ProcessIdentity]struct{})
	result := make(
		[]valkeyv1alpha1.ProcessIdentity,
		0,
		len(instance.Status.Nodes)+len(instance.Status.PreviousProcesses),
	)
	for _, process := range append(slices.Clone(instance.Status.Nodes), instance.Status.PreviousProcesses...) {
		identity := processIdentityForTest(process)
		if _, found := seen[identity]; found {
			continue
		}
		seen[identity] = struct{}{}
		result = append(result, identity)
	}
	return result
}

func assertDL01FinalizerBoundary(
	t *testing.T,
	h *harness,
	instance *testInstance,
	point *deletionActionPoint,
	known []valkeyv1alpha1.ProcessIdentity,
) {
	t.Helper()
	assertDeletionFinalizerBoundary(t, h, instance, point, known)
	point.close()
	waitForValkeyInstanceDeletion(t, h, instance)
}

func assertDeletionFinalizerBoundary(
	t *testing.T,
	h *harness,
	instance *testInstance,
	point *deletionActionPoint,
	known []valkeyv1alpha1.ProcessIdentity,
) {
	t.Helper()
	event := point.waitFor(t, 3*time.Minute)
	if event.Stage != valkeyv1alpha1.DeletionStageVerifying {
		t.Fatalf("удаление дошло до finalizer на стадии %s", event.Stage)
	}
	current := h.getInstance(t, instance)
	if current.Status.Deletion == nil || current.Status.Deletion.Stage != valkeyv1alpha1.DeletionStageVerifying {
		t.Fatalf("удаление не сохранило стадию проверки: %+v", current.Status.Deletion)
	}
	processes := append(slices.Clone(current.Status.Nodes), current.Status.PreviousProcesses...)
	for _, identity := range known {
		found := slices.ContainsFunc(processes, func(process valkeyv1alpha1.NodeStatus) bool {
			return processIdentityForTest(process) == identity && process.Termination != nil &&
				process.Termination.Evidence != "" && !process.Termination.FinishedAt.IsZero()
		})
		if !found {
			t.Fatalf("удаление не сохранило доказательство остановки процесса %+v: %+v", identity, processes)
		}
	}
	for _, process := range processes {
		if process.Termination == nil {
			t.Fatalf("удаление снимает finalizer при неизвестном состоянии процесса: %+v", process)
		}
	}
	pods := &corev1.PodList{}
	if err := h.k8s.List(
		t.Context(),
		pods,
		client.InNamespace(instance.namespace),
		client.MatchingLabels{valkeyv1alpha1.InstanceLabelKey: instance.slug},
	); err != nil {
		t.Fatalf("прочитать Pod перед снятием finalizer: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("удаление снимает finalizer до исчезновения Pod: %+v", pods.Items)
	}
	secret := &corev1.Secret{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{
		Name: valkeyv1alpha1.AuthSecretName(instance.slug), Namespace: instance.namespace,
	}, secret); err != nil {
		t.Fatalf("удаление не сохранило Secret до доказательств остановки: %v", err)
	}
}

func Test_DL02_DeleteInstance_AfterAgentIsDestroyed_AcceptsNodeDeletionAsTerminationEvidence(t *testing.T) {
	testDL02DL03NodeFailureDuringDeletion(t, "agent-destroyed")
}

func Test_DL03_DeleteInstance_WithLiveIsolatedAgent_WaitsForProcessTermination(t *testing.T) {
	testDL02DL03NodeFailureDuringDeletion(t, "live-isolated-agent")
}

func testDL02DL03NodeFailureDuringDeletion(t *testing.T, scenario string) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })
	h.requireNodeCount(t, 3)

	if scenario == "agent-destroyed" {
		instance := h.createHA(t, "dlagent")
		status := h.waitRunning(t, instance)
		assertHAComposition(t, h, instance, status)
		target := deletionAgentProcess(t, status)
		node := h.getNode(t, target.NodeName)
		fault := h.newAgentFault(t, node)
		t.Cleanup(func() { fault.restore(t, h) })
		stagePoint := newDeletionActionPoint(
			t,
			instance.namespace,
			"stage-saved",
			valkeyv1alpha1.DeletionStageDisablingApp,
		)
		h.requestDeletion(t, instance)
		event := stagePoint.waitFor(t, time.Minute)
		if event.Stage != valkeyv1alpha1.DeletionStageDisablingApp {
			t.Fatalf("DL-02 остановилась на стадии %s", event.Stage)
		}
		status = h.getInstance(t, instance)
		known := knownProcessIdentities(status)
		finalizerPoint := newDeletionActionPoint(t, instance.namespace, "before-finalizer-removal", "")
		fault.kill(t, h)
		h.deleteNode(t, node)
		stagePoint.close()
		assertDL01FinalizerBoundary(t, h, instance, finalizerPoint, known)
		fault.restore(t, h)
	}

	if scenario == "live-isolated-agent" {
		instance := h.createHA(t, "dlisolated")
		status := h.waitRunning(t, instance)
		assertHAComposition(t, h, instance, status)
		target := deletionAgentProcess(t, status)
		pod := h.getPodOrdinal(t, instance, target.Ordinal)
		operatorPassword := h.servicePasswords(t, instance)[0]
		serverAddress := requiredEnv(t, "K3S_SERVER_IP")
		agentAddress := h.nodeInternalAddress(t, target.NodeName)
		h.assertAgentControlPlane(t, target.NodeName, serverAddress, true)
		fault := h.addNodeNetworkFault(
			t,
			"dl03-agent-control-plane",
			target.NodeName,
			"OUTPUT",
			hostCIDR(t, agentAddress),
			hostCIDR(t, serverAddress),
			6443,
		)
		t.Cleanup(func() {
			if err := fault.restore(); err != nil {
				t.Errorf("DL-03 снять разрыв agent/control-plane: %v", err)
			}
		})
		fault.assertPresent(t)
		h.assertAgentControlPlane(t, target.NodeName, serverAddress, false)
		node := h.waitSameNodeReadyState(t, target.NodeName, types.UID(target.NodeUID), false)
		known := knownProcessIdentities(status)
		finalizerPoint := newDeletionActionPoint(t, instance.namespace, "before-finalizer-removal", "")
		h.requestDeletion(t, instance)
		status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
			return current.Status.Deletion != nil &&
				current.Status.Deletion.Stage == valkeyv1alpha1.DeletionStageVerifying
		}, "DL-03 ожидания доказательства живой изолированной ноды")
		deletingPod := h.getPodOrdinal(t, instance, target.Ordinal)
		if deletingPod.UID != types.UID(target.PodUID) || deletingPod.DeletionTimestamp.IsZero() {
			t.Fatalf("DL-03 не сохранила удаляемый Pod живой ноды: uid=%s deletion=%s",
				deletingPod.UID, deletingPod.DeletionTimestamp)
		}
		expectedRole := "slave"
		if target.Role == valkeyv1alpha1.NodeRolePrimary {
			expectedRole = "master"
		}
		if role := directValkeyRoleAtAddress(
			t,
			net.JoinHostPort(pod.Status.PodIP, "6379"),
			operatorPassword,
		); role != expectedRole {
			t.Fatalf("DL-03 живой процесс сменил роль: %s", role)
		}
		negativeWindow := operatorconfig.NodeUnreachableTimeout +
			2*operatorconfig.HealthCheckInterval
		observeSafetyWindow(t, negativeWindow, func() {
			current := h.getInstance(t, instance)
			if current.Status.Deletion == nil ||
				current.Status.Deletion.Stage != valkeyv1alpha1.DeletionStageVerifying ||
				finalizerPoint.triggered.Load() {
				t.Fatalf("DL-03 завершила удаление живой изолированной ноды: %+v", current.Status)
			}
			secret := &corev1.Secret{}
			if err := h.k8s.Get(t.Context(), client.ObjectKey{
				Name: valkeyv1alpha1.AuthSecretName(instance.slug), Namespace: instance.namespace,
			}, secret); err != nil {
				t.Fatalf("DL-03 удалила Secret до доказательства остановки: %v", err)
			}
		})
		if err := fault.restore(); err != nil {
			t.Fatalf("DL-03 восстановить agent/control-plane: %v", err)
		}
		fault.assertAbsent(t)
		h.assertAgentControlPlane(t, target.NodeName, serverAddress, true)
		h.waitSameNodeReadyState(t, target.NodeName, node.UID, true)
		assertDL01FinalizerBoundary(t, h, instance, finalizerPoint, known)
		namespace := &corev1.Namespace{}
		if err := h.k8s.Get(t.Context(), client.ObjectKey{Name: instance.namespace}, namespace); err != nil {
			t.Fatalf("DL-03 оператор удалил namespace вызывающей стороны: %v", err)
		}
	}
}

func Test_DL04_ResumeHADeletion_WhenOperatorRestartsAtEveryStage_CompletesSafely(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHAWithWhitelist(t, "dlrestart", valkeyv1alpha1.WhitelistSpec{
		IsEnabled: true,
		CIDRs:     slices.Clone(h.operatorCIDRs),
	})
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	known := knownProcessIdentities(status)

	beforeNetwork := newDeletionActionPoint(
		t,
		instance.namespace,
		"stage-saved",
		valkeyv1alpha1.DeletionStageRemovingNetwork,
	)
	h.requestDeletion(t, instance)
	stopOperatorAtDeletionBoundary(t, h, instance, beforeNetwork, valkeyv1alpha1.DeletionStageRemovingNetwork, func(
		_ *valkeyv1alpha1.ValkeyInstance,
	) {
		assertDeletionNetworkState(t, h, instance, true)
	})

	afterNetwork := newDeletionActionPoint(
		t,
		instance.namespace,
		"action-completed",
		valkeyv1alpha1.DeletionStageRemovingNetwork,
	)
	h.startOperator(t)
	stopOperatorAtDeletionBoundary(t, h, instance, afterNetwork, valkeyv1alpha1.DeletionStageRemovingNetwork, func(
		_ *valkeyv1alpha1.ValkeyInstance,
	) {
		assertDeletionNetworkState(t, h, instance, false)
	})

	beforeApp := newDeletionActionPoint(
		t,
		instance.namespace,
		"stage-saved",
		valkeyv1alpha1.DeletionStageDisablingApp,
	)
	h.startOperator(t)
	stopOperatorAtDeletionBoundary(t, h, instance, beforeApp, valkeyv1alpha1.DeletionStageDisablingApp, nil)

	afterApp := newDeletionActionPoint(
		t,
		instance.namespace,
		"action-completed",
		valkeyv1alpha1.DeletionStageDisablingApp,
	)
	h.startOperator(t)
	stopOperatorAtDeletionBoundary(t, h, instance, afterApp, valkeyv1alpha1.DeletionStageDisablingApp, func(
		current *valkeyv1alpha1.ValkeyInstance,
	) {
		for _, process := range current.Status.Nodes {
			assertRealAppACLState(t, readRealAppACL(t, h, instance, process), instance.password, false)
		}
	})

	beforeStopping := newDeletionActionPoint(
		t,
		instance.namespace,
		"stage-saved",
		valkeyv1alpha1.DeletionStageStopping,
	)
	h.startOperator(t)
	stopOperatorAtDeletionBoundary(t, h, instance, beforeStopping, valkeyv1alpha1.DeletionStageStopping, func(
		_ *valkeyv1alpha1.ValkeyInstance,
	) {
		assertPodLifecycleCounts(t, h, instance, 3, 0)
	})

	afterStopping := newDeletionActionPoint(
		t,
		instance.namespace,
		"action-completed",
		valkeyv1alpha1.DeletionStageStopping,
	)
	h.startOperator(t)
	stopOperatorAtDeletionBoundary(t, h, instance, afterStopping, valkeyv1alpha1.DeletionStageStopping, func(
		_ *valkeyv1alpha1.ValkeyInstance,
	) {
		assertPodDeletionStarted(t, h, instance)
	})

	beforeVerifying := newDeletionActionPoint(
		t,
		instance.namespace,
		"stage-saved",
		valkeyv1alpha1.DeletionStageVerifying,
	)
	h.startOperator(t)
	stopOperatorAtDeletionBoundary(t, h, instance, beforeVerifying, valkeyv1alpha1.DeletionStageVerifying, func(
		_ *valkeyv1alpha1.ValkeyInstance,
	) {
		assertNoActivePods(t, h, instance)
	})

	beforeFinalizer := newDeletionActionPoint(
		t,
		instance.namespace,
		"before-finalizer-removal",
		valkeyv1alpha1.DeletionStageVerifying,
	)
	h.startOperator(t)
	assertDeletionFinalizerBoundary(t, h, instance, beforeFinalizer, known)
	h.stopOperator(t)
	beforeFinalizer.close()

	h.startOperator(t)
	waitForValkeyInstanceDeletion(t, h, instance)
	namespace := &corev1.Namespace{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{Name: instance.namespace}, namespace); err != nil {
		t.Fatalf("DL-04 оператор удалил namespace вызывающей стороны: %v", err)
	}
	h.waitDeleted(t, instance)
}

func stopOperatorAtDeletionBoundary(
	t *testing.T,
	h *harness,
	instance *testInstance,
	point *deletionActionPoint,
	stage valkeyv1alpha1.DeletionStage,
	check func(*valkeyv1alpha1.ValkeyInstance),
) {
	t.Helper()
	event := point.waitFor(t, 3*time.Minute)
	if event.Stage != stage {
		t.Fatalf("DL-04 ожидала стадию %s, получила %s", stage, event.Stage)
	}
	current := h.getInstance(t, instance)
	if current.Status.Deletion == nil || current.Status.Deletion.Stage != stage {
		t.Fatalf("DL-04 не сохранила стадию %s: %+v", stage, current.Status.Deletion)
	}
	if check != nil {
		check(current)
	}
	h.stopOperator(t)
	point.close()
}

func assertDeletionNetworkState(t *testing.T, h *harness, instance *testInstance, present bool) {
	t.Helper()
	for _, suffix := range []string{"", "-ro"} {
		key := client.ObjectKey{Name: instance.slug + suffix, Namespace: instance.namespace}
		for _, object := range []client.Object{
			&gatewayv1alpha2.TCPRoute{},
			&envoyv1alpha1.SecurityPolicy{},
		} {
			err := h.k8s.Get(t.Context(), key, object)
			if present && err != nil {
				t.Fatalf("DL-04 не нашла сетевой ресурс %T %s: %v", object, key.Name, err)
			}
			if !present && !apierrors.IsNotFound(err) {
				t.Fatalf("DL-04 сохранила сетевой ресурс %T %s: %v", object, key.Name, err)
			}
		}
	}
	gateway := &gatewayv1.Gateway{}
	if err := h.k8s.Get(
		t.Context(),
		client.ObjectKey{Name: "valkey", Namespace: systemNamespace},
		gateway,
	); err != nil {
		t.Fatalf("DL-04 прочитать Gateway: %v", err)
	}
	for _, suffix := range []string{"", "-ro"} {
		name := gatewayv1.SectionName(instance.slug + suffix)
		found := slices.ContainsFunc(gateway.Spec.Listeners, func(listener gatewayv1.Listener) bool {
			return listener.Name == name
		})
		if found != present {
			t.Fatalf("DL-04 listener %s присутствует=%t вместо %t", name, found, present)
		}
	}
}

func instancePods(t *testing.T, h *harness, instance *testInstance) []corev1.Pod {
	t.Helper()
	pods := &corev1.PodList{}
	if err := h.k8s.List(
		t.Context(),
		pods,
		client.InNamespace(instance.namespace),
		client.MatchingLabels{valkeyv1alpha1.InstanceLabelKey: instance.slug},
	); err != nil {
		t.Fatalf("прочитать Pod инстанса: %v", err)
	}
	return pods.Items
}

func assertPodLifecycleCounts(t *testing.T, h *harness, instance *testInstance, total, deleting int) {
	t.Helper()
	pods := instancePods(t, h, instance)
	actualDeleting := 0
	for index := range pods {
		if !pods[index].DeletionTimestamp.IsZero() {
			actualDeleting++
		}
	}
	if len(pods) != total || actualDeleting != deleting {
		t.Fatalf("DL-04 получила Pod=%d, удаляются=%d; ожидала Pod=%d, удаляются=%d",
			len(pods), actualDeleting, total, deleting)
	}
}

func assertPodDeletionStarted(t *testing.T, h *harness, instance *testInstance) {
	t.Helper()
	for _, pod := range instancePods(t, h, instance) {
		if !pod.DeletionTimestamp.IsZero() {
			return
		}
	}
	t.Fatal("DL-04 не запросила удаление ни одного Pod")
}

func assertNoActivePods(t *testing.T, h *harness, instance *testInstance) {
	t.Helper()
	for _, pod := range instancePods(t, h, instance) {
		if pod.DeletionTimestamp.IsZero() {
			t.Fatalf("DL-04 оставила активный Pod %s", pod.Name)
		}
	}
}

func waitForValkeyInstanceDeletion(t *testing.T, h *harness, instance *testInstance) {
	t.Helper()
	err := wait.PollUntilContextTimeout(
		t.Context(),
		250*time.Millisecond,
		2*time.Minute,
		true,
		func(ctx context.Context) (bool, error) {
			current := &valkeyv1alpha1.ValkeyInstance{}
			err := h.k8s.Get(ctx, client.ObjectKey{Name: instance.slug, Namespace: instance.namespace}, current)
			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		},
	)
	if err != nil {
		t.Fatalf("DL-04 дождаться удаления CR: %v", err)
	}
}

func Test_CT09_ReconcileCredentialLoss_WhenSecretOrServiceFieldReturns_ReusesCredentialsWithoutLeaks(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "ctcredentials")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	originalIdentities := processIdentities(status.Status.Nodes)
	originalPasswords := h.servicePasswords(t, instance)
	secretKey := client.ObjectKey{
		Name: valkeyv1alpha1.AuthSecretName(instance.slug), Namespace: instance.namespace,
	}
	originalSecret := &corev1.Secret{}
	if err := h.k8s.Get(t.Context(), secretKey, originalSecret); err != nil {
		t.Fatalf("CT-09 прочитать исходный Secret: %v", err)
	}

	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "ct09-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "ct09-key", "preserved")
	closeConnections([]*persistentConnection{connection})
	failedPrimary := primaryProcess(t, status)
	if err := h.k8s.Delete(t.Context(), originalSecret); err != nil {
		t.Fatalf("CT-09 удалить исходный Secret: %v", err)
	}
	waitForCredentialFailure(t, h, instance, "SecretNotFound")
	waitForCredentialEvent(t, h, instance, "SecretNotFound")
	if err := h.k8s.Get(t.Context(), secretKey, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("CT-09 оператор восстановил удалённый Secret: %v", err)
	}

	h.sigkillValkeyProcess(t, instance, failedPrimary)
	negativeWindow := operatorconfig.EmptyPrimaryTimeout +
		2*operatorconfig.HealthCheckInterval
	observeSafetyWindow(t, negativeWindow, func() {
		current := h.getInstance(t, instance)
		if current.Status.Failover != nil || current.Status.Rollout != nil ||
			current.Status.CredentialRotation != nil {
			t.Fatalf("CT-09 начал опасную операцию без Secret: %+v", current.Status)
		}
		assertProcessIdentities(t, originalIdentities, current.Status.Nodes)
		pod := h.getPodOrdinal(t, instance, failedPrimary.Ordinal)
		if string(pod.UID) != failedPrimary.PodUID {
			container := namedContainerStatus(pod.Status.ContainerStatuses, "valkey")
			if containerHasStarted(container) {
				t.Fatalf("CT-09 запустил новый процесс без Secret: %s", pod.UID)
			}
		}
		for _, process := range status.Status.Nodes {
			if process.Ordinal == failedPrimary.Ordinal {
				continue
			}
			pod := h.getPodOrdinal(t, instance, process.Ordinal)
			role := directValkeyRoleAtAddress(
				t,
				net.JoinHostPort(pod.Status.PodIP, "6379"),
				originalPasswords[0],
			)
			if role != "slave" {
				t.Fatalf("CT-09 продвинул ordinal %d без Secret: %s", process.Ordinal, role)
			}
		}
		if err := h.k8s.Get(t.Context(), secretKey, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Fatalf("CT-09 создал новый Secret во время ожидания: %v", err)
		}
	})

	restoredSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace},
		Type:       originalSecret.Type,
		Data:       maps.Clone(originalSecret.Data),
	}
	if err := h.k8s.Create(t.Context(), restoredSecret); err != nil {
		t.Fatalf("CT-09 вернуть исходный Secret: %v", err)
	}
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		primary, found := nodeStatusAtOrdinal(current.Status.Nodes, failedPrimary.Ordinal)
		return credentialsRecovered(current) && found && primary.PodUID != failedPrimary.PodUID &&
			current.Status.Failover == nil && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "CT-09 восстановления после возврата Secret")
	assertHAComposition(t, h, instance, status)
	if current := h.servicePasswords(t, instance); current != originalPasswords {
		t.Fatal("CT-09 изменил служебные пароли после возврата Secret")
	}
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "ct09-key"); value != "preserved" {
		closeConnections([]*persistentConnection{connection})
		t.Fatalf("CT-09 потерял синхронизированный ключ после восстановления: %q", value)
	}
	closeConnections([]*persistentConnection{connection})

	partialBaseline := processIdentities(status.Status.Nodes)
	partialSecret := &corev1.Secret{}
	if err := h.k8s.Get(t.Context(), secretKey, partialSecret); err != nil {
		t.Fatalf("CT-09 прочитать Secret перед удалением поля: %v", err)
	}
	replicaPassword := slices.Clone(partialSecret.Data[valkeyv1alpha1.ReplicaPasswordKey])
	if err := retry.RetryOnConflict(wait.Backoff{
		Duration: 100 * time.Millisecond, Factor: 2, Steps: 8,
	}, func() error {
		current := &corev1.Secret{}
		if err := h.k8s.Get(t.Context(), secretKey, current); err != nil {
			return err
		}
		delete(current.Data, valkeyv1alpha1.ReplicaPasswordKey)
		return h.k8s.Update(t.Context(), current)
	}); err != nil {
		t.Fatalf("CT-09 удалить служебное поле: %v", err)
	}
	waitForCredentialFailure(t, h, instance, "ServiceCredentialsPartial")
	waitForCredentialEvent(t, h, instance, "ServiceCredentialsPartial")
	observeSafetyWindow(t, negativeWindow, func() {
		current := h.getInstance(t, instance)
		assertProcessIdentities(t, partialBaseline, current.Status.Nodes)
		secret := &corev1.Secret{}
		if err := h.k8s.Get(t.Context(), secretKey, secret); err != nil {
			t.Fatalf("CT-09 прочитать частичный Secret: %v", err)
		}
		if len(secret.Data[valkeyv1alpha1.ReplicaPasswordKey]) != 0 {
			t.Fatal("CT-09 сгенерировал отсутствующий пароль репликации")
		}
		for _, process := range current.Status.Nodes {
			pod := h.getPodOrdinal(t, instance, process.Ordinal)
			expectedRole := "slave"
			if process.Role == valkeyv1alpha1.NodeRolePrimary {
				expectedRole = "master"
			}
			if role := directValkeyRoleAtAddress(
				t,
				net.JoinHostPort(pod.Status.PodIP, "6379"),
				originalPasswords[0],
			); role != expectedRole {
				t.Fatalf("CT-09 сменил роль ordinal %d при потере поля: %s", process.Ordinal, role)
			}
		}
	})

	sensitive := [][]byte{
		[]byte(instance.password),
		originalSecret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"1"],
		originalSecret.Data[valkeyv1alpha1.OperatorPasswordKey],
		originalSecret.Data[valkeyv1alpha1.ReplicaPasswordKey],
		originalSecret.Data[valkeyv1alpha1.HealthPasswordKey],
	}
	assertCT09DiagnosticsSafe(t, instance, sensitive)

	if err := retry.RetryOnConflict(wait.Backoff{
		Duration: 100 * time.Millisecond, Factor: 2, Steps: 8,
	}, func() error {
		current := &corev1.Secret{}
		if err := h.k8s.Get(t.Context(), secretKey, current); err != nil {
			return err
		}
		current.Data[valkeyv1alpha1.ReplicaPasswordKey] = slices.Clone(replicaPassword)
		return h.k8s.Update(t.Context(), current)
	}); err != nil {
		t.Fatalf("CT-09 восстановить служебное поле: %v", err)
	}
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return credentialsRecovered(current) && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "CT-09 восстановления служебного поля")
	assertProcessIdentities(t, partialBaseline, status.Status.Nodes)
	if current := h.servicePasswords(t, instance); current != originalPasswords {
		t.Fatal("CT-09 изменил комплект после восстановления служебного поля")
	}
	assertHAComposition(t, h, instance, status)
	h.deleteInstance(t, instance)
}

func Test_CT10CT11_RunSingleLifecycle_AfterHAFailureAndMutation_PreservesInstanceIsolation(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	ha := h.createHA(t, "ctregressionha")
	haStatus := h.waitRunning(t, ha)
	assertHAComposition(t, h, ha, haStatus)
	haPasswords := h.servicePasswords(t, ha)
	haImage := assertInstanceValkeyImage(t, h, ha, operatorconfig.DefaultValkeyImage)
	haConnection := openPersistentConnection(t, h.publicAddr, ha, h.caFile, false, func() {})
	setValue(t, haConnection, "ct11-ha-key", "preserved")
	assertKeyOnAllReplicas(t, h, ha, haStatus, "ct11-ha-key", "preserved")

	oldHAPassword := ha.password
	newHAPassword := "ct11-" + mustUUIDv7(t)
	h.rotatePassword(t, ha, newHAPassword, 2)
	haStatus = h.waitFor(t, ha, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.CredentialRotation == nil && current.Status.AppliedPasswordVersion == 2 &&
			current.Status.ObservedGeneration == 2 && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "CT-11 ротации пароля HA")
	assertConnectionClosed(t, haConnection, false)
	closeConnections([]*persistentConnection{haConnection})
	ha.password = newHAPassword
	assertAuthenticationRejected(t, h, ha, oldHAPassword)

	var failedReplica valkeyv1alpha1.NodeStatus
	for _, process := range haStatus.Status.Nodes {
		if process.Role == valkeyv1alpha1.NodeRoleReplica {
			failedReplica = process
			break
		}
	}
	if failedReplica.PodUID == "" {
		t.Fatalf("CT-11 не нашла реплику HA: %+v", haStatus.Status.Nodes)
	}
	h.deletePod(t, ha, failedReplica.Ordinal)
	replacement := h.waitForReplacementOrdinal(t, ha, failedReplica.Ordinal, types.UID(failedReplica.PodUID))
	haStatus = h.waitFor(t, ha, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		process, found := nodeStatusAtOrdinal(current.Status.Nodes, failedReplica.Ordinal)
		return found && process.PodUID == string(replacement.UID) && process.Readiness &&
			current.Status.Failover == nil && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "CT-11 восстановления реплики HA")
	assertHAComposition(t, h, ha, haStatus)
	assertKeyOnAllReplicas(t, h, ha, haStatus, "ct11-ha-key", "preserved")
	if current := h.servicePasswords(t, ha); current != haPasswords {
		t.Fatal("CT-11 изменила служебные пароли HA")
	}
	assertInstanceValkeyImage(t, h, ha, haImage)
	haConnection = openPersistentConnection(t, h.publicAddr, ha, h.caFile, false, func() {})
	if value := getValue(t, haConnection, "ct11-ha-key"); value != "preserved" {
		closeConnections([]*persistentConnection{haConnection})
		t.Fatalf("CT-11 потеряла ключ HA после замены реплики: %q", value)
	}

	single := h.createSingle(t, "ctregressionsingle", valkeyv1alpha1.WhitelistSpec{})
	singleStatus := h.waitRunning(t, single)
	if len(singleStatus.Status.Nodes) != 1 {
		t.Fatalf("CT-11 single получил неверный состав: %+v", singleStatus.Status.Nodes)
	}
	assertCT10IndependentInstanceObservation(t, h, ha, haStatus, single, singleStatus)
	haStatus = h.getInstance(t, ha)
	singleStatus = h.getInstance(t, single)
	assertHAComposition(t, h, ha, haStatus)
	singlePasswords := h.servicePasswords(t, single)
	singleImage := assertInstanceValkeyImage(t, h, single, operatorconfig.DefaultValkeyImage)
	singleConnections := h.openEnvoyConnections(t, single)
	setValue(t, singleConnections[0], "ct11-single-key", "single")
	pingConnections(t, singleConnections)
	assertAuthenticationRejected(t, h, single, ha.password)
	assertAuthenticationRejected(t, h, ha, single.password)
	assertWrongSNIRejected(t, h, single)
	assertPodIsolation(t, h, single, ha)
	assertPodIsolation(t, h, ha, single)

	h.deleteInstance(t, ha)
	assertConnectionClosed(t, haConnection, false)
	closeConnections([]*persistentConnection{haConnection})
	pingConnections(t, singleConnections)
	if value := getValue(t, singleConnections[0], "ct11-single-key"); value != "single" {
		t.Fatalf("CT-11 удаление HA изменило single: %q", value)
	}
	if current := h.servicePasswords(t, single); current != singlePasswords {
		t.Fatal("CT-11 удаление HA изменило служебные пароли single")
	}
	assertInstanceValkeyImage(t, h, single, singleImage)

	oldSingle := processAtOrdinal(t, h.getInstance(t, single), 0)
	h.deletePod(t, single, oldSingle.Ordinal)
	singleReplacement := h.waitForReplacementOrdinal(t, single, 0, types.UID(oldSingle.PodUID))
	singleStatus = h.waitFor(t, single, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		process, found := nodeStatusAtOrdinal(current.Status.Nodes, 0)
		return found && process.PodUID == string(singleReplacement.UID) && process.Readiness &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "CT-11 восстановления single после удаления Pod")
	for _, connection := range singleConnections {
		assertConnectionClosed(t, connection, false)
	}
	closeConnections(singleConnections)
	if current := h.servicePasswords(t, single); current != singlePasswords {
		t.Fatal("CT-11 замена single изменила служебные пароли")
	}
	assertInstanceValkeyImage(t, h, single, singleImage)
	replacementConnection := openPersistentConnection(t, h.publicAddr, single, h.caFile, false, func() {})
	writeRESP(t, replacementConnection, "GET", "ct11-single-key")
	if value := readRESP(t, replacementConnection); value != nil {
		closeConnections([]*persistentConnection{replacementConnection})
		t.Fatalf("CT-11 замена single сохранила старый кэш: %#v", value)
	}
	setValue(t, replacementConnection, "ct11-after-recovery", "ready")
	if value := getValue(t, replacementConnection, "ct11-after-recovery"); value != "ready" {
		closeConnections([]*persistentConnection{replacementConnection})
		t.Fatalf("CT-11 новый single не принимает запись: %q", value)
	}
	closeConnections([]*persistentConnection{replacementConnection})
	assertAuthenticationRejected(t, h, single, ha.password)
	assertWrongSNIRejected(t, h, single)
	h.deleteInstance(t, single)
}

func assertInstanceValkeyImage(
	t *testing.T,
	h *harness,
	instance *testInstance,
	expected string,
) string {
	t.Helper()
	status := h.getInstance(t, instance)
	image := status.Status.ValkeyImage
	if image != expected {
		t.Fatalf("CT-11 инстанс %s хранит образ %q вместо %q", instance.slug, image, expected)
	}
	for _, process := range status.Status.Nodes {
		pod := h.getPodOrdinal(t, instance, process.Ordinal)
		if podImage := valkeyContainer(t, pod.Spec.Containers).Image; podImage != expected {
			t.Fatalf("CT-11 Pod %s использует образ %q вместо %q", pod.Name, podImage, expected)
		}
	}
	return image
}

func waitForCredentialFailure(
	t *testing.T,
	h *harness,
	instance *testInstance,
	reason string,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()
	return h.waitForWithin(t, instance, time.Minute, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		credentialsReady := apimeta.FindStatusCondition(current.Status.Conditions, "CredentialsReady")
		recovery := apimeta.FindStatusCondition(current.Status.Conditions, "RecoveryRequired")
		return credentialsReady != nil && credentialsReady.Status == metav1.ConditionFalse &&
			credentialsReady.Reason == reason && recovery != nil &&
			recovery.Status == metav1.ConditionTrue && recovery.Reason == reason
	}, "CT-09 RECOVERY_REQUIRED "+reason)
}

func credentialsRecovered(instance *valkeyv1alpha1.ValkeyInstance) bool {
	ready := apimeta.FindStatusCondition(instance.Status.Conditions, "CredentialsReady")
	recovery := apimeta.FindStatusCondition(instance.Status.Conditions, "RecoveryRequired")
	return ready != nil && ready.Status == metav1.ConditionTrue && recovery == nil
}

func waitForCredentialEvent(t *testing.T, h *harness, instance *testInstance, reason string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(
		t.Context(), 250*time.Millisecond, 15*time.Second, true,
		func(ctx context.Context) (bool, error) {
			events := &corev1.EventList{}
			if err := h.k8s.List(ctx, events, client.InNamespace(instance.namespace)); err != nil {
				return false, err
			}
			return slices.ContainsFunc(events.Items, func(event corev1.Event) bool {
				return event.InvolvedObject.Name == instance.slug && event.Reason == "RecoveryRequired" &&
					strings.Contains(event.Message, reason)
			}), nil
		},
	)
	if err != nil {
		t.Fatalf("CT-09 дождаться безопасного Event %s: %v", reason, err)
	}
}

func assertCT09DiagnosticsSafe(t *testing.T, instance *testInstance, sensitive [][]byte) {
	t.Helper()
	repository := requiredEnv(t, "MANAGED_VALKEY_REPO_ROOT")
	stateDirectory := requiredEnv(t, "MV_STATE_DIR")
	command := exec.CommandContext(
		t.Context(),
		filepath.Join(repository, "scripts", "test_environment.sh"),
		"diagnostics",
		stateDirectory,
	)
	command.Env = append(os.Environ(), "MV_DIAGNOSTIC_SCENARIO=CT-09")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("CT-09 собрать диагностику: output=%q error=%v", output, err)
	}
	entries, err := os.ReadDir(filepath.Join(stateDirectory, "diagnostics"))
	if err != nil {
		t.Fatalf("CT-09 прочитать каталог диагностики: %v", err)
	}
	foundInstance := false
	foundRecovery := false
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(stateDirectory, "diagnostics", entry.Name()))
		if err != nil {
			t.Fatalf("CT-09 прочитать диагностический файл %s: %v", entry.Name(), err)
		}
		for _, value := range sensitive {
			if len(value) > 0 && bytes.Contains(content, value) {
				t.Fatalf("CT-09 диагностический файл %s содержит секретное значение", entry.Name())
			}
		}
		foundInstance = foundInstance || bytes.Contains(content, []byte(instance.slug))
		foundRecovery = foundRecovery || bytes.Contains(content, []byte("RECOVERY_REQUIRED"))
	}
	if !foundInstance || !foundRecovery {
		t.Fatalf("CT-09 диагностика не содержит безопасное состояние отказа: instance=%t recovery=%t",
			foundInstance, foundRecovery)
	}
}

func deletionAgentProcess(
	t *testing.T,
	instance *valkeyv1alpha1.ValkeyInstance,
) valkeyv1alpha1.NodeStatus {
	t.Helper()
	for _, process := range instance.Status.Nodes {
		if process.NodeName != "k3s-server" {
			return process
		}
	}
	t.Fatalf("не найден процесс Valkey на agent: %+v", instance.Status.Nodes)
	return valkeyv1alpha1.NodeStatus{}
}

func Test_PW02_RotatePassword_WithHashAndSpecDeliveredInEitherOrder_WaitsForBothInputs(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "pwdelivery")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	identities := processIdentities(status.Status.Nodes)
	servicePasswords := h.servicePasswords(t, instance)
	passwordOne := instance.password
	passwordTwo := "second-" + mustUUIDv7(t)
	passwordThree := "third-" + mustUUIDv7(t)
	assertSecretACLForPassword(t, h, instance, passwordOne, 1, servicePasswords)

	if status.Status.ObservedAt == nil {
		t.Fatal("PW-02 running HA не содержит observedAt")
	}
	previousObservedAt := status.Status.ObservedAt.DeepCopy()
	h.deliverPasswordHash(t, instance, passwordTwo, 2)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.ObservedAt != nil && previousObservedAt != nil &&
			current.Status.ObservedAt.Time.After(previousObservedAt.Time)
	}, "PW-02 наблюдения после ранней доставки хеша")
	if status.Status.AcceptedConfiguration == nil || status.Status.AcceptedConfiguration.PasswordVersion != 1 ||
		status.Status.CredentialRotation != nil || status.Status.AppliedPasswordVersion != 1 ||
		status.Status.ObservedGeneration != 1 {
		t.Fatalf("PW-02 применила хеш до намерения: %+v", status.Status)
	}
	assertSecretACLForPassword(t, h, instance, passwordOne, 1, servicePasswords)
	assertPasswordsOnAllProcesses(t, h, instance, passwordTwo, passwordOne)

	h.requestPasswordVersion(t, instance, 2, 2)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.CredentialRotation == nil && current.Status.AppliedPasswordVersion == 2 &&
			current.Status.ObservedGeneration == 2 && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "PW-02 применения хеша после намерения")
	assertProcessIdentities(t, identities, status.Status.Nodes)
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("PW-02 изменила служебные пароли при раннем хеше")
	}
	secret := assertSecretACLForPassword(t, h, instance, passwordTwo, 2, servicePasswords)
	if _, found := secret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"1"]; found {
		t.Fatal("PW-02 сохранила предыдущий хеш после завершения первой ротации")
	}
	assertPasswordsOnAllProcesses(t, h, instance, passwordOne, passwordTwo)

	h.requestPasswordVersion(t, instance, 3, 3)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		pending := slices.ContainsFunc(current.Status.Conditions, func(condition metav1.Condition) bool {
			return condition.Type == "ConfigurationPending" && condition.Status == metav1.ConditionTrue &&
				condition.Reason == "AppPasswordHashMissing"
		})
		return pending && current.Status.AcceptedConfiguration != nil &&
			current.Status.AcceptedConfiguration.PasswordVersion == 2 &&
			current.Status.CredentialRotation == nil && current.Status.AppliedPasswordVersion == 2 &&
			current.Status.ObservedGeneration == 2
	}, "PW-02 ожидания хеша после намерения")
	assertProcessIdentities(t, identities, status.Status.Nodes)
	assertSecretACLForPassword(t, h, instance, passwordTwo, 2, servicePasswords)
	assertPasswordsOnAllProcesses(t, h, instance, passwordThree, passwordTwo)

	h.deliverPasswordHash(t, instance, passwordThree, 3)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.AcceptedConfiguration != nil &&
			current.Status.AcceptedConfiguration.PasswordVersion == 3 &&
			current.Status.CredentialRotation == nil && current.Status.AppliedPasswordVersion == 3 &&
			current.Status.ObservedGeneration == 3 && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "PW-02 завершения после позднего хеша")
	assertProcessIdentities(t, identities, status.Status.Nodes)
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("PW-02 изменила служебные пароли при позднем хеше")
	}
	secret = assertSecretACLForPassword(t, h, instance, passwordThree, 3, servicePasswords)
	if _, found := secret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"2"]; found {
		t.Fatal("PW-02 сохранила предыдущий хеш после второй ротации")
	}
	assertPasswordsOnAllProcesses(t, h, instance, passwordTwo, passwordThree)
	instance.password = passwordThree

	h.deleteInstance(t, instance)
}

func Test_RZ03_ShrinkHA_WithFullStop_StartsEmptyOnTargetSize(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHAWithSize(t, "hashrink", 2, 2)
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	servicePasswords := h.servicePasswords(t, instance)
	before := processIdentities(status.Status.Nodes)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "shrink-key", "discarded")

	h.resize(t, instance, 1, 1, 2)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Initialized && current.Status.Rollout == nil &&
			current.Status.Applied != nil && current.Status.Applied.VCPU == 1 &&
			current.Status.Applied.RAMGB == 1 && current.Status.ObservedGeneration == 2 &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "RZ-03 самостоятельного уменьшения HA")
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	assertHAComposition(t, h, instance, status)
	assertAllProcessIdentitiesChanged(t, before, status.Status.Nodes)
	if current := h.servicePasswords(t, instance); current != servicePasswords {
		t.Fatal("RZ-03 изменила служебные пароли")
	}
	for _, process := range status.Status.Nodes {
		assertPodResourcesAndMaxmemory(t, h, instance, process, 1, 1)
	}
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	writeRESP(t, connection, "GET", "shrink-key")
	if value := readRESP(t, connection); value != nil {
		t.Fatalf("RZ-03 сохранил кэш после полного останова: %#v", value)
	}

	h.deleteInstance(t, instance)
}

func Test_RZ08_ResumeHAShrink_AfterRestarts_KeepsWorkloadStoppedUntilTargetTemplate(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHAWithSize(t, "rzshrinkrestart", 2, 2)
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	before := processIdentities(status.Status.Nodes)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "rz08-key", "discarded")
	assertKeyOnAllReplicas(t, h, instance, status, "rz08-key", "discarded")
	oldConfigName := podConfigMapName(t, &h.getPodOrdinal(t, instance, 0).Spec)

	beforeZero := newRolloutActionPoint(t, valkeyv1alpha1.RolloutStageStopping, 0)
	h.resize(t, instance, 1, 1, 2)
	beforeZeroEvent := beforeZero.waitFor(t, 3*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Rollout == nil || status.Status.Rollout.Stage != valkeyv1alpha1.RolloutStageStopping ||
		!status.Status.Rollout.AccessClosed {
		t.Fatalf("RZ-08 не сохранила закрытый полный останов: %+v", status.Status.Rollout)
	}
	if beforeZeroEvent.DesiredConfigName != oldConfigName {
		t.Fatalf("RZ-08 выбрала конфигурацию %s до полной остановки вместо %s",
			beforeZeroEvent.DesiredConfigName, oldConfigName)
	}
	for _, pod := range instancePods(t, h, instance) {
		if !pod.DeletionTimestamp.IsZero() || podConfigMapName(t, &pod.Spec) != oldConfigName {
			t.Fatalf("RZ-08 изменила Pod %s до границы полной остановки", pod.Name)
		}
	}
	h.stopOperator(t)
	beforeZero.close()
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})

	beforeStart := newRolloutActionPoint(t, valkeyv1alpha1.RolloutStageStarting, 3)
	h.startOperator(t)
	beforeStartEvent := beforeStart.waitFor(t, 3*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Rollout == nil || status.Status.Rollout.Stage != valkeyv1alpha1.RolloutStageStarting {
		t.Fatalf("RZ-08 не сохранила стадию запуска после полного останова: %+v", status.Status.Rollout)
	}
	targetConfigName := beforeStartEvent.DesiredConfigName
	if targetConfigName == oldConfigName {
		t.Fatalf("RZ-08 не выбрала целевую ConfigMap после полной остановки: %s", targetConfigName)
	}
	if pods := instancePods(t, h, instance); len(pods) != 0 {
		t.Fatalf("RZ-08 преждевременно создала Pod после полной остановки: %+v", pods)
	}
	for _, name := range []string{oldConfigName, targetConfigName} {
		configMap := &corev1.ConfigMap{}
		if err := h.k8s.Get(t.Context(), client.ObjectKey{
			Name: name, Namespace: instance.namespace,
		}, configMap); err != nil {
			t.Fatalf("RZ-08 не сохранила ConfigMap %s: %v", name, err)
		}
	}
	h.stopOperator(t)
	beforeStart.close()

	startedAt := time.Now()
	h.startOperator(t)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Rollout == nil && current.Status.Applied != nil &&
			current.Status.Applied.VCPU == 1 && current.Status.Applied.RAMGB == 1 &&
			current.Status.ObservedGeneration == 2 && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "RZ-08 завершения HA shrink после рестартов")
	t.Logf("RZ-08 пустой запуск после целевого шаблона: %s", time.Since(startedAt))
	assertHAComposition(t, h, instance, status)
	assertAllProcessIdentitiesChanged(t, before, status.Status.Nodes)
	for _, process := range status.Status.Nodes {
		assertPodResourcesAndMaxmemory(t, h, instance, process, 1, 1)
	}
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	writeRESP(t, connection, "GET", "rz08-key")
	if value := readRESP(t, connection); value != nil {
		t.Fatalf("RZ-08 сохранила ключ после полного останова: %#v", value)
	}
	setValue(t, connection, "rz08-new-key", "available")

	h.deleteInstance(t, instance)
}

func Test_RZ08_ResumeHARollingResize_AfterRestartsAtMutationBoundaries_PreservesData(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "rzrollingrestart")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	before := processIdentities(status.Status.Nodes)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "rz08-rolling-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "rz08-rolling-key", "preserved")
	oldConfigName := podConfigMapName(t, &h.getPodOrdinal(t, instance, 0).Spec)

	beforeTemplate := newRolloutActionPoint(t, valkeyv1alpha1.RolloutStageUpdatingTemplate, 3)
	h.resize(t, instance, 2, 2, 2)
	beforeTemplateEvent := beforeTemplate.waitFor(t, 3*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Rollout == nil || status.Status.Rollout.Stage != valkeyv1alpha1.RolloutStageUpdatingTemplate ||
		beforeTemplateEvent.DesiredConfigName == oldConfigName {
		t.Fatalf("RZ-08 не подготовила целевую конфигурацию: операция=%+v", status.Status.Rollout)
	}
	for _, pod := range instancePods(t, h, instance) {
		if podConfigMapName(t, &pod.Spec) != oldConfigName {
			t.Fatalf("RZ-08 изменила Pod %s до управляемой замены", pod.Name)
		}
	}
	h.stopOperator(t)
	beforeTemplate.close()

	afterTemplate := newNamedRolloutActionPoint(
		t,
		"workload-plan-saved",
		valkeyv1alpha1.RolloutStageUpdatingTemplate,
		3,
	)
	h.startOperator(t)
	afterTemplateEvent := afterTemplate.waitFor(t, 3*time.Minute)
	status = h.getInstance(t, instance)
	targetConfigName := afterTemplateEvent.DesiredConfigName
	if status.Status.Rollout == nil || status.Status.Rollout.Stage != valkeyv1alpha1.RolloutStageUpdatingTemplate ||
		targetConfigName == oldConfigName {
		t.Fatalf("RZ-08 не сохранила целевую конфигурацию: операция=%+v", status.Status.Rollout)
	}
	assertProcessIdentities(t, before, status.Status.Nodes)
	for _, pod := range instancePods(t, h, instance) {
		if podConfigMapName(t, &pod.Spec) != oldConfigName {
			t.Fatalf("RZ-08 изменила Pod %s при сохранении плана", pod.Name)
		}
	}
	h.stopOperator(t)
	afterTemplate.close()

	beforeDelete := newProcessActionPoint(t, "rollout-request-delete", "", "")
	h.startOperator(t)
	beforeDelete.waitFor(t, 3*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Rollout == nil || status.Status.Rollout.Process == nil ||
		status.Status.Rollout.Stage != valkeyv1alpha1.RolloutStageReplacingReplicas {
		t.Fatalf("RZ-08 не сохранила процесс перед DELETE: %+v", status.Status.Rollout)
	}
	target := processForIdentity(t, status.Status.Nodes, *status.Status.Rollout.Process)
	targetPod := h.getPodOrdinal(t, instance, target.Ordinal)
	if !targetPod.DeletionTimestamp.IsZero() {
		t.Fatalf("RZ-08 удалила Pod до управляемой границы: %s", targetPod.DeletionTimestamp)
	}
	h.stopOperator(t)
	beforeDelete.close()

	afterDelete := newProcessActionPoint(t, "rollout-delete-accepted", target.PodUID, "")
	h.startOperator(t)
	afterDelete.waitFor(t, 3*time.Minute)
	targetPod = h.getPodOrdinal(t, instance, target.Ordinal)
	if targetPod.UID != types.UID(target.PodUID) || targetPod.DeletionTimestamp.IsZero() {
		t.Fatalf("RZ-08 не сохранила принятый DELETE: uid=%s deletion=%s",
			targetPod.UID, targetPod.DeletionTimestamp)
	}
	h.stopOperator(t)
	afterDelete.close()

	beforeApplied := newRolloutStatusActionPoint(
		t,
		"before-rollout-applied",
		valkeyv1alpha1.RolloutStageVerifying,
	)
	h.startOperator(t)
	beforeApplied.waitFor(t, 4*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Rollout == nil || status.Status.Rollout.Stage != valkeyv1alpha1.RolloutStageVerifying ||
		status.Status.Applied == nil || status.Status.Applied.VCPU != 1 || status.Status.Applied.RAMGB != 1 ||
		status.Status.ObservedGeneration != 1 {
		t.Fatalf("RZ-08 подтвердила размер до границы applied: %+v", status.Status)
	}
	assertHAComposition(t, h, instance, status)
	beforeAppliedIdentities := processIdentities(status.Status.Nodes)
	h.stopOperator(t)
	beforeApplied.close()

	afterApplied := newRolloutStatusActionPoint(t, "after-rollout-applied", "")
	h.startOperator(t)
	afterApplied.waitFor(t, 2*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Rollout != nil || status.Status.Applied == nil || status.Status.Applied.VCPU != 2 ||
		status.Status.Applied.RAMGB != 2 {
		t.Fatalf("RZ-08 не сохранила applied перед рестартом: %+v", status.Status)
	}
	h.stopOperator(t)
	afterApplied.close()

	h.startOperator(t)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Rollout == nil && current.Status.Applied != nil &&
			current.Status.Applied.VCPU == 2 && current.Status.Applied.RAMGB == 2 &&
			current.Status.ObservedGeneration == 2 && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "RZ-08 завершения поколения после рестартов на границах rolling rollout")
	assertProcessIdentities(t, beforeAppliedIdentities, status.Status.Nodes)
	assertHAComposition(t, h, instance, status)
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	if value := getValue(t, connection, "rz08-rolling-key"); value != "preserved" {
		t.Fatalf("RZ-08 потеряла ключ после рестартов rolling rollout: %q", value)
	}

	h.deleteInstance(t, instance)
}

func Test_RZ04_ResizeHA_WhenReplacementHasNoCapacity_KeepsAcceptedRolloutUnapplied(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "rzpending")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	beforeNodes := append([]valkeyv1alpha1.NodeStatus(nil), status.Status.Nodes...)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "rz04-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "rz04-key", "preserved")

	deletePoint := newProcessActionPoint(t, "rollout-request-delete", "", "")
	h.resize(t, instance, 2, 2, 2)
	deletePoint.waitFor(t, 3*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Rollout == nil || status.Status.Rollout.Process == nil ||
		status.Status.Rollout.DesiredGeneration != 2 || status.Status.Rollout.VCPU != 2 ||
		status.Status.Rollout.RAMGB != 2 {
		t.Fatalf("RZ-04 не сохранила принятую цель: %+v", status.Status.Rollout)
	}
	target := processForIdentity(t, status.Status.Nodes, *status.Status.Rollout.Process)
	occupied := map[string]bool{}
	for _, process := range status.Status.Nodes {
		if process.Ordinal != target.Ordinal {
			occupied[process.NodeName] = true
		}
	}
	nodes := &corev1.NodeList{}
	if err := h.k8s.List(t.Context(), nodes); err != nil {
		t.Fatalf("RZ-04 прочитать Node: %v", err)
	}
	cordoned := make([]string, 0, len(nodes.Items)-len(occupied))
	for _, node := range nodes.Items {
		if occupied[node.Name] || node.Spec.Unschedulable {
			continue
		}
		h.setNodeUnschedulable(t, node.Name, true)
		cordoned = append(cordoned, node.Name)
	}
	if len(cordoned) == 0 {
		t.Fatal("RZ-04 не нашла Node для ограничения ёмкости")
	}
	t.Cleanup(func() {
		for _, nodeName := range cordoned {
			h.setNodeUnschedulable(t, nodeName, false)
		}
	})
	deletePoint.close()

	pending := h.waitForPendingReplacement(t, instance, target.Ordinal, types.UID(target.PodUID))
	status = h.getInstance(t, instance)
	if status.Status.Rollout == nil || status.Status.Rollout.DesiredGeneration != 2 ||
		status.Status.Rollout.VCPU != 2 || status.Status.Rollout.RAMGB != 2 ||
		status.Status.Applied == nil || status.Status.Applied.VCPU != 1 || status.Status.Applied.RAMGB != 1 ||
		status.Status.ObservedGeneration != 1 {
		t.Fatalf("RZ-04 подтвердила Pending rollout либо потеряла цель: %+v", status.Status)
	}
	if !otherRolloutProcessesUnchanged(beforeNodes, status.Status.Nodes, target.Ordinal) {
		t.Fatal("RZ-04 заменила другой процесс во время Pending")
	}
	t.Logf("RZ-04 Pending Pod %s сохранил неприменённое поколение", pending.Name)

	h.setNodeUnschedulable(t, target.NodeName, false)
	startedAt := time.Now()
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Rollout == nil && current.Status.Applied != nil &&
			current.Status.Applied.VCPU == 2 && current.Status.Applied.RAMGB == 2 &&
			current.Status.ObservedGeneration == 2 && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "RZ-04 продолжения rollout после возврата ёмкости")
	t.Logf("RZ-04 продолжение после Pending: %s", time.Since(startedAt))
	for _, nodeName := range cordoned {
		h.setNodeUnschedulable(t, nodeName, false)
	}
	cordoned = nil
	assertHAComposition(t, h, instance, status)
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	if value := getValue(t, connection, "rz04-key"); value != "preserved" {
		t.Fatalf("RZ-04 потеряла ключ после Pending: %q", value)
	}

	h.deleteInstance(t, instance)
}

func Test_RZ05_ResumeHARollout_WhenPrimaryFails_KeepsAcceptedTarget(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "rzprimaryfail")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "rz05-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "rz05-key", "preserved")

	replicaReady := newProcessActionPoint(t, "rollout-replacement-ready", "", "")
	h.resize(t, instance, 2, 2, 2)
	replicaReady.waitFor(t, 3*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Rollout == nil || status.Status.Rollout.Stage != valkeyv1alpha1.RolloutStageReplacingReplicas ||
		status.Status.Rollout.DesiredGeneration != 2 {
		t.Fatalf("RZ-05 не сохранила rollout после первой реплики: %+v", status.Status.Rollout)
	}
	firstSource := primaryProcess(t, status)
	startedAt := time.Now()
	h.sigkillValkeyProcess(t, instance, firstSource)
	replicaReady.close()
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Rollout != nil && current.Status.Rollout.DesiredGeneration == 2 &&
			current.Status.Failover != nil && current.Status.Failover.Reason == valkeyv1alpha1.FailoverReasonFailure
	}, "RZ-05 аварийного failover после первой обновлённой реплики")
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Rollout == nil && current.Status.Failover == nil && current.Status.Applied != nil &&
			current.Status.Applied.VCPU == 2 && current.Status.Applied.RAMGB == 2 &&
			current.Status.ObservedGeneration == 2 && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "RZ-05 завершения rollout после аварии primary")
	t.Logf("RZ-05 отказ primary после первой реплики: %s", time.Since(startedAt))
	closeConnections([]*persistentConnection{connection})
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "rz05-key"); value != "preserved" {
		t.Fatalf("RZ-05 потеряла ключ после первой аварии primary: %q", value)
	}
	setValue(t, connection, "rz05-switch-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "rz05-switch-key", "preserved")

	secondSource := primaryProcess(t, status)
	secondSourcePod := h.getPodOrdinal(t, instance, secondSource.Ordinal)
	fencingPoint := newValkeyCommandPoint(
		t,
		net.JoinHostPort(secondSourcePod.Status.PodIP, "6379"),
		"ACL SETUSER",
		operatorvalkey.IntegrationCommandBefore,
		false,
	)
	h.resize(t, instance, 1, 2, 3)
	fencingPoint.waitFor(t, 3*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Rollout == nil || status.Status.Rollout.DesiredGeneration != 3 ||
		status.Status.Failover == nil || status.Status.Failover.Reason != valkeyv1alpha1.FailoverReasonResize ||
		status.Status.Failover.Stage != valkeyv1alpha1.FailoverStageFencing {
		t.Fatalf(
			"RZ-05 не достигла планового fencing: rollout=%+v failover=%+v",
			status.Status.Rollout,
			status.Status.Failover,
		)
	}
	startedAt = time.Now()
	h.sigkillValkeyProcess(t, instance, secondSource)
	fencingPoint.close()
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Rollout != nil && current.Status.Rollout.DesiredGeneration == 3 &&
			current.Status.Failover != nil && current.Status.PrimaryPodUID == secondSource.PodUID
	}, "RZ-05 сохранённой цели после отказа на плановом fencing")
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Rollout == nil && current.Status.Failover == nil && current.Status.Applied != nil &&
			current.Status.Applied.VCPU == 1 && current.Status.Applied.RAMGB == 2 &&
			current.Status.ObservedGeneration == 3 && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "RZ-05 завершения после отказа на плановом переключении")
	t.Logf("RZ-05 отказ primary на плановом fencing: %s", time.Since(startedAt))
	closeConnections([]*persistentConnection{connection})
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	for _, key := range []string{"rz05-key", "rz05-switch-key"} {
		if value := getValue(t, connection, key); value != "preserved" {
			t.Fatalf("RZ-05 потеряла ключ %s: %q", key, value)
		}
	}
	assertHAComposition(t, h, instance, status)

	h.deleteInstance(t, instance)
}

func Test_RZ07_ResumeHARollout_WhenAgentIsDestroyedDuringRollingOrFullStop_CompletesTarget(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })
	h.requireNodeCount(t, 4)
	h.setNodeUnschedulable(t, "k3s-server", true)
	serverCordoned := true
	t.Cleanup(func() {
		if serverCordoned {
			h.setNodeUnschedulable(t, "k3s-server", false)
		}
	})

	instance := h.createHA(t, "rzagentloss")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	h.setNodeUnschedulable(t, "k3s-server", false)
	serverCordoned = false
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "rz07-rolling-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "rz07-rolling-key", "preserved")

	rollingPoint := newProcessActionPoint(t, "rollout-request-delete", "", "")
	h.resize(t, instance, 2, 2, 2)
	rollingPoint.waitFor(t, 3*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Rollout == nil || status.Status.Rollout.Process == nil ||
		status.Status.Rollout.Stage != valkeyv1alpha1.RolloutStageReplacingReplicas {
		t.Fatalf("RZ-07 не сохранила заменяемую реплику: %+v", status.Status.Rollout)
	}
	rollingTarget := processForIdentity(t, status.Status.Nodes, *status.Status.Rollout.Process)
	if rollingTarget.NodeName == "k3s-server" {
		t.Fatalf("RZ-07 выбрала server вместо agent: %+v", rollingTarget)
	}
	closeConnections([]*persistentConnection{connection})
	h.useSurvivingEnvoy(t, rollingTarget.NodeName)
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})

	rollingNode := h.getNode(t, rollingTarget.NodeName)
	rollingFault := h.newAgentFault(t, rollingNode)
	t.Cleanup(func() { rollingFault.restore(t, h) })
	startedAt := time.Now()
	rollingFault.kill(t, h)
	h.deleteNode(t, rollingNode)
	rollingPoint.close()
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		if current.Status.Rollout != nil || current.Status.Applied == nil ||
			current.Status.Applied.VCPU != 2 || current.Status.Applied.RAMGB != 2 ||
			current.Status.ObservedGeneration != 2 || current.Status.Phase != valkeyv1alpha1.InstancePhaseRunning {
			return false
		}
		for _, process := range current.Status.Nodes {
			if process.NodeUID == string(rollingNode.UID) || process.Termination != nil {
				return false
			}
		}
		return true
	}, "RZ-07 продолжения rolling rollout после node_deleted")
	t.Logf("RZ-07 rolling rollout после потери agent: %s", time.Since(startedAt))
	assertHAComposition(t, h, instance, status)
	for _, process := range status.Status.Nodes {
		assertPodResourcesAndMaxmemory(t, h, instance, process, 2, 2)
	}
	closeConnections([]*persistentConnection{connection})
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "rz07-rolling-key"); value != "preserved" {
		t.Fatalf("RZ-07 потеряла ключ после rolling rollout: %q", value)
	}
	closeConnections([]*persistentConnection{connection})
	rollingFault.restore(t, h)

	status = h.waitRunning(t, instance)
	var shrinkTarget valkeyv1alpha1.NodeStatus
	foundShrinkTarget := false
	for _, process := range status.Status.Nodes {
		if process.NodeName != "k3s-server" {
			shrinkTarget = process
			foundShrinkTarget = true
			break
		}
	}
	if !foundShrinkTarget {
		t.Fatalf("RZ-07 не нашла процесс на agent перед shrink: %+v", status.Status.Nodes)
	}
	h.useSurvivingEnvoy(t, shrinkTarget.NodeName)
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "rz07-shrink-key", "discarded")
	beforeShrink := processIdentities(status.Status.Nodes)

	shrinkPoint := newRolloutActionPoint(t, valkeyv1alpha1.RolloutStageStopping, 0)
	h.resize(t, instance, 1, 1, 3)
	shrinkPoint.waitFor(t, 3*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Rollout == nil || status.Status.Rollout.Stage != valkeyv1alpha1.RolloutStageStopping ||
		!status.Status.Rollout.AccessClosed {
		t.Fatalf("RZ-07 не достигла полной остановки: %+v", status.Status.Rollout)
	}

	shrinkNode := h.getNode(t, shrinkTarget.NodeName)
	shrinkFault := h.newAgentFault(t, shrinkNode)
	t.Cleanup(func() { shrinkFault.restore(t, h) })
	startedAt = time.Now()
	shrinkFault.kill(t, h)
	h.deleteNode(t, shrinkNode)
	shrinkPoint.close()
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		if current.Status.Rollout != nil || current.Status.Applied == nil ||
			current.Status.Applied.VCPU != 1 || current.Status.Applied.RAMGB != 1 ||
			current.Status.ObservedGeneration != 3 || current.Status.Phase != valkeyv1alpha1.InstancePhaseRunning {
			return false
		}
		for _, process := range current.Status.Nodes {
			if process.NodeUID == string(shrinkNode.UID) || process.Termination != nil {
				return false
			}
		}
		return true
	}, "RZ-07 продолжения полного останова после node_deleted")
	t.Logf("RZ-07 shrink после потери agent: %s", time.Since(startedAt))
	assertHAComposition(t, h, instance, status)
	assertAllProcessIdentitiesChanged(t, beforeShrink, status.Status.Nodes)
	for _, process := range status.Status.Nodes {
		assertPodResourcesAndMaxmemory(t, h, instance, process, 1, 1)
	}
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	writeRESP(t, connection, "GET", "rz07-shrink-key")
	if value := readRESP(t, connection); value != nil {
		t.Fatalf("RZ-07 сохранила кэш после shrink: %#v", value)
	}

	shrinkFault.restore(t, h)
	h.deleteInstance(t, instance)
}

func Test_RZ11_ProcessCompetingGenerationAndDeletion_WhenHARolloutIsInProgress_PreservesAcceptedTargetAndPrioritizesDeletion(
	t *testing.T,
) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "rzintent")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "rz11-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "rz11-key", "preserved")

	deletePoint := newProcessActionPoint(t, "rollout-request-delete", "", "")
	afterGenerationTwo := newRolloutStatusActionPoint(t, "after-rollout-applied", "")
	h.resize(t, instance, 2, 2, 2)
	deletePoint.waitFor(t, 3*time.Minute)
	h.resize(t, instance, 1, 2, 3)
	status = h.getInstance(t, instance)
	if status.Spec.DesiredGeneration != 3 || status.Status.AcceptedConfiguration == nil ||
		status.Status.AcceptedConfiguration.DesiredGeneration != 2 || status.Status.Rollout == nil ||
		status.Status.Rollout.DesiredGeneration != 2 || status.Status.Rollout.VCPU != 2 ||
		status.Status.Rollout.RAMGB != 2 || status.Status.ObservedGeneration != 1 {
		t.Fatalf("RZ-11 подменила принятую цель ранним поколением: %+v", status.Status)
	}
	deletePoint.close()
	afterGenerationTwo.waitFor(t, 4*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Rollout != nil || status.Status.AcceptedConfiguration == nil ||
		status.Status.AcceptedConfiguration.DesiredGeneration != 2 || status.Status.Applied == nil ||
		status.Status.Applied.VCPU != 2 || status.Status.Applied.RAMGB != 2 {
		t.Fatalf("RZ-11 не завершила сохранённое поколение перед следующим: %+v", status.Status)
	}
	afterGenerationTwo.close()

	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.AcceptedConfiguration != nil &&
			current.Status.AcceptedConfiguration.DesiredGeneration == 3 && current.Status.Rollout == nil &&
			current.Status.Applied != nil && current.Status.Applied.VCPU == 1 && current.Status.Applied.RAMGB == 2 &&
			current.Status.ObservedGeneration == 3 && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "RZ-11 применения последнего раннего поколения")
	assertHAComposition(t, h, instance, status)
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "rz11-key"); value != "preserved" {
		t.Fatalf("RZ-11 потеряла ключ между поколениями: %q", value)
	}
	setValue(t, connection, "rz11-delete-key", "removed")
	assertKeyOnAllReplicas(t, h, instance, status, "rz11-delete-key", "removed")

	deletionPoint := newProcessActionPoint(t, "rollout-request-delete", "", "")
	h.resize(t, instance, 2, 2, 4)
	deletionPoint.waitFor(t, 3*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Rollout == nil || status.Status.Rollout.Process == nil ||
		status.Status.Rollout.DesiredGeneration != 4 {
		t.Fatalf("RZ-11 не достигла удаления Pod во время rollout: %+v", status.Status.Rollout)
	}
	target := processForIdentity(t, status.Status.Nodes, *status.Status.Rollout.Process)
	knownPods := make(map[types.UID]struct{}, len(status.Status.Nodes))
	for _, process := range status.Status.Nodes {
		knownPods[types.UID(process.PodUID)] = struct{}{}
	}

	h.requestDeletion(t, instance)
	status = h.getInstance(t, instance)
	if status.DeletionTimestamp.IsZero() || status.Status.Rollout == nil ||
		status.Status.Rollout.DesiredGeneration != 4 {
		t.Fatalf("RZ-11 не сохранила deletionTimestamp поверх rollout: %+v", status)
	}
	targetPod := h.getPodOrdinal(t, instance, target.Ordinal)
	if targetPod.UID != types.UID(target.PodUID) || !targetPod.DeletionTimestamp.IsZero() {
		t.Fatalf("RZ-11 изменила Pod до возврата из границы rollout: uid=%s deletion=%s",
			targetPod.UID, targetPod.DeletionTimestamp)
	}
	deletionPoint.close()

	err := wait.PollUntilContextTimeout(
		t.Context(), 100*time.Millisecond, 5*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			pods := &corev1.PodList{}
			if err := h.k8s.List(
				ctx,
				pods,
				client.InNamespace(instance.namespace),
				client.MatchingLabels{valkeyv1alpha1.InstanceLabelKey: instance.slug},
			); err != nil {
				return false, err
			}
			for _, pod := range pods.Items {
				if _, found := knownPods[pod.UID]; !found {
					return false, fmt.Errorf("после запроса удаления появился Pod %s с UID %s", pod.Name, pod.UID)
				}
			}
			current := &valkeyv1alpha1.ValkeyInstance{}
			err := h.k8s.Get(ctx, client.ObjectKey{Name: instance.slug, Namespace: instance.namespace}, current)
			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		},
	)
	if err != nil {
		t.Fatalf("RZ-11 дождаться удаления без возрождения workload: %v", err)
	}
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	h.waitDeleted(t, instance)
}

func (h *harness) waitForPendingReplacement(
	t *testing.T,
	instance *testInstance,
	ordinal int32,
	oldUID types.UID,
) *corev1.Pod {
	t.Helper()
	var result corev1.Pod
	err := wait.PollUntilContextTimeout(
		t.Context(), 250*time.Millisecond, 2*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			pod := &corev1.Pod{}
			err := h.k8s.Get(ctx, client.ObjectKey{
				Name: fmt.Sprintf("%s-%d", instance.slug, ordinal), Namespace: instance.namespace,
			}, pod)
			if client.IgnoreNotFound(err) != nil {
				return false, err
			}
			if err != nil || pod.UID == oldUID || pod.Spec.NodeName != "" || pod.Status.Phase != corev1.PodPending {
				return false, nil
			}
			result = *pod.DeepCopy()
			return true, nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться Pending замены ordinal %d: %v", ordinal, err)
	}
	return result.DeepCopy()
}

func Test_RZ09_RestartOldPod_WhenHARolloutIsInProgress_UsesOriginalConfiguration(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "rzoldconfig")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "rz09-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "rz09-key", "preserved")

	point := newProcessActionPoint(t, "rollout-request-delete", "", "")
	h.resize(t, instance, 2, 2, 2)
	point.waitFor(t, 3*time.Minute)
	status = h.getInstance(t, instance)
	if status.Status.Rollout == nil || status.Status.Rollout.Process == nil ||
		status.Status.Rollout.Stage != valkeyv1alpha1.RolloutStageReplacingReplicas {
		t.Fatalf("RZ-09 не остановилась перед заменой старой реплики: %+v", status.Status.Rollout)
	}
	oldProcess := processForIdentity(t, status.Status.Nodes, *status.Status.Rollout.Process)
	oldPod := h.getPodOrdinal(t, instance, oldProcess.Ordinal)
	oldConfigName := podConfigMapName(t, &oldPod.Spec)
	oldImage := valkeyContainer(t, oldPod.Spec.Containers).Image

	targetConfigName := otherInstanceConfigMapName(t, h, instance, oldConfigName)
	if targetConfigName == oldConfigName {
		t.Fatalf("RZ-09 не сохранила целевую ConfigMap вместо %s", oldConfigName)
	}
	if status.Status.ValkeyImage != oldImage || status.Status.Rollout.Image != oldImage {
		t.Fatalf("RZ-09 изменила образ: процесс=%q сохранённый=%q операция=%q",
			oldImage, status.Status.ValkeyImage, status.Status.Rollout.Image)
	}
	for _, name := range []string{oldConfigName, targetConfigName} {
		configMap := &corev1.ConfigMap{}
		if err := h.k8s.Get(t.Context(), client.ObjectKey{
			Name: name, Namespace: instance.namespace,
		}, configMap); err != nil {
			t.Fatalf("RZ-09 не сохранила ConfigMap %s: %v", name, err)
		}
	}

	h.stopOperator(t)
	point.close()
	maxmemory := h.restartContainerTaskAndReadMaxmemory(t, oldPod, oldProcess.ContainerID, instance)
	expectedOldMaxmemory := int64(1024 * 1024 * 1024 * 3 / 4)
	if maxmemory != expectedOldMaxmemory {
		t.Fatalf("RZ-09 старый контейнер получил maxmemory %d вместо %d", maxmemory, expectedOldMaxmemory)
	}

	h.startOperator(t)
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Rollout == nil && current.Status.Applied != nil &&
			current.Status.Applied.VCPU == 2 && current.Status.Applied.RAMGB == 2 &&
			current.Status.ObservedGeneration == 2 && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, "RZ-09 завершения rollout после рестарта старого контейнера")
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	assertHAComposition(t, h, instance, status)
	for _, process := range status.Status.Nodes {
		assertPodResourcesAndMaxmemory(t, h, instance, process, 2, 2)
	}
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	if value := getValue(t, connection, "rz09-key"); value != "preserved" {
		t.Fatalf("RZ-09 потеряла синхронизированный ключ: %q", value)
	}

	h.deleteInstance(t, instance)
}

func Test_RZ06_ResumeHARollout_WhenUpdatedReplicaFails_WaitsForSynchronizedReplacement(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	instance := h.createHA(t, "rzreplicafail")
	status := h.waitRunning(t, instance)
	assertHAComposition(t, h, instance, status)
	connection := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	setValue(t, connection, "rz06-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "rz06-key", "preserved")
	closeConnections([]*persistentConnection{connection})

	startedAt := time.Now()
	h.failUpdatedReplicaDuringRollout(t, instance, 2, 2, 2, false)
	t.Logf("RZ-06 отказ обновляемой реплики до синхронизации: %s", time.Since(startedAt))
	status = h.waitRunning(t, instance)
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	if value := getValue(t, connection, "rz06-key"); value != "preserved" {
		t.Fatalf("RZ-06 потеряла ключ после отказа до синхронизации: %q", value)
	}
	setValue(t, connection, "rz06-after-sync-key", "preserved")
	assertKeyOnAllReplicas(t, h, instance, status, "rz06-after-sync-key", "preserved")
	closeConnections([]*persistentConnection{connection})

	startedAt = time.Now()
	h.failUpdatedReplicaDuringRollout(t, instance, 1, 2, 3, true)
	t.Logf("RZ-06 отказ обновлённой реплики после синхронизации: %s", time.Since(startedAt))
	status = h.waitRunning(t, instance)
	connection = openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{connection})
	for _, key := range []string{"rz06-key", "rz06-after-sync-key"} {
		if value := getValue(t, connection, key); value != "preserved" {
			t.Fatalf("RZ-06 потеряла ключ %s после отказа синхронизированной реплики: %q", key, value)
		}
	}

	h.deleteInstance(t, instance)
}

func (h *harness) failUpdatedReplicaDuringRollout(
	t *testing.T,
	instance *testInstance,
	vcpu int32,
	ram int32,
	generation int64,
	afterSync bool,
) {
	t.Helper()
	action := "rollout-request-delete"
	if afterSync {
		action = "rollout-replacement-ready"
	}
	point := newProcessActionPoint(t, action, "", "")
	before := h.getInstance(t, instance)
	h.resize(t, instance, vcpu, ram, generation)
	point.waitFor(t, 3*time.Minute)

	status := h.getInstance(t, instance)
	if status.Status.Rollout == nil || status.Status.Rollout.Process == nil ||
		status.Status.Rollout.Stage != valkeyv1alpha1.RolloutStageReplacingReplicas {
		t.Fatalf("RZ-06 не остановилась на обновлении реплики: %+v", status.Status.Rollout)
	}
	oldProcess := processForIdentity(
		t,
		append(status.Status.Nodes, status.Status.PreviousProcesses...),
		*status.Status.Rollout.Process,
	)
	target := oldProcess
	if afterSync {
		target = processAtOrdinal(t, status, oldProcess.Ordinal)
		if sameNodeProcess(target, oldProcess) || target.Replication == nil || target.Replication.SyncedAt == nil {
			t.Fatalf("RZ-06 не получила синхронизированную замену: old=%+v current=%+v", oldProcess, target)
		}
	}
	primary := primaryProcess(t, status)
	primaryPod := h.getPodOrdinal(t, instance, primary.Ordinal)
	targetNode := h.getNode(t, target.NodeName)
	if targetNode.Spec.PodCIDR == "" {
		t.Fatalf("RZ-06 Node %s не содержит PodCIDR", target.NodeName)
	}
	fault := h.addNodeNetworkFault(
		t,
		fmt.Sprintf("rz06-%d", generation),
		target.NodeName,
		"FORWARD",
		targetNode.Spec.PodCIDR,
		hostCIDR(t, primaryPod.Status.PodIP),
		6379,
	)
	fault.assertPresent(t)

	if afterSync {
		h.sigkillValkeyProcess(t, instance, target)
		point.close()
	} else {
		point.close()
		replacementPod := h.waitForReplacementOrdinal(t, instance, oldProcess.Ordinal, types.UID(oldProcess.PodUID))
		status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
			replacement, found := nodeStatusAtOrdinal(current.Status.Nodes, oldProcess.Ordinal)
			return found && replacement.PodUID == string(replacementPod.UID) &&
				replacement.Replication != nil && !replacement.Replication.LinkUp &&
				replacement.Replication.SyncedAt == nil
		}, "RZ-06 обновляемой реплики до синхронизации")
		target = processAtOrdinal(t, status, oldProcess.Ordinal)
		h.sigkillValkeyProcess(t, instance, target)
	}

	replacementPod := h.waitForReplacementOrdinal(t, instance, target.Ordinal, types.UID(target.PodUID))
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		replacement, found := nodeStatusAtOrdinal(current.Status.Nodes, target.Ordinal)
		if !found || replacement.PodUID != string(replacementPod.UID) || replacement.Replication == nil ||
			replacement.Replication.LinkUp || replacement.Replication.SyncedAt != nil ||
			current.Status.Rollout == nil || current.Status.Rollout.Stage != valkeyv1alpha1.RolloutStageReplacingReplicas {
			return false
		}
		return otherRolloutProcessesUnchanged(before.Status.Nodes, current.Status.Nodes, target.Ordinal)
	}, "RZ-06 ожидания синхронизации повторной замены")
	if status.Status.PrimaryPodUID != primary.PodUID || status.Status.Failover != nil {
		t.Fatalf("RZ-06 сменила primary до готовности повторной замены: %+v", status.Status)
	}

	if err := fault.restore(); err != nil {
		t.Fatalf("RZ-06 восстановить репликацию: %v", err)
	}
	status = h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Rollout == nil && current.Status.Applied != nil &&
			current.Status.Applied.VCPU == vcpu && current.Status.Applied.RAMGB == ram &&
			current.Status.ObservedGeneration == generation && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning
	}, fmt.Sprintf("RZ-06 завершения поколения %d", generation))
	assertHAComposition(t, h, instance, status)
	for _, process := range status.Status.Nodes {
		assertPodResourcesAndMaxmemory(t, h, instance, process, vcpu, ram)
	}
}

func otherRolloutProcessesUnchanged(
	before []valkeyv1alpha1.NodeStatus,
	after []valkeyv1alpha1.NodeStatus,
	targetOrdinal int32,
) bool {
	for _, previous := range before {
		if previous.Ordinal == targetOrdinal {
			continue
		}
		current, found := nodeStatusAtOrdinal(after, previous.Ordinal)
		if !found || !sameNodeProcess(current, previous) {
			return false
		}
	}
	return true
}

func podConfigMapName(t *testing.T, podSpec *corev1.PodSpec) string {
	t.Helper()
	for _, volume := range podSpec.Volumes {
		if volume.Name == "config" && volume.ConfigMap != nil {
			return volume.ConfigMap.Name
		}
	}
	t.Fatal("Pod не содержит том config из ConfigMap")
	return ""
}

func otherInstanceConfigMapName(
	t *testing.T,
	h *harness,
	instance *testInstance,
	excluded string,
) string {
	t.Helper()
	configMaps := &corev1.ConfigMapList{}
	if err := h.k8s.List(
		t.Context(),
		configMaps,
		client.InNamespace(instance.namespace),
		client.MatchingLabels{valkeyv1alpha1.InstanceLabelKey: instance.slug},
	); err != nil {
		t.Fatalf("прочитать ConfigMap инстанса: %v", err)
	}
	for _, configMap := range configMaps.Items {
		if configMap.Name != excluded {
			return configMap.Name
		}
	}
	t.Fatalf("не найдена целевая ConfigMap, отличная от %s", excluded)
	return ""
}

func valkeyContainer(t *testing.T, containers []corev1.Container) *corev1.Container {
	t.Helper()
	for index := range containers {
		if containers[index].Name == "valkey" {
			return &containers[index]
		}
	}
	t.Fatal("Pod не содержит контейнер Valkey")
	return nil
}

func (h *harness) restartContainerTaskAndReadMaxmemory(
	t *testing.T,
	pod *corev1.Pod,
	containerID string,
	instance *testInstance,
) int64 {
	t.Helper()
	nodeContainer := h.composeNodeContainer(t, pod.Spec.NodeName, true)
	id := strings.TrimPrefix(containerID, "containerd://")
	if id == "" || id == containerID {
		t.Fatalf("RZ-09 получила неверный container ID %q", containerID)
	}
	if output, err := exec.Command(
		"docker", "exec", nodeContainer, "crictl", "stop", "--timeout", "0", id,
	).CombinedOutput(); err != nil {
		t.Fatalf("RZ-09 остановить старый контейнер %s: output=%q error=%v", id, output, err)
	}
	if output, err := exec.Command(
		"docker", "exec", nodeContainer, "ctr", "-n", "k8s.io", "tasks", "start", "--detach", id,
	).CombinedOutput(); err != nil {
		t.Fatalf("RZ-09 перезапустить старый контейнер %s: output=%q error=%v", id, output, err)
	}

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			if output, err := exec.Command(
				"docker", "exec", nodeContainer, "ctr", "-n", "k8s.io", "tasks", "delete", "--force", id,
			).CombinedOutput(); err != nil {
				t.Errorf("RZ-09 остановить перезапущенный контейнер %s: output=%q error=%v", id, output, err)
			}
		})
	}
	t.Cleanup(stop)

	password := h.servicePasswords(t, instance)[0]
	var lastOutput string
	err := wait.PollUntilContextTimeout(
		t.Context(), 250*time.Millisecond, 15*time.Second, true,
		func(context.Context) (bool, error) {
			execID := "rz09-" + strings.ReplaceAll(mustUUIDv7(t), "-", "")
			output, commandErr := exec.Command(
				"docker", "exec", nodeContainer,
				"ctr", "-n", "k8s.io", "tasks", "exec", "--exec-id", execID, id,
				"valkey-cli", "--user", "operator", "--pass", password,
				"--no-auth-warning", "--raw", "CONFIG", "GET", "maxmemory",
			).CombinedOutput()
			lastOutput = string(output)
			return commandErr == nil, nil
		},
	)
	stop()
	if err != nil {
		t.Fatalf("RZ-09 прочитать maxmemory после рестарта: output=%q error=%v", lastOutput, err)
	}
	fields := strings.Fields(lastOutput)
	if len(fields) != 2 || fields[0] != "maxmemory" {
		t.Fatalf("RZ-09 получила неверный CONFIG GET maxmemory: %q", lastOutput)
	}
	maxmemory, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("RZ-09 разобрать maxmemory %q: %v", fields[1], err)
	}
	return maxmemory
}

func resizeSingleAndAssertEmpty(
	t *testing.T,
	h *harness,
	instance *testInstance,
	connection *persistentConnection,
	vcpu int32,
	ram int32,
	generation int64,
) {
	t.Helper()
	before := h.getInstance(t, instance)
	if len(before.Status.Nodes) != 1 {
		t.Fatalf("RZ-01 получил неверный состав single: %+v", before.Status.Nodes)
	}
	oldProcess := before.Status.Nodes[0]
	setValue(t, connection, "resize-key", strconv.FormatInt(generation, 10))
	startedAt := time.Now()
	h.resize(t, instance, vcpu, ram, generation)
	status := h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Rollout == nil && current.Status.Applied != nil &&
			current.Status.Applied.VCPU == vcpu && current.Status.Applied.RAMGB == ram &&
			current.Status.ObservedGeneration == generation &&
			current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning && len(current.Status.Nodes) == 1 &&
			current.Status.Nodes[0].PodUID != oldProcess.PodUID
	}, fmt.Sprintf("RZ-01 ресайза single до %d CPU/%d GiB", vcpu, ram))
	assertConnectionClosed(t, connection, false)
	closeConnections([]*persistentConnection{connection})
	current := openPersistentConnection(t, h.publicAddr, instance, h.caFile, false, func() {})
	writeRESP(t, current, "GET", "resize-key")
	if value := readRESP(t, current); value != nil {
		closeConnections([]*persistentConnection{current})
		t.Fatalf("RZ-01 сохранил кэш после полного останова: %#v", value)
	}
	assertPodResourcesAndMaxmemory(t, h, instance, status.Status.Nodes[0], vcpu, ram)
	closeConnections([]*persistentConnection{current})
	t.Logf("RZ-01 ресайз single до %d CPU/%d GiB: %s", vcpu, ram, time.Since(startedAt))
}

func processIdentities(nodes []valkeyv1alpha1.NodeStatus) map[int32]valkeyv1alpha1.ProcessIdentity {
	result := make(map[int32]valkeyv1alpha1.ProcessIdentity, len(nodes))
	for _, node := range nodes {
		result[node.Ordinal] = valkeyv1alpha1.ProcessIdentity{
			PodUID: node.PodUID, ContainerID: node.ContainerID,
			RunID: node.RunID, NodeName: node.NodeName, NodeUID: node.NodeUID,
		}
	}
	return result
}

func assertProcessIdentities(
	t *testing.T,
	expected map[int32]valkeyv1alpha1.ProcessIdentity,
	actual []valkeyv1alpha1.NodeStatus,
) {
	t.Helper()
	if len(expected) != len(actual) {
		t.Fatalf("число процессов изменилось: %d != %d", len(expected), len(actual))
	}
	for _, process := range actual {
		identity := valkeyv1alpha1.ProcessIdentity{
			PodUID: process.PodUID, ContainerID: process.ContainerID,
			RunID: process.RunID, NodeName: process.NodeName, NodeUID: process.NodeUID,
		}
		if expected[process.Ordinal] != identity {
			t.Fatalf("идентичность ordinal %d изменилась при ротации: %+v", process.Ordinal, identity)
		}
	}
}

func assertPasswordsOnAllProcesses(
	t *testing.T,
	h *harness,
	instance *testInstance,
	oldPassword string,
	newPassword string,
) {
	t.Helper()
	current := h.getInstance(t, instance)
	for _, process := range current.Status.Nodes {
		assertProcessPasswordAcceptedAndRejected(t, h, instance, process, newPassword, oldPassword)
	}
}

func assertProcessPasswordAcceptedAndRejected(
	t *testing.T,
	h *harness,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
	acceptedPassword string,
	rejectedPassword string,
) {
	t.Helper()
	pod := h.getPodOrdinal(t, instance, process.Ordinal)
	output, err := execInPod(t, h.adminREST, pod.Namespace, pod.Name, []string{
		"valkey-cli", "--user", "app", "--pass", acceptedPassword, "--no-auth-warning", "PING",
	})
	if err != nil || strings.TrimSpace(output) != "PONG" {
		t.Fatalf("актуальный пароль не принят ordinal %d: output=%q error=%v", process.Ordinal, output, err)
	}
	output, err = execInPod(t, h.adminREST, pod.Namespace, pod.Name, []string{
		"valkey-cli", "--user", "app", "--pass", rejectedPassword, "--no-auth-warning", "PING",
	})
	if err == nil && strings.TrimSpace(output) == "PONG" {
		t.Fatalf("предыдущий пароль принят ordinal %d", process.Ordinal)
	}
}

func (h *harness) requestPasswordAndSize(
	t *testing.T,
	instance *testInstance,
	passwordVersion int64,
	vcpu int32,
	ram int32,
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
		resource.Spec.PasswordVersion = passwordVersion
		resource.Spec.VCPU = vcpu
		resource.Spec.RAMGB = ram
		resource.Spec.DesiredGeneration = generation
		return h.k8s.Update(t.Context(), resource)
	}); err != nil {
		t.Fatalf("запросить общее изменение пароля и размера: %v", err)
	}
}

type realAppACL map[string]json.RawMessage

func readRealAppACLs(
	t *testing.T,
	h *harness,
	instance *testInstance,
	processes []valkeyv1alpha1.NodeStatus,
) map[int32]realAppACL {
	t.Helper()
	result := make(map[int32]realAppACL, len(processes))
	for _, process := range processes {
		result[process.Ordinal] = readRealAppACL(t, h, instance, process)
	}
	return result
}

func readRealAppACL(
	t *testing.T,
	h *harness,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
) realAppACL {
	t.Helper()
	pod := h.getPodOrdinal(t, instance, process.Ordinal)
	operatorPassword := h.servicePasswords(t, instance)[0]
	output, err := execInPod(t, h.adminREST, pod.Namespace, pod.Name, []string{
		"valkey-cli", "--user", "operator", "--pass", operatorPassword,
		"--no-auth-warning", "--json", "ACL", "GETUSER", "app",
	})
	if err != nil {
		t.Fatalf("прочитать ACL app ordinal %d: %v", process.Ordinal, err)
	}
	result := realAppACL{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &result); err != nil {
		t.Fatalf("разобрать ACL app ordinal %d: %v", process.Ordinal, err)
	}
	return result
}

func assertRealAppACLsChangedOnlyPassword(
	t *testing.T,
	h *harness,
	instance *testInstance,
	processes []valkeyv1alpha1.NodeStatus,
	before map[int32]realAppACL,
	password string,
	enabled bool,
) {
	t.Helper()
	for _, process := range processes {
		expected, found := before[process.Ordinal]
		if !found {
			t.Fatalf("нет исходного ACL ordinal %d", process.Ordinal)
		}
		actual := readRealAppACL(t, h, instance, process)
		assertRealAppACLState(t, actual, password, enabled)
		assertRealAppACLFieldsEqual(t, expected, actual, "passwords")
	}
}

func assertRealAppACLState(t *testing.T, acl realAppACL, password string, enabled bool) {
	t.Helper()
	var flags []string
	if err := json.Unmarshal(acl["flags"], &flags); err != nil {
		t.Fatalf("разобрать flags ACL app: %v", err)
	}
	actualEnabled := slices.Contains(flags, "on") && !slices.Contains(flags, "off")
	if actualEnabled != enabled {
		t.Fatalf("ACL app enabled=%t вместо %t", actualEnabled, enabled)
	}
	var hashes []string
	if err := json.Unmarshal(acl["passwords"], &hashes); err != nil {
		t.Fatalf("разобрать passwords ACL app: %v", err)
	}
	digest := sha256.Sum256([]byte(password))
	if len(hashes) != 1 || hashes[0] != hex.EncodeToString(digest[:]) {
		t.Fatal("ACL app не содержит единственный ожидаемый хеш")
	}
}

func assertRealAppACLFieldsEqual(t *testing.T, before, after realAppACL, excluded ...string) {
	t.Helper()
	beforeCompared := maps.Clone(before)
	afterCompared := maps.Clone(after)
	for _, field := range excluded {
		delete(beforeCompared, field)
		delete(afterCompared, field)
	}
	beforeJSON, err := json.Marshal(beforeCompared)
	if err != nil {
		t.Fatalf("сериализовать исходный ACL app: %v", err)
	}
	afterJSON, err := json.Marshal(afterCompared)
	if err != nil {
		t.Fatalf("сериализовать новый ACL app: %v", err)
	}
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatal("ротация изменила поля ACL app помимо разрешённых")
	}
}

func fenceRealProcessApp(t *testing.T, podIP, operatorPassword, appPassword string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	connection, err := operatorvalkey.Dial(ctx, operatorvalkey.ClientConfig{
		Address: net.JoinHostPort(podIP, "6379"), Username: "operator", Password: operatorPassword,
	})
	if err != nil {
		t.Fatalf("подключиться к процессу для fencing: %v", err)
	}
	defer connection.Close()
	session, err := connection.OpenSession(ctx)
	if err != nil {
		t.Fatalf("открыть сессию для fencing: %v", err)
	}
	defer session.Close()
	if _, err := session.TakeControl(ctx, operatorPassword); err != nil {
		t.Fatalf("принять управление перед fencing: %v", err)
	}
	digest := sha256.Sum256([]byte(appPassword))
	if err := session.SetAppUser(ctx, false, hex.EncodeToString(digest[:])); err != nil {
		t.Fatalf("выключить app перед PW-08: %v", err)
	}
	if err := session.KillAppClients(ctx); err != nil {
		t.Fatalf("закрыть app-соединения перед PW-08: %v", err)
	}
	state, err := session.TakeControl(ctx, operatorPassword)
	if err != nil {
		t.Fatalf("проверить fencing перед PW-08: %v", err)
	}
	if state.AppEnabled || len(state.AppPasswordHashes) != 1 ||
		state.AppPasswordHashes[0] != hex.EncodeToString(digest[:]) {
		t.Fatal("процесс не подтвердил fencing перед PW-08")
	}
}

func openDirectAppConnection(
	t *testing.T,
	address string,
	password string,
	pubsub bool,
) *persistentConnection {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		t.Fatalf("подключиться напрямую к процессу Valkey: %v", err)
	}
	result := &persistentConnection{
		conn: connection, read: bufio.NewReader(connection), pub: pubsub, stop: func() {},
	}
	writeRESP(t, result, "AUTH", "app", password)
	if response := readRESP(t, result); response != "OK" {
		closeConnections([]*persistentConnection{result})
		t.Fatalf("прямой AUTH app вернул %#v", response)
	}
	if pubsub {
		writeRESP(t, result, "SUBSCRIBE", "pw01-direct")
		if response := readRESP(t, result); response == nil {
			closeConnections([]*persistentConnection{result})
			t.Fatal("прямой SUBSCRIBE не подтвердил подписку")
		}
	}
	return result
}

func assertSecretACLForPassword(
	t *testing.T,
	h *harness,
	instance *testInstance,
	password string,
	version int64,
	servicePasswords [3]string,
) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{
		Name: valkeyv1alpha1.AuthSecretName(instance.slug), Namespace: instance.namespace,
	}, secret); err != nil {
		t.Fatalf("прочитать Secret ACL %s: %v", instance.slug, err)
	}
	digest := sha256.Sum256([]byte(password))
	hash := hex.EncodeToString(digest[:])
	key := valkeyv1alpha1.AppPasswordHashKeyPrefix + strconv.FormatInt(version, 10)
	if string(secret.Data[key]) != hash {
		t.Fatalf("Secret %s не содержит хеш версии %d", instance.slug, version)
	}
	expected := operatorvalkey.InitialACL(
		hash,
		servicePasswords[0],
		servicePasswords[1],
		servicePasswords[2],
	)
	if !bytes.Equal(secret.Data[valkeyv1alpha1.UsersACLKey], expected) {
		t.Fatalf("users.acl %s не соответствует выключенному app версии %d", instance.slug, version)
	}
	return secret.DeepCopy()
}

func assertConnectionClosed(t *testing.T, connection *persistentConnection, responsePending bool) {
	t.Helper()
	_ = connection.conn.SetDeadline(time.Now().Add(5 * time.Second))
	if !responsePending {
		if _, err := io.WriteString(connection.conn, "*1\r\n$4\r\nPING\r\n"); err != nil {
			return
		}
	}
	if line, err := connection.read.ReadString('\n'); err == nil {
		t.Fatalf("старое соединение осталось открытым: %q", line)
	}
}

func assertPodResourcesAndMaxmemory(
	t *testing.T,
	h *harness,
	instance *testInstance,
	process valkeyv1alpha1.NodeStatus,
	vcpu int32,
	ram int32,
) {
	t.Helper()
	pod := h.getPodOrdinal(t, instance, process.Ordinal)
	var container *corev1.Container
	for index := range pod.Spec.Containers {
		if pod.Spec.Containers[index].Name == "valkey" {
			container = &pod.Spec.Containers[index]
			break
		}
	}
	if container == nil {
		t.Fatalf("Pod %s не содержит Valkey", pod.Name)
	}
	expectedCPU := resource.MustParse(strconv.FormatInt(int64(vcpu), 10))
	expectedMemory := resource.MustParse(strconv.FormatInt(int64(ram), 10) + "Gi")
	if container.Resources.Requests.Cpu().Cmp(expectedCPU) != 0 ||
		container.Resources.Limits.Cpu().Cmp(expectedCPU) != 0 ||
		container.Resources.Requests.Memory().Cmp(expectedMemory) != 0 ||
		container.Resources.Limits.Memory().Cmp(expectedMemory) != 0 {
		t.Fatalf("Pod %s получил неверные ресурсы: %+v", pod.Name, container.Resources)
	}
	operatorPassword := h.servicePasswords(t, instance)[0]
	output, err := execInPod(t, h.adminREST, pod.Namespace, pod.Name, []string{
		"valkey-cli", "--user", "operator", "--pass", operatorPassword,
		"--no-auth-warning", "--raw", "CONFIG", "GET", "maxmemory",
	})
	expectedMaxmemory := int64(ram) * 1024 * 1024 * 1024 * 3 / 4
	if err != nil || !strings.HasSuffix(strings.TrimSpace(output), strconv.FormatInt(expectedMaxmemory, 10)) {
		t.Fatalf("maxmemory Pod %s не совпал с %d: output=%q error=%v", pod.Name, expectedMaxmemory, output, err)
	}
}
