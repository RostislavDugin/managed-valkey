package operator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func TestCT06ManualFencingRequiresExactExpectedIdentityAndStoppedNode(t *testing.T) {
	tests := []struct {
		name      string
		identity  func(valkeyv1alpha1.ProcessIdentity) valkeyv1alpha1.ProcessIdentity
		nodeReady corev1.ConditionStatus
		wantProof bool
		wantEvent string
	}{
		{
			name: "exact stopped", identity: sameTestIdentity, nodeReady: corev1.ConditionFalse,
			wantProof: true, wantEvent: "ManualFencingAccepted",
		},
		{name: "stale pod", identity: differentTestPod, nodeReady: corev1.ConditionFalse},
		{name: "stale container", identity: differentTestContainer, nodeReady: corev1.ConditionFalse},
		{name: "stale run", identity: differentTestRun, nodeReady: corev1.ConditionFalse},
		{name: "stale node", identity: differentTestNode, nodeReady: corev1.ConditionFalse},
		{
			name: "node still ready", identity: sameTestIdentity, nodeReady: corev1.ConditionTrue,
			wantEvent: "ManualFencingPending",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			instance, pod, node, _ := processObservationObjects()
			process := testObservedNode(pod)
			process.Recovery = &valkeyv1alpha1.ProcessRecoveryStatus{
				Reason:    valkeyv1alpha1.ProcessRecoveryUnresponsive,
				Stage:     valkeyv1alpha1.ProcessRecoveryStageWaitingForTermination,
				StartedAt: metav1.Now(),
			}
			instance.Status.Nodes = []valkeyv1alpha1.NodeStatus{process}
			identity := test.identity(processIdentity(process))
			encoded, err := json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			instance.Annotations = map[string]string{manualFencingAnnotation: string(encoded)}
			node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: test.nodeReady}}
			k8s := fake.NewClientBuilder().
				WithScheme(NewScheme()).
				WithStatusSubresource(&valkeyv1alpha1.ValkeyInstance{}).
				WithObjects(instance, node).
				Build()
			recorder := events.NewFakeRecorder(1)
			reconciler := &ValkeyInstanceReconciler{Client: k8s, APIReader: k8s, Recorder: recorder}

			if _, err := reconciler.reconcileManualFencing(ctx, instance); err != nil {
				t.Fatalf("обработать ручное fencing: %v", err)
			}
			proof := instance.Status.Nodes[0].Termination
			if test.wantProof && (proof == nil || proof.Evidence != "manual_fencing") {
				t.Fatalf("точное подтверждение не принято: %+v", proof)
			}
			if !test.wantProof && proof != nil {
				t.Fatalf("недопустимое подтверждение принято: %+v", proof)
			}
			if test.wantEvent == "" {
				if len(recorder.Events) != 0 {
					t.Fatalf("устаревшее подтверждение создало Event: %q", <-recorder.Events)
				}
			} else {
				select {
				case event := <-recorder.Events:
					if !strings.Contains(event, test.wantEvent) {
						t.Fatalf("неверный Event ручного fencing: %q", event)
					}
				default:
					t.Fatalf("не создан Event %s", test.wantEvent)
				}
			}
			condition := apimeta.FindStatusCondition(instance.Status.Conditions, conditionTypeManualFencing)
			if test.nodeReady == corev1.ConditionTrue &&
				test.identity(processIdentity(process)).PodUID == process.PodUID {
				if condition == nil || condition.Status != metav1.ConditionFalse ||
					condition.Reason != "NodeStillReady" {
					t.Fatalf("ожидание ручного fencing не отражено в condition: %+v", condition)
				}
			}
		})
	}
}

func sameTestIdentity(identity valkeyv1alpha1.ProcessIdentity) valkeyv1alpha1.ProcessIdentity {
	return identity
}

func differentTestPod(identity valkeyv1alpha1.ProcessIdentity) valkeyv1alpha1.ProcessIdentity {
	identity.PodUID = "old-pod"
	return identity
}

func differentTestContainer(identity valkeyv1alpha1.ProcessIdentity) valkeyv1alpha1.ProcessIdentity {
	identity.ContainerID = "containerd://old-container"
	return identity
}

func differentTestRun(identity valkeyv1alpha1.ProcessIdentity) valkeyv1alpha1.ProcessIdentity {
	identity.RunID = "old-run"
	return identity
}

func differentTestNode(identity valkeyv1alpha1.ProcessIdentity) valkeyv1alpha1.ProcessIdentity {
	identity.NodeUID = "old-node"
	return identity
}
