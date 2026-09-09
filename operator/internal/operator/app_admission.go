package operator

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

const conditionTypePublicReady = "PublicReady"

var errAppAdmissionIncomplete = errors.New("условия допуска app не выполнены")

type AppAccessUpdater func(
	context.Context,
	string,
	string,
	string,
	bool,
) (operatorvalkey.ProcessState, error)

func (r *ValkeyInstanceReconciler) reconcilePrimaryLabel(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Spec.Slug + "-0"}
	if err := r.Get(ctx, key, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("прочитать Pod для primary Service: %w", err)
	}
	primary := currentPrimaryStatus(instance, pod)
	hasPrimaryLabel := pod.Labels[applicationRoleLabel] == string(valkeyv1alpha1.NodeRolePrimary)
	if hasPrimaryLabel == primary {
		return ctrl.Result{}, nil
	}

	before := pod.DeepCopy()
	pod.Labels = maps.Clone(pod.Labels)
	if primary {
		if pod.Labels == nil {
			pod.Labels = make(map[string]string, 1)
		}
		pod.Labels[applicationRoleLabel] = string(valkeyv1alpha1.NodeRolePrimary)
	} else {
		delete(pod.Labels, applicationRoleLabel)
	}
	if err := r.Patch(ctx, pod, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, fmt.Errorf("обновить роль Pod: %w", err)
	}

	return requeueIf(true), nil
}

func currentPrimaryStatus(
	instance *valkeyv1alpha1.ValkeyInstance,
	pod *corev1.Pod,
) bool {
	if len(instance.Status.Nodes) != 1 || instance.Status.Nodes[0].Termination != nil {
		return false
	}
	container := valkeyContainerStatus(pod.Status.ContainerStatuses)
	if container == nil {
		return false
	}
	node := instance.Status.Nodes[0]

	return node.Role == valkeyv1alpha1.NodeRolePrimary &&
		node.PodUID == string(pod.UID) &&
		node.ContainerID == container.ContainerID
}

func (r *ValkeyInstanceReconciler) reconcileAppAdmission(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	pod, node, err := r.admissionProcess(ctx, instance)
	if err != nil {
		return r.admissionPending(ctx, instance, "PrimaryNotReady", err.Error())
	}
	ready, err := r.primaryEndpointReady(ctx, instance, pod)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return r.admissionPending(
			ctx,
			instance,
			"EndpointSliceNotReady",
			"primary Service ещё не указывает только на текущий Pod",
		)
	}

	credentials, appHash, err := r.processCredentials(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	update := r.UpdateAppAccess
	if update == nil {
		update = r.updateAppAccess
	}
	state, err := update(
		ctx,
		net.JoinHostPort(pod.Status.PodIP, "6379"),
		string(credentials.OperatorPassword),
		appHash,
		true,
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	if state.Role != string(valkeyv1alpha1.NodeRolePrimary) ||
		state.RunID != node.RunID || !state.AppEnabled ||
		!appACLMatches(state.AppPasswordHashes, appHash) {
		return r.admissionPending(
			ctx,
			instance,
			"AppAccessNotReady",
			"роль процесса или ACL app не подтверждены после включения",
		)
	}

	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		ordinal := int32(0)
		status.Initialized = true
		status.Phase = valkeyv1alpha1.InstancePhaseRunning
		status.Reason = ""
		status.PrimaryOrdinal = &ordinal
		status.PrimaryPodUID = node.PodUID
		status.PrimaryContainerID = node.ContainerID
		status.ObservedGeneration = instance.Status.AcceptedConfiguration.DesiredGeneration
		status.AppliedPasswordVersion = instance.Status.AcceptedConfiguration.PasswordVersion
		status.Applied = &valkeyv1alpha1.AppliedConfiguration{
			Mode:  instance.Status.AcceptedConfiguration.Mode,
			VCPU:  instance.Status.AcceptedConfiguration.VCPU,
			RAMGB: instance.Status.AcceptedConfiguration.RAMGB,
		}
		now := metav1.NewTime(r.now())
		status.ObservedAt = &now
		setCondition(
			instance,
			status,
			conditionTypePublicReady,
			metav1.ConditionTrue,
			"Admitted",
			"текущий primary доступен через подтверждённый публичный маршрут",
		)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	return requeueIf(changed), nil
}

func (r *ValkeyInstanceReconciler) admissionProcess(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (*corev1.Pod, valkeyv1alpha1.NodeStatus, error) {
	if len(instance.Status.Nodes) != 1 {
		return nil, valkeyv1alpha1.NodeStatus{}, errAppAdmissionIncomplete
	}
	node := instance.Status.Nodes[0]
	if node.Role != valkeyv1alpha1.NodeRolePrimary || !node.Readiness || node.Termination != nil {
		return nil, valkeyv1alpha1.NodeStatus{}, errAppAdmissionIncomplete
	}
	pod := &corev1.Pod{}
	key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Spec.Slug + "-0"}
	if err := r.Get(ctx, key, pod); err != nil {
		return nil, valkeyv1alpha1.NodeStatus{}, fmt.Errorf("прочитать Pod допуска app: %w", err)
	}
	container := valkeyContainerStatus(pod.Status.ContainerStatuses)
	if pod.Status.PodIP == "" || container == nil || container.State.Running == nil ||
		node.PodUID != string(pod.UID) || node.ContainerID != container.ContainerID ||
		pod.Labels[applicationRoleLabel] != string(valkeyv1alpha1.NodeRolePrimary) {
		return nil, valkeyv1alpha1.NodeStatus{}, errAppAdmissionIncomplete
	}

	return pod, node, nil
}

func (r *ValkeyInstanceReconciler) primaryEndpointReady(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	pod *corev1.Pod,
) (bool, error) {
	endpointSlices := &discoveryv1.EndpointSliceList{}
	if err := r.List(
		ctx,
		endpointSlices,
		client.InNamespace(instance.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: instance.Spec.Slug + "-primary"},
	); err != nil {
		return false, fmt.Errorf("прочитать EndpointSlice primary Service: %w", err)
	}
	readyEndpoints := 0
	for index := range endpointSlices.Items {
		endpointSlice := &endpointSlices.Items[index]
		if !endpointSliceHasValkeyPort(endpointSlice.Ports) {
			continue
		}
		for _, endpoint := range endpointSlice.Endpoints {
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue
			}
			readyEndpoints++
			if endpoint.TargetRef == nil || endpoint.TargetRef.Kind != "Pod" ||
				endpoint.TargetRef.Namespace != pod.Namespace || endpoint.TargetRef.Name != pod.Name ||
				endpoint.TargetRef.UID != pod.UID ||
				!slices.Equal(endpoint.Addresses, []string{pod.Status.PodIP}) {
				return false, nil
			}
		}
	}

	return readyEndpoints == 1, nil
}

