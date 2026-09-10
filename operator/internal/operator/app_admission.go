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

func (r *ValkeyInstanceReconciler) reconcilePodMetadata(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	for ordinal := range expectedProcessCount(instance) {
		pod := &corev1.Pod{}
		key := client.ObjectKey{
			Namespace: instance.Namespace,
			Name:      fmt.Sprintf("%s-%d", instance.Status.AcceptedConfiguration.Slug, ordinal),
		}
		if err := r.Get(ctx, key, pod); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return ctrl.Result{}, fmt.Errorf("прочитать Pod для primary Service: %w", err)
		}
		before := pod.DeepCopy()
		if !setPodMetadata(instance, pod, ordinal) {
			continue
		}
		if err := r.Patch(ctx, pod, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, fmt.Errorf("обновить метаданные Pod: %w", err)
		}

		return requeueIf(true), nil
	}

	return ctrl.Result{}, nil
}

func setPodMetadata(
	instance *valkeyv1alpha1.ValkeyInstance,
	pod *corev1.Pod,
	ordinal int32,
) bool {
	changed := false
	labels := maps.Clone(pod.Labels)
	if labels == nil {
		labels = make(map[string]string)
	}
	for key, value := range workloadResourceLabels(instance) {
		if labels[key] != value {
			labels[key] = value
			changed = true
		}
	}
	desiredRole := currentProcessRoleLabel(instance, pod, ordinal)
	for _, key := range []string{applicationRoleLabel, valkeyv1alpha1.RoleLabelKey} {
		if desiredRole == "" {
			if _, found := labels[key]; found {
				delete(labels, key)
				changed = true
			}
		} else if labels[key] != desiredRole {
			labels[key] = desiredRole
			changed = true
		}
	}
	if changed {
		pod.Labels = labels
	}

	email := instance.Annotations[valkeyv1alpha1.UserEmailAnnotationKey]
	if email != "" && pod.Annotations[valkeyv1alpha1.UserEmailAnnotationKey] != email {
		annotations := maps.Clone(pod.Annotations)
		if annotations == nil {
			annotations = make(map[string]string, 1)
		}
		annotations[valkeyv1alpha1.UserEmailAnnotationKey] = email
		pod.Annotations = annotations
		changed = true
	}

	return changed
}

func currentProcessRoleLabel(
	instance *valkeyv1alpha1.ValkeyInstance,
	pod *corev1.Pod,
	ordinal int32,
) string {
	if currentPrimaryStatus(instance, pod, ordinal) {
		return string(valkeyv1alpha1.NodeRolePrimary)
	}
	if instance.Status.AcceptedConfiguration == nil ||
		instance.Status.AcceptedConfiguration.Mode != valkeyv1alpha1.ValkeyModeHA ||
		hasUnterminatedPreviousProcess(instance.Status.PreviousProcesses) {
		return ""
	}
	node, found := nodeStatusAtOrdinal(instance.Status.Nodes, ordinal)
	container := valkeyContainerStatus(pod.Status.ContainerStatuses)
	if !found || container == nil || node.Role != valkeyv1alpha1.NodeRoleReplica ||
		node.Termination != nil || node.Observation != nil || !node.Readiness ||
		node.Replication == nil || !node.Replication.LinkUp || node.Replication.SyncInProgress ||
		node.Replication.SyncedAt == nil ||
		!node.AppEnabled ||
		node.AppPasswordVersion != instance.Status.AcceptedConfiguration.PasswordVersion ||
		node.PodUID != string(pod.UID) || node.ContainerID != container.ContainerID {
		return ""
	}

	return string(valkeyv1alpha1.NodeRoleReplica)
}

