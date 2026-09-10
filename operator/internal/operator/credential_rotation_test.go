package operator

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func TestPW08RotationPreservesFencedProcessState(t *testing.T) {
	ctx := context.Background()
	instance, pod, node, secret := processObservationObjects()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.PasswordVersion = 2
	instance.Status.CredentialRotation = &valkeyv1alpha1.CredentialRotationStatus{
		TargetVersion: 2, PreviousVersion: 1,
		Stage: valkeyv1alpha1.CredentialRotationStageUpdatingReplicas,
	}
	process := testObservedNode(pod)
	process.Role = valkeyv1alpha1.NodeRoleReplica
	process.AppEnabled = false
	process.AppPasswordVersion = 1
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{process}
	secret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"2"] = []byte(strings.Repeat("cd", 32))
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}, &corev1.Pod{}).
		WithObjects(instance, pod, node, secret).
		Build()
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s, APIReader: k8s,
		RotatePassword: func(
			_ context.Context,
			_ string,
			_ string,
			hash string,
		) (operatorvalkey.ProcessState, error) {
			return operatorvalkey.ProcessState{
				Role: "replica", RunID: process.RunID, AppEnabled: false,
				AppPasswordHashes: []string{hash},
			}, nil
		},
	}

	progress, err := reconciler.rotatePasswordForRole(ctx, instance, false)
	if err != nil || progress.complete || progress.result.IsZero() {
		t.Fatalf("применить пароль к реплике: progress=%+v error=%v", progress, err)
	}
	if len(instance.Status.CredentialRotation.Confirmations) != 1 ||
		instance.Status.CredentialRotation.Confirmations[0].Version != 2 ||
		instance.Status.Nodes[0].AppEnabled || instance.Status.Nodes[0].AppPasswordVersion != 2 {
		t.Fatalf("неверное подтверждение ротации: %+v", instance.Status)
	}
}

func TestPW05ConfirmationDoesNotTransferToReplacement(t *testing.T) {
	_, pod, _, _ := processObservationObjects()
	previous := testObservedNode(pod)
	rotation := &valkeyv1alpha1.CredentialRotationStatus{
		TargetVersion: 2,
		Confirmations: []valkeyv1alpha1.CredentialRotationConfirmation{{
			Process: processIdentity(previous), Version: 2,
		}},
	}
	replacement := previous
	replacement.PodUID = "replacement-pod"
	replacement.ContainerID = "containerd://replacement-container"
	replacement.RunID = "replacement-run"

	if rotationConfirmed(rotation, replacement) {
		t.Fatal("подтверждение прежней инкарнации применилось к замене")
	}
	if !rotationConfirmed(rotation, previous) {
		t.Fatal("точное подтверждение прежней инкарнации потеряно")
	}
}

func TestPW06LiveUnavailableProcessBlocksRotation(t *testing.T) {
	instance, pod, _, _ := processObservationObjects()
	process := testObservedNode(pod)
	process.Role = valkeyv1alpha1.NodeRoleReplica
	process.Observation = &valkeyv1alpha1.ProcessObservationStatus{
		Kind: valkeyv1alpha1.ProcessObservationTransportError, ObservedAt: metav1.Now(),
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{process}
	instance.Status.CredentialRotation = &valkeyv1alpha1.CredentialRotationStatus{
		TargetVersion: 2, PreviousVersion: 1,
		Stage: valkeyv1alpha1.CredentialRotationStageUpdatingReplicas,
	}

	progress, err := (&ValkeyInstanceReconciler{}).rotatePasswordForRole(
		context.Background(),
		instance,
		false,
	)
	if err != nil || progress.complete || !progress.blocked || !progress.result.IsZero() {
		t.Fatalf("живой недоступный процесс не удержал ротацию: progress=%+v error=%v", progress, err)
	}
}

func TestPW06UnavailableReplicaDoesNotBlockReachableReplica(t *testing.T) {
	ctx := context.Background()
	instance, pod, _, secret := processObservationObjects()
	instance.Status.AcceptedConfiguration.Mode = valkeyv1alpha1.ValkeyModeHA
	instance.Status.AcceptedConfiguration.PasswordVersion = 2
	instance.Status.CredentialRotation = &valkeyv1alpha1.CredentialRotationStatus{
		TargetVersion: 2, PreviousVersion: 1,
		Stage: valkeyv1alpha1.CredentialRotationStageUpdatingReplicas,
	}
	blocked := testObservedNode(pod)
	blocked.Ordinal = 1
	blocked.Role = valkeyv1alpha1.NodeRoleReplica
	blocked.Observation = &valkeyv1alpha1.ProcessObservationStatus{
		Kind: valkeyv1alpha1.ProcessObservationTransportError, ObservedAt: metav1.Now(),
	}
	reachablePod := pod.DeepCopy()
	reachablePod.Name = instance.Name + "-2"
	reachablePod.UID = "pod-2"
	reachablePod.Spec.NodeName = "worker-2"
	reachablePod.Status.PodIP = "10.42.0.12"
	reachablePod.Status.ContainerStatuses[0].ContainerID = "containerd://2"
	reachableNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-2", UID: "node-2"}}
	reachable := testObservedNode(reachablePod)
	reachable.Ordinal = 2
	reachable.Role = valkeyv1alpha1.NodeRoleReplica
	reachable.NodeUID = string(reachableNode.UID)
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{blocked, reachable}
	secret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"2"] = []byte(strings.Repeat("cd", 32))
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}, &corev1.Pod{}).
		WithObjects(instance, reachablePod, reachableNode, secret).
		Build()
	reconciler := &ValkeyInstanceReconciler{
		Client: k8s, APIReader: k8s,
		RotatePassword: func(
			_ context.Context,
			_ string,
			_ string,
			hash string,
		) (operatorvalkey.ProcessState, error) {
			return operatorvalkey.ProcessState{
				Role: "replica", RunID: reachable.RunID, AppEnabled: false,
				AppPasswordHashes: []string{hash},
			}, nil
		},
	}

	progress, err := reconciler.rotatePasswordForRole(ctx, instance, false)
	if err != nil || progress.complete || !progress.blocked || progress.result.IsZero() {
		t.Fatalf("обойти недоступную реплику: progress=%+v error=%v", progress, err)
	}
	if !rotationConfirmed(instance.Status.CredentialRotation, reachable) ||
		rotationConfirmed(instance.Status.CredentialRotation, blocked) {
		t.Fatalf("подтверждения процессов неверны: %+v", instance.Status.CredentialRotation.Confirmations)
	}
}

