package operator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

const conditionTypeProcessReady = "ProcessReady"

var ErrServiceCredentialsMissing = errors.New("служебные учётные данные отсутствуют")

type ProcessInspector func(
	context.Context,
	string,
	string,
	string,
) (operatorvalkey.ProcessState, error)

func (r *ValkeyInstanceReconciler) reconcileProcess(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	pod := &corev1.Pod{}
	podKey := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Spec.Slug + "-0"}
	if err := r.Get(ctx, podKey, pod); err != nil {
		if apierrors.IsNotFound(err) {
			if len(instance.Status.Nodes) > 0 && instance.Status.Nodes[0].Termination == nil {
				deleted, checkErr := r.previousNodeDeleted(ctx, instance.Status.Nodes[0])
				if checkErr != nil {
					return ctrl.Result{}, checkErr
				}
				if deleted {
					return r.saveNodeDeletionTermination(ctx, instance, instance.Status.Nodes[0])
				}
			}
			return r.processUnavailable(ctx, instance, "PodNotFound", "Pod Valkey отсутствует")
		}

		return ctrl.Result{}, fmt.Errorf("прочитать Pod Valkey: %w", err)
	}
	if !slices.Contains(pod.Finalizers, processFinalizer) {
		before := pod.DeepCopy()
		pod.Finalizers = append(pod.Finalizers, processFinalizer)
		if err := r.Patch(ctx, pod, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, fmt.Errorf("добавить finalizer Pod: %w", err)
		}

		return requeueIf(true), nil
	}

	containerStatus := valkeyContainerStatus(pod.Status.ContainerStatuses)
	if containerStatus != nil && containerStatus.State.Terminated != nil {
		return r.saveContainerTermination(ctx, instance, pod, containerStatus)
	}
	if pod.Status.PodIP == "" || pod.Spec.NodeName == "" || containerStatus == nil ||
		containerStatus.ContainerID == "" || containerStatus.State.Running == nil {
		return r.processUnavailable(ctx, instance, "ProcessNotRunning", "процесс Valkey не запущен")
	}

	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("прочитать Node процесса Valkey: %w", err)
	}

	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{
		Namespace: instance.Namespace,
		Name:      valkeyv1alpha1.AuthSecretName(instance.Spec.Slug),
	}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("прочитать Secret процесса Valkey: %w", err)
	}
	credentials, err := valkeyv1alpha1.ParseServiceCredentials(secret.Data)
	if err != nil {
		return ctrl.Result{}, err
	}
	if credentials == nil {
		return ctrl.Result{}, ErrServiceCredentialsMissing
	}
	appHash, err := valkeyv1alpha1.ParseAppPasswordHash(
		secret.Data,
		instance.Status.AcceptedConfiguration.PasswordVersion,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	inspect := r.InspectProcess
	if inspect == nil {
		inspect = r.inspectProcess
	}
	state, err := inspect(
		ctx,
		net.JoinHostPort(pod.Status.PodIP, "6379"),
		string(credentials.OperatorPassword),
		appHash,
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !instance.Status.Initialized &&
		(state.AppEnabled || !appACLMatches(state.AppPasswordHashes, appHash)) {
		update := r.UpdateAppAccess
		if update == nil {
			update = r.updateAppAccess
		}
		if _, err := update(
			ctx,
			net.JoinHostPort(pod.Status.PodIP, "6379"),
			string(credentials.OperatorPassword),
			appHash,
			false,
		); err != nil {
			return ctrl.Result{}, err
		}

		return requeueIf(true), nil
	}

	observed := valkeyv1alpha1.NodeStatus{
		Ordinal:     0,
		PodUID:      string(pod.UID),
		ContainerID: containerStatus.ContainerID,
		RunID:       state.RunID,
		NodeName:    node.Name,
		NodeUID:     string(node.UID),
		Role:        valkeyv1alpha1.NodeRole(state.Role),
		Readiness:   podReady(pod.Status.Conditions),
	}
	if len(instance.Status.Nodes) > 0 && !sameProcess(instance.Status.Nodes[0], observed) &&
		instance.Status.Nodes[0].Termination == nil {
		deleted, checkErr := r.previousNodeDeleted(ctx, instance.Status.Nodes[0])
		if checkErr != nil {
			return ctrl.Result{}, checkErr
		}
		if deleted {
			return r.saveNodeDeletionTermination(ctx, instance, instance.Status.Nodes[0])
		}

		return r.terminationProofLost(ctx, instance)
	}

	_, err = r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		status.Nodes = []valkeyv1alpha1.NodeStatus{observed}
		now := r.now()
		if heartbeatDue(status.ObservedAt, now) {
			observedAt := metav1.NewTime(now)
			status.ObservedAt = &observedAt
		}
		setCondition(
			instance,
			status,
			conditionTypeProcessReady,
			metav1.ConditionTrue,
			"Observed",
			"идентичность процесса сохранена",
		)
	})

	return ctrl.Result{}, err
}

func (r *ValkeyInstanceReconciler) processUnavailable(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	reason string,
	message string,
) (ctrl.Result, error) {
	_, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		if status.Initialized {
			status.Phase = valkeyv1alpha1.InstancePhaseUnavailable
			status.Reason = "PRIMARY_NOT_READY"
		}
		setCondition(
			instance,
			status,
			conditionTypeProcessReady,
			metav1.ConditionFalse,
			reason,
			message,
		)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
}

func heartbeatDue(previous *metav1.Time, now time.Time) bool {
	return previous == nil || now.Before(previous.Time) ||
		now.Sub(previous.Time) >= config.HealthCheckInterval
}

func (r *ValkeyInstanceReconciler) inspectProcess(
	ctx context.Context,
	address string,
	operatorPassword string,
	appHash string,
) (operatorvalkey.ProcessState, error) {
	cfg := operatorvalkey.ClientConfig{
		Address:        address,
		Username:       "operator",
		Password:       operatorPassword,
		DialTimeout:    config.ValkeyDialTimeout,
		CommandTimeout: config.ValkeyCommandTimeout,
	}
	var connection *operatorvalkey.Client
	var err error
	if r.Lease != nil {
		connection, err = r.Lease.DialValkey(ctx, cfg)
	} else {
		connection, err = operatorvalkey.Dial(ctx, cfg)
	}
	if err != nil {
		return operatorvalkey.ProcessState{}, err
	}
	defer connection.Close()

	session, err := connection.OpenSession(ctx)
	if err != nil {
		return operatorvalkey.ProcessState{}, err
	}
	defer session.Close()

	return session.TakeControl(ctx, operatorPassword)
}

func valkeyContainerStatus(statuses []corev1.ContainerStatus) *corev1.ContainerStatus {
	for index := range statuses {
		if statuses[index].Name == "valkey" {
			return &statuses[index]
		}
	}

	return nil
}

func podReady(conditions []corev1.PodCondition) bool {
	for _, condition := range conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func sameProcess(current, observed valkeyv1alpha1.NodeStatus) bool {
	return current.PodUID == observed.PodUID &&
		current.ContainerID == observed.ContainerID &&
		current.RunID == observed.RunID
}