func endpointSliceHasValkeyPort(ports []discoveryv1.EndpointPort) bool {
	for _, port := range ports {
		if port.Port != nil && *port.Port == valkeyPort &&
			(port.Protocol == nil || *port.Protocol == corev1.ProtocolTCP) {
			return true
		}
	}

	return false
}

func (r *ValkeyInstanceReconciler) processCredentials(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (*valkeyv1alpha1.ServiceCredentials, string, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{
		Namespace: instance.Namespace,
		Name:      valkeyv1alpha1.AuthSecretName(instance.Spec.Slug),
	}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, "", fmt.Errorf("прочитать Secret допуска app: %w", err)
	}
	credentials, err := valkeyv1alpha1.ParseServiceCredentials(secret.Data)
	if err != nil {
		return nil, "", err
	}
	if credentials == nil {
		return nil, "", ErrServiceCredentialsMissing
	}
	appHash, err := valkeyv1alpha1.ParseAppPasswordHash(
		secret.Data,
		instance.Status.AcceptedConfiguration.PasswordVersion,
	)
	if err != nil {
		return nil, "", err
	}

	return credentials, appHash, nil
}

func (r *ValkeyInstanceReconciler) updateAppAccess(
	ctx context.Context,
	address string,
	operatorPassword string,
	appHash string,
	enabled bool,
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

	state, err := session.TakeControl(ctx, operatorPassword)
	if err != nil {
		return operatorvalkey.ProcessState{}, err
	}
	if enabled && state.Role != string(valkeyv1alpha1.NodeRolePrimary) {
		return state, errAppAdmissionIncomplete
	}
	if state.AppEnabled == enabled && appACLMatches(state.AppPasswordHashes, appHash) {
		if !enabled {
			if err := session.KillAppClients(ctx); err != nil {
				return operatorvalkey.ProcessState{}, err
			}
		}
		return state, nil
	}
	if err := session.SetAppUser(ctx, enabled, appHash); err != nil {
		return operatorvalkey.ProcessState{}, err
	}
	if !enabled {
		if err := session.KillAppClients(ctx); err != nil {
			return operatorvalkey.ProcessState{}, err
		}
	}
	state, err = session.TakeControl(ctx, operatorPassword)
	if err != nil {
		return operatorvalkey.ProcessState{}, err
	}
	if state.AppEnabled != enabled || !appACLMatches(state.AppPasswordHashes, appHash) {
		return state, errAppAdmissionIncomplete
	}

	return state, nil
}

func appACLMatches(hashes []string, expected string) bool {
	return len(hashes) == 1 && hashes[0] == expected
}

func (r *ValkeyInstanceReconciler) admissionPending(
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
			conditionTypePublicReady,
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

func endpointSliceInstanceRequests(_ context.Context, object client.Object) []reconcile.Request {
	serviceName := object.GetLabels()[discoveryv1.LabelServiceName]
	slug, found := strings.CutSuffix(serviceName, "-primary")
	if !found || slug == "" {
		return nil
	}

	return []reconcile.Request{newReconcileRequest(object.GetNamespace(), slug)}
}