func TestPW07TerminatedAndNotStartedProcessesDoNotBlockRotation(t *testing.T) {
	instance, pod, _, _ := processObservationObjects()
	process := testObservedNode(pod)
	process.Role = valkeyv1alpha1.NodeRoleReplica
	process.Termination = &valkeyv1alpha1.ProcessTermination{
		Reason: "NodeDeleted", ExitCode: 137, FinishedAt: metav1.Now(), Evidence: "node_deleted",
	}
	instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{process}
	instance.Status.CredentialRotation = &valkeyv1alpha1.CredentialRotationStatus{
		TargetVersion: 2, PreviousVersion: 1,
		Stage: valkeyv1alpha1.CredentialRotationStageUpdatingReplicas,
	}

	progress, err := (&ValkeyInstanceReconciler{}).rotatePasswordForRole(
		context.Background(),
		instance,
		false,
	)
	if err != nil || !progress.complete || progress.blocked || !progress.result.IsZero() {
		t.Fatalf(
			"доказанно остановленный процесс задержал ротацию: progress=%+v error=%v",
			progress,
			err,
		)
	}
}

func TestCT08PasswordCleanupPreservesFutureHashAndServiceData(t *testing.T) {
	ctx := context.Background()
	instance, _, _, secret := processObservationObjects()
	instance.Status.AcceptedConfiguration.PasswordVersion = 2
	instance.Status.AppliedPasswordVersion = 1
	instance.Status.CredentialRotation = &valkeyv1alpha1.CredentialRotationStatus{
		TargetVersion: 2, PreviousVersion: 1,
		Stage: valkeyv1alpha1.CredentialRotationStageCleaningSecret,
	}
	secret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"2"] = []byte(strings.Repeat("cd", 32))
	secret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"3"] = []byte(strings.Repeat("ef", 32))
	operatorPassword := string(secret.Data[valkeyv1alpha1.OperatorPasswordKey])
	k8s := fake.NewClientBuilder().
		WithScheme(NewScheme()).
		WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
		WithObjects(instance, secret).
		Build()
	reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s}

	if result, err := reconciler.finishCredentialRotation(ctx, instance); err != nil || result.IsZero() {
		t.Fatalf("очистить прежний хеш: result=%+v error=%v", result, err)
	}
	observedSecret := &corev1.Secret{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(secret), observedSecret); err != nil {
		t.Fatalf("прочитать Secret: %v", err)
	}
	if _, found := observedSecret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"1"]; found ||
		string(observedSecret.Data[valkeyv1alpha1.AppPasswordHashKeyPrefix+"3"]) != strings.Repeat("ef", 32) ||
		string(observedSecret.Data[valkeyv1alpha1.OperatorPasswordKey]) != operatorPassword {
		t.Fatalf("очистка повредила Secret: %v", observedSecret.Data)
	}
	if result, err := reconciler.finishCredentialRotation(ctx, instance); err != nil || result.IsZero() {
		t.Fatalf("подтвердить версию: result=%+v error=%v", result, err)
	}
	if instance.Status.CredentialRotation != nil || instance.Status.AppliedPasswordVersion != 2 {
		t.Fatalf("ротация не завершена: %+v", instance.Status)
	}
}
