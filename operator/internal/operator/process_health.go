package operator

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

type processThresholdState struct {
	TransportFailure bool
	BusyIntervention bool
	Unresponsive     bool
	EmptyPrimary     bool
}

func (r *ValkeyInstanceReconciler) recordProcessObservationError(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	pod *corev1.Pod,
	node *corev1.Node,
	container *corev1.ContainerStatus,
	ordinal int32,
	cause error,
) (ctrl.Result, error) {
	kind, handled := processObservationKind(operatorvalkey.ClassifyError(cause))
	if !handled {
		return ctrl.Result{}, cause
	}

	current, found := nodeStatusAtOrdinal(instance.Status.Nodes, ordinal)
	known := found && sameKubernetesProcess(current, pod, node, container, ordinal)
	now := r.now()
	reason := processObservationReason(kind)
	_, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		primaryFailureConfirmed := kind != valkeyv1alpha1.ProcessObservationTransportError
		if known {
			for index := range status.Nodes {
				if !sameProcess(status.Nodes[index], current) {
					continue
				}
				status.Nodes[index].Readiness = podReady(pod.Status.Conditions)
				previous := status.Nodes[index].Observation
				if previous != nil && !r.ObservationStartedAt.IsZero() &&
					previous.ObservedAt.Time.Before(r.ObservationStartedAt) {
					previous = nil
				}
				status.Nodes[index].Observation = nextProcessObservation(
					previous,
					kind,
					now,
				)
				if kind == valkeyv1alpha1.ProcessObservationTransportError {
					primaryFailureConfirmed = processThresholds(
						status.Nodes[index].Observation,
						nil,
						now,
					).TransportFailure
				}
			}
		}
		if status.Initialized && processFailureAffectsPrimary(status, ordinal) && primaryFailureConfirmed {
			status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
			status.Reason = reason
		} else if status.Initialized && status.AcceptedConfiguration != nil &&
			status.AcceptedConfiguration.Mode == valkeyv1alpha1.ValkeyModeHA &&
			status.Failover == nil && status.Rollout == nil && status.CredentialRotation == nil {
			status.Phase = valkeyv1alpha1.InstancePhaseDegraded
			status.Reason = "REPLICAS_NOT_READY"
		}
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if kind == valkeyv1alpha1.ProcessObservationServerError {
		return ctrl.Result{}, cause
	}

	return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
}

func processFailureAffectsPrimary(status *valkeyv1alpha1.ValkeyInstanceStatus, ordinal int32) bool {
	return status.PrimaryOrdinal == nil || *status.PrimaryOrdinal == ordinal
}

func processObservationKind(kind operatorvalkey.ErrorKind) (valkeyv1alpha1.ProcessObservationKind, bool) {
	switch kind {
	case operatorvalkey.ErrorKindTransport:
		return valkeyv1alpha1.ProcessObservationTransportError, true
	case operatorvalkey.ErrorKindBusy:
		return valkeyv1alpha1.ProcessObservationBusy, true
	case operatorvalkey.ErrorKindLoading:
		return valkeyv1alpha1.ProcessObservationLoading, true
	case operatorvalkey.ErrorKindAuth:
		return valkeyv1alpha1.ProcessObservationAuthError, true
	case operatorvalkey.ErrorKindServer:
		return valkeyv1alpha1.ProcessObservationServerError, true
	default:
		return "", false
	}
}

func processObservationReason(kind valkeyv1alpha1.ProcessObservationKind) string {
	switch kind {
	case valkeyv1alpha1.ProcessObservationTransportError:
		return "PROCESS_UNRESPONSIVE"
	case valkeyv1alpha1.ProcessObservationBusy:
		return "SCRIPT_BUSY"
	case valkeyv1alpha1.ProcessObservationLoading:
		return "DATA_LOADING"
	case valkeyv1alpha1.ProcessObservationAuthError:
		return "AUTH_FAILED"
	default:
		return "PROCESS_RESPONSE_INVALID"
	}
}

func nextProcessObservation(
	previous *valkeyv1alpha1.ProcessObservationStatus,
	kind valkeyv1alpha1.ProcessObservationKind,
	now time.Time,
) *valkeyv1alpha1.ProcessObservationStatus {
	observedAt := metav1.NewTime(now)
	result := &valkeyv1alpha1.ProcessObservationStatus{Kind: kind, ObservedAt: observedAt}
	switch kind {
	case valkeyv1alpha1.ProcessObservationTransportError:
		result.ConsecutiveTransportErrors = 1
		result.TransportErrorSince = &observedAt
		if previous != nil && previous.Kind == kind && previous.TransportErrorSince != nil {
			result.ConsecutiveTransportErrors = previous.ConsecutiveTransportErrors + 1
			result.TransportErrorSince = previous.TransportErrorSince.DeepCopy()
		}
	case valkeyv1alpha1.ProcessObservationBusy:
		result.BusySince = &observedAt
		if previous != nil && previous.Kind == kind && previous.BusySince != nil {
			result.BusySince = previous.BusySince.DeepCopy()
		}
	}

	return result
}

func processThresholds(
	observation *valkeyv1alpha1.ProcessObservationStatus,
	emptyPrimarySince *metav1.Time,
	now time.Time,
) processThresholdState {
	state := processThresholdState{EmptyPrimary: elapsed(emptyPrimarySince, now, config.EmptyPrimaryTimeout)}
	if observation == nil {
		return state
	}

	switch observation.Kind {
	case valkeyv1alpha1.ProcessObservationTransportError:
		state.TransportFailure = observation.ConsecutiveTransportErrors >= config.PrimaryFailureThreshold &&
			elapsed(observation.TransportErrorSince, now, config.PrimaryFailureMinDuration)
		state.Unresponsive = observation.ConsecutiveTransportErrors >= config.PrimaryFailureThreshold &&
			elapsed(observation.TransportErrorSince, now, config.ProcessUnresponsiveTimeout)
	case valkeyv1alpha1.ProcessObservationBusy:
		state.BusyIntervention = elapsed(observation.BusySince, now, config.ScriptBusyTimeout)
	}

	return state
}

func elapsed(since *metav1.Time, now time.Time, duration time.Duration) bool {
	return since != nil && !now.Before(since.Time) && now.Sub(since.Time) >= duration
}

func sameKubernetesProcess(
	current valkeyv1alpha1.NodeStatus,
	pod *corev1.Pod,
	node *corev1.Node,
	container *corev1.ContainerStatus,
	ordinal int32,
) bool {
	return current.Ordinal == ordinal &&
		current.PodUID == string(pod.UID) &&
		current.ContainerID == container.ContainerID &&
		current.NodeName == node.Name &&
		current.NodeUID == string(node.UID) &&
		current.RunID != ""
}