func currentPrimaryStatus(
	instance *valkeyv1alpha1.ValkeyInstance,
	pod *corev1.Pod,
	ordinal int32,
) bool {
	primaryOrdinal, selected := selectedPrimaryOrdinal(instance)
	if !selected || primaryOrdinal != ordinal || hasUnterminatedPreviousProcess(instance.Status.PreviousProcesses) {
		return false
	}
	container := valkeyContainerStatus(pod.Status.ContainerStatuses)
	if container == nil {
		return false
	}
	node, found := nodeStatusAtOrdinal(instance.Status.Nodes, ordinal)
	if !found || node.Termination != nil {
		return false
	}

	return node.Role == valkeyv1alpha1.NodeRolePrimary &&
		node.PodUID == string(pod.UID) &&
		node.ContainerID == container.ContainerID &&
		primaryIdentityMatches(instance, node)
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
	if node.AppEnabled &&
		node.AppPasswordVersion == instance.Status.AcceptedConfiguration.PasswordVersion {
		return r.finishAppAdmission(ctx, instance, node)
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

	return r.finishAppAdmission(ctx, instance, node)
}

func (r *ValkeyInstanceReconciler) finishAppAdmission(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	node valkeyv1alpha1.NodeStatus,
) (ctrl.Result, error) {
	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		ordinal := node.Ordinal
		firstAdmission := !status.Initialized
		status.Initialized = true
		status.PrimaryOrdinal = &ordinal
		status.PrimaryPodUID = node.PodUID
		status.PrimaryContainerID = node.ContainerID
		status.PrimaryRunID = node.RunID
		status.PrimaryNodeName = node.NodeName
		status.PrimaryNodeUID = node.NodeUID
		for index := range status.Nodes {
			if sameProcess(status.Nodes[index], node) {
				status.Nodes[index].AppEnabled = true
				status.Nodes[index].AppPasswordVersion = status.AcceptedConfiguration.PasswordVersion
			}
		}
		if firstAdmission {
			status.AppliedPasswordVersion = instance.Status.AcceptedConfiguration.PasswordVersion
			status.Applied = &valkeyv1alpha1.AppliedConfiguration{
				Mode:  instance.Status.AcceptedConfiguration.Mode,
				VCPU:  instance.Status.AcceptedConfiguration.VCPU,
				RAMGB: instance.Status.AcceptedConfiguration.RAMGB,
			}
			now := metav1.NewTime(r.now())
			status.ObservedAt = &now
		}
		setCondition(
			instance,
			status,
			conditionTypePublicReady,
			metav1.ConditionTrue,
			"Admitted",
			"текущий primary доступен через подтверждённый публичный маршрут",
		)
		applyOperationalPhase(instance, status)
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	return requeueIf(changed), nil
}

func (r *ValkeyInstanceReconciler) reconcileReplicaAdmission(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	if !instance.Status.Initialized ||
		instance.Status.AcceptedConfiguration.Mode != valkeyv1alpha1.ValkeyModeHA ||
		instance.Status.PrimaryOrdinal == nil || instance.Status.CredentialRotation != nil {
		return ctrl.Result{}, nil
	}
	primaryPod := &corev1.Pod{}
	primaryKey := client.ObjectKey{
		Namespace: instance.Namespace,
		Name: fmt.Sprintf(
			"%s-%d",
			instance.Status.AcceptedConfiguration.Slug,
			*instance.Status.PrimaryOrdinal,
		),
	}
	if err := r.Get(ctx, primaryKey, primaryPod); err != nil {
		return ctrl.Result{}, fmt.Errorf("прочитать primary для допуска реплик: %w", err)
	}
	primary, found := nodeStatusAtOrdinal(instance.Status.Nodes, *instance.Status.PrimaryOrdinal)
	primaryContainer := valkeyContainerStatus(primaryPod.Status.ContainerStatuses)
	if !found || primaryContainer == nil || primaryContainer.State.Running == nil ||
		primary.Role != valkeyv1alpha1.NodeRolePrimary || !primary.Readiness ||
		primary.PodUID != string(primaryPod.UID) || primary.ContainerID != primaryContainer.ContainerID ||
		!primaryIdentityMatches(instance, primary) {
		return ctrl.Result{}, nil
	}
	credentials, appHash, err := r.processCredentials(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	for _, node := range instance.Status.Nodes {
		if node.Ordinal == *instance.Status.PrimaryOrdinal || node.Role != valkeyv1alpha1.NodeRoleReplica ||
			!node.Readiness || node.Termination != nil || node.Observation != nil ||
			node.Replication == nil || !node.Replication.LinkUp || node.Replication.SyncInProgress ||
			node.Replication.SyncedAt == nil || node.Replication.UpstreamHost != primaryPod.Status.PodIP ||
			node.Replication.UpstreamPort != valkeyPort ||
			node.AppEnabled && node.AppPasswordVersion == instance.Status.AcceptedConfiguration.PasswordVersion {
			continue
		}
		pod := &corev1.Pod{}
		key := client.ObjectKey{
			Namespace: instance.Namespace,
			Name:      fmt.Sprintf("%s-%d", instance.Status.AcceptedConfiguration.Slug, node.Ordinal),
		}
		if err := r.Get(ctx, key, pod); err != nil {
			return ctrl.Result{}, fmt.Errorf("прочитать Pod допуска реплики: %w", err)
		}
		container := valkeyContainerStatus(pod.Status.ContainerStatuses)
		if pod.Status.PodIP == "" || container == nil || container.State.Running == nil ||
			node.PodUID != string(pod.UID) || node.ContainerID != container.ContainerID {
			continue
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
		if state.Role != string(valkeyv1alpha1.NodeRoleReplica) || state.RunID != node.RunID ||
			!state.AppEnabled || !appACLMatches(state.AppPasswordHashes, appHash) ||
			state.Replication == nil || !state.Replication.LinkUp || state.Replication.SyncInProgress ||
			state.Replication.UpstreamHost != primaryPod.Status.PodIP ||
			state.Replication.UpstreamPort != valkeyPort {
			return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
		}
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			for index := range status.Nodes {
				if sameProcess(status.Nodes[index], node) {
					status.Nodes[index].AppEnabled = true
					status.Nodes[index].AppPasswordVersion = status.AcceptedConfiguration.PasswordVersion
				}
			}
			applyOperationalPhase(instance, status)
		})
		if err != nil {
			return ctrl.Result{}, err
		}

		return requeueIf(changed), nil
	}

	return ctrl.Result{}, nil
}

func (r *ValkeyInstanceReconciler) admissionProcess(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (*corev1.Pod, valkeyv1alpha1.NodeStatus, error) {
	ordinal, selected := selectedPrimaryOrdinal(instance)
	if !selected || hasUnterminatedPreviousProcess(instance.Status.PreviousProcesses) {
		return nil, valkeyv1alpha1.NodeStatus{}, errAppAdmissionIncomplete
	}
	node, found := nodeStatusAtOrdinal(instance.Status.Nodes, ordinal)
	if !found {
		return nil, valkeyv1alpha1.NodeStatus{}, errAppAdmissionIncomplete
	}
	if node.Role != valkeyv1alpha1.NodeRolePrimary || !node.Readiness || node.Termination != nil {
		return nil, valkeyv1alpha1.NodeStatus{}, errAppAdmissionIncomplete
	}
	pod := &corev1.Pod{}
	key := client.ObjectKey{
		Namespace: instance.Namespace,
		Name:      fmt.Sprintf("%s-%d", instance.Status.AcceptedConfiguration.Slug, ordinal),
	}
	if err := r.Get(ctx, key, pod); err != nil {
		return nil, valkeyv1alpha1.NodeStatus{}, fmt.Errorf("прочитать Pod допуска app: %w", err)
	}
	container := valkeyContainerStatus(pod.Status.ContainerStatuses)
	if pod.Status.PodIP == "" || container == nil || container.State.Running == nil ||
		node.PodUID != string(pod.UID) || node.ContainerID != container.ContainerID ||
		!primaryIdentityMatches(instance, node) ||
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
	connection, err := r.dialValkey(ctx, cfg)
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
	if state.AppEnabled == enabled && appACLMatches(state.AppPasswordHashes, appHash) {
		if !enabled {
			if err := session.KillAppClients(ctx); err != nil {
				return operatorvalkey.ProcessState{}, err
			}
		}
		return state, nil
	}
	if enabled && !appACLMatches(state.AppPasswordHashes, appHash) {
		if err := session.SetAppUser(ctx, false, appHash); err != nil {
			return operatorvalkey.ProcessState{}, err
		}
		if err := session.KillAppClients(ctx); err != nil {
			return operatorvalkey.ProcessState{}, err
		}
		state, err = session.TakeControl(ctx, operatorPassword)
		if err != nil {
			return operatorvalkey.ProcessState{}, err
		}
		if state.AppEnabled || !appACLMatches(state.AppPasswordHashes, appHash) {
			return state, errAppAdmissionIncomplete
		}
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
	if err := runAppAccessActionControl(ctx, address, enabled); err != nil {
		return state, err
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
	for _, suffix := range []string{"-primary", "-replicas"} {
		slug, found := strings.CutSuffix(serviceName, suffix)
		if found && slug != "" {
			return []reconcile.Request{newReconcileRequest(object.GetNamespace(), slug)}
		}
	}

	return nil
}
