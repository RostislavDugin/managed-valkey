package operator

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

const (
	applicationNameLabel = "app.kubernetes.io/name"
	applicationRoleLabel = "role"
	processFinalizer     = "valkey.h3llo-demo.com/process-stopped"
	valkeyPort           = int32(6379)
	envoyNamespace       = "envoy-gateway-system"
)

func (r *ValkeyInstanceReconciler) reconcileWorkloadResources(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	workloadInstance := rolloutWorkloadInstance(instance)
	configMap, err := r.desiredConfigMap(workloadInstance)
	if err != nil {
		return ctrl.Result{}, err
	}
	changed := false
	desiredCount := rolloutWorkloadProcessCount(instance)
	if err := runRolloutActionControl(
		ctx,
		"before-workload-reconcile",
		instance,
		desiredCount,
		configMap.Name,
	); err != nil {
		return ctrl.Result{}, err
	}
	if desiredCount == 0 {
		podsChanged, err := r.requestWorkloadStop(ctx, instance)
		if err != nil {
			return ctrl.Result{}, err
		}
		changed = changed || podsChanged
	} else {
		for ordinal := range desiredCount {
			if !processCreationAllowed(instance, ordinal) {
				continue
			}
			desired := desiredPod(
				workloadInstance,
				ordinal,
				configMap.Name,
				instance.Status.ValkeyImage,
				r.ResourceRequests,
			)
			podChanged, blocked, err := r.ensurePod(ctx, instance, desired)
			if err != nil {
				return ctrl.Result{}, err
			}
			changed = changed || podChanged
			if blocked {
				if changed {
					return requeueIf(true), nil
				}
				return ctrl.Result{RequeueAfter: config.HealthCheckInterval}, nil
			}
		}
	}
	if changed {
		if err := runRolloutActionControl(
			ctx,
			"after-workload-reconcile",
			instance,
			desiredCount,
			configMap.Name,
		); err != nil {
			return ctrl.Result{}, err
		}
	}

	for _, service := range desiredServices(instance) {
		serviceChanged, err := r.ensureService(ctx, service)
		if err != nil {
			return ctrl.Result{}, err
		}
		changed = changed || serviceChanged
	}

	networkPolicyChanged, err := r.ensureNetworkPolicy(
		ctx,
		desiredNetworkPolicy(instance, r.SystemNamespace, r.OperatorCIDRs),
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	changed = changed || networkPolicyChanged
	if instance.Status.AcceptedConfiguration.Mode == valkeyv1alpha1.ValkeyModeHA {
		pdbChanged, err := r.ensurePodDisruptionBudget(ctx, desiredPodDisruptionBudget(instance))
		if err != nil {
			return ctrl.Result{}, err
		}
		changed = changed || pdbChanged
	}

	return requeueIf(changed), nil
}

func rolloutWorkloadInstance(instance *valkeyv1alpha1.ValkeyInstance) *valkeyv1alpha1.ValkeyInstance {
	workloadInstance := instance.DeepCopy()
	rollout := instance.Status.Rollout
	if rollout == nil || instance.Status.Applied == nil || rolloutUsesTargetTemplate(rollout.Stage) {
		return workloadInstance
	}
	accepted := *workloadInstance.Status.AcceptedConfiguration
	accepted.VCPU = instance.Status.Applied.VCPU
	accepted.RAMGB = instance.Status.Applied.RAMGB
	workloadInstance.Status.AcceptedConfiguration = &accepted
	return workloadInstance
}

func rolloutUsesTargetTemplate(stage valkeyv1alpha1.RolloutStage) bool {
	switch stage {
	case valkeyv1alpha1.RolloutStageUpdatingTemplate,
		valkeyv1alpha1.RolloutStageReplacingReplicas,
		valkeyv1alpha1.RolloutStageSwitchingPrimary,
		valkeyv1alpha1.RolloutStageReplacingPrimary,
		valkeyv1alpha1.RolloutStageStarting,
		valkeyv1alpha1.RolloutStageVerifying:
		return true
	default:
		return false
	}
}

func rolloutWorkloadProcessCount(instance *valkeyv1alpha1.ValkeyInstance) int32 {
	normal := expectedProcessCount(instance)
	rollout := instance.Status.Rollout
	if rollout == nil || !rolloutRequiresFullStop(instance) {
		return normal
	}
	if rollout.Stage == valkeyv1alpha1.RolloutStageStopping && rollout.AccessClosed ||
		rollout.Stage == valkeyv1alpha1.RolloutStageUpdatingTemplate {
		return 0
	}
	return normal
}

func rolloutRequiresFullStop(instance *valkeyv1alpha1.ValkeyInstance) bool {
	if instance.Status.Rollout == nil || instance.Status.Applied == nil {
		return false
	}
	return instance.Status.AcceptedConfiguration.Mode == valkeyv1alpha1.ValkeyModeSingle ||
		instance.Status.Rollout.RAMGB < instance.Status.Applied.RAMGB
}

func (r *ValkeyInstanceReconciler) reconcileValkeyImage(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, bool, error) {
	if instance.Status.ValkeyImage == "" {
		if instanceHasProcessHistory(instance.Status) {
			changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
				setCondition(
					instance,
					status,
					conditionTypeRecoveryRequired,
					metav1.ConditionTrue,
					"ValkeyImageMissing",
					"сохранённый образ Valkey отсутствует у инстанса с историей процессов",
				)
			})

			return true, changed, err
		}

		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			status.ValkeyImage = r.ValkeyImage
		})

		return false, changed, err
	}

	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		condition := apimeta.FindStatusCondition(status.Conditions, conditionTypeRecoveryRequired)
		if status.ValkeyImage != r.ValkeyImage {
			setCondition(
				instance,
				status,
				conditionTypeRecoveryRequired,
				metav1.ConditionTrue,
				"ValkeyImageChanged",
				"VALKEY_IMAGE отличается от сохранённого образа инстанса",
			)
			return
		}
		if condition != nil && (condition.Reason == "ValkeyImageChanged" || condition.Reason == "ValkeyImageMissing") {
			apimeta.RemoveStatusCondition(&status.Conditions, conditionTypeRecoveryRequired)
		}
	})

	return false, changed, err
}

func instanceHasProcessHistory(status valkeyv1alpha1.ValkeyInstanceStatus) bool {
	return status.Initialized || status.Applied != nil || len(status.Nodes) > 0 || len(status.PreviousProcesses) > 0
}

func desiredPod(
	instance *valkeyv1alpha1.ValkeyInstance,
	ordinal int32,
	configMapName string,
	image string,
	requestOverrides corev1.ResourceList,
) *corev1.Pod {
	accepted := instance.Status.AcceptedConfiguration
	selectorLabels := workloadLabels(accepted.Slug)
	labels := workloadResourceLabels(instance)
	name := fmt.Sprintf("%s-%d", accepted.Slug, ordinal)
	defaultMode := int32(0o555)
	terminationGracePeriod := int64(30)
	limits := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(accepted.VCPU), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(int64(accepted.RAMGB)*gibibyte, resource.BinarySI),
	}
	requests := limits.DeepCopy()
	for name, quantity := range requestOverrides {
		requests[name] = quantity.DeepCopy()
	}
	resources := corev1.ResourceRequirements{Requests: requests, Limits: limits}
	readinessProbe := new(corev1.Probe)
	readinessProbe.Exec = &corev1.ExecAction{
		Command: []string{"/bin/sh", "/etc/valkey-config/readiness.sh"},
	}
	readinessProbe.InitialDelaySeconds = 1
	readinessProbe.PeriodSeconds = config.ReadinessProbePeriod
	readinessProbe.TimeoutSeconds = config.ReadinessProbeTimeout
	readinessProbe.FailureThreshold = config.ReadinessProbeFailures
	configVolume := corev1.Volume{
		Name: "config",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
				DefaultMode:          &defaultMode,
			},
		},
	}
	authVolume := corev1.Volume{
		Name: "auth",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: valkeyv1alpha1.AuthSecretName(accepted.Slug),
		}},
	}
	runtimeVolume := corev1.Volume{
		Name: "runtime",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
		},
	}
	var affinity *corev1.Affinity
	if accepted.Mode == valkeyv1alpha1.ValkeyModeHA {
		affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: maps.Clone(selectorLabels)},
				TopologyKey:   corev1.LabelHostname,
			}},
		}}
	}

	pod := &corev1.Pod{
		ObjectMeta: ownedObjectMeta(instance, name, labels),
		Spec: corev1.PodSpec{
			Hostname:                      name,
			Subdomain:                     accepted.Slug + "-hl",
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &terminationGracePeriod,
			Affinity:                      affinity,
			Containers: []corev1.Container{{
				Name:            "valkey",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/bin/sh", "/etc/valkey-config/start.sh"},
				Ports: []corev1.ContainerPort{{
					Name: "valkey", ContainerPort: valkeyPort, Protocol: corev1.ProtocolTCP,
				}},
				Resources:      resources,
				ReadinessProbe: readinessProbe,
				VolumeMounts: []corev1.VolumeMount{
					{Name: "config", MountPath: "/etc/valkey-config", ReadOnly: true},
					{Name: "auth", MountPath: "/etc/valkey-auth", ReadOnly: true},
					{Name: "runtime", MountPath: "/run/valkey"},
				},
			}},
			Volumes: []corev1.Volume{configVolume, authVolume, runtimeVolume},
		},
	}
	pod.Finalizers = []string{processFinalizer}

	return pod
}

func desiredPodDisruptionBudget(instance *valkeyv1alpha1.ValkeyInstance) *policyv1.PodDisruptionBudget {
	selectorLabels := workloadLabels(instance.Status.AcceptedConfiguration.Slug)

	return &policyv1.PodDisruptionBudget{
		ObjectMeta: ownedObjectMeta(
			instance,
			instance.Status.AcceptedConfiguration.Slug,
			workloadResourceLabels(instance),
		),
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &intstr.IntOrString{Type: intstr.Int, IntVal: 2},
			Selector:     &metav1.LabelSelector{MatchLabels: maps.Clone(selectorLabels)},
		},
	}
}

func desiredServices(instance *valkeyv1alpha1.ValkeyInstance) []*corev1.Service {
	accepted := instance.Status.AcceptedConfiguration
	selectorLabels := workloadLabels(accepted.Slug)
	labels := workloadResourceLabels(instance)
	headless := &corev1.Service{
		ObjectMeta: ownedObjectMeta(instance, accepted.Slug+"-hl", labels),
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 maps.Clone(selectorLabels),
			Ports: []corev1.ServicePort{{
				Name: "valkey", Port: valkeyPort, TargetPort: intstr.FromString("valkey"), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	primarySelector := maps.Clone(selectorLabels)
	primarySelector[applicationRoleLabel] = string(valkeyv1alpha1.NodeRolePrimary)
	primary := &corev1.Service{
		ObjectMeta: ownedObjectMeta(instance, accepted.Slug+"-primary", labels),
		Spec: corev1.ServiceSpec{
			Selector: primarySelector,
			Ports: []corev1.ServicePort{{
				Name: "valkey", Port: valkeyPort, TargetPort: intstr.FromString("valkey"), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	if accepted.Mode == valkeyv1alpha1.ValkeyModeHA {
		replicaSelector := maps.Clone(selectorLabels)
		replicaSelector[applicationRoleLabel] = string(valkeyv1alpha1.NodeRoleReplica)
		replicas := &corev1.Service{
			ObjectMeta: ownedObjectMeta(instance, accepted.Slug+"-replicas", labels),
			Spec: corev1.ServiceSpec{
				Selector: replicaSelector,
				Ports: []corev1.ServicePort{{
					Name: "valkey", Port: valkeyPort,
					TargetPort: intstr.FromString("valkey"), Protocol: corev1.ProtocolTCP,
				}},
			},
		}

		return []*corev1.Service{headless, primary, replicas}
	}

	return []*corev1.Service{headless, primary}
}

func desiredNetworkPolicy(
	instance *valkeyv1alpha1.ValkeyInstance,
	systemNamespace string,
	operatorCIDRs []string,
) *networkingv1.NetworkPolicy {
	accepted := instance.Status.AcceptedConfiguration
	selectorLabels := workloadLabels(accepted.Slug)
	labels := workloadResourceLabels(instance)
	namespaceNameLabel := "kubernetes.io/metadata.name"
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	port := intstr.FromInt32(valkeyPort)
	dnsPort := intstr.FromInt32(53)

	ingressPeers := []networkingv1.NetworkPolicyPeer{
		{NamespaceSelector: namespaceSelector(namespaceNameLabel, envoyNamespace)},
		{NamespaceSelector: namespaceSelector(namespaceNameLabel, systemNamespace)},
		{PodSelector: &metav1.LabelSelector{MatchLabels: maps.Clone(selectorLabels)}},
	}
	for _, cidr := range operatorCIDRs {
		ingressPeers = append(ingressPeers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: cidr},
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: ownedObjectMeta(instance, accepted.Slug, labels),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: maps.Clone(selectorLabels)},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  ingressPeers,
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{PodSelector: &metav1.LabelSelector{MatchLabels: maps.Clone(selectorLabels)}},
					},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: namespaceSelector(namespaceNameLabel, metav1.NamespaceSystem),
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &dnsPort},
						{Protocol: &tcp, Port: &dnsPort},
					},
				},
			},
		},
	}
}

func (r *ValkeyInstanceReconciler) reconcileLegacyStatefulSet(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, bool, error) {
	statefulSet := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.AcceptedConfiguration.Slug}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	err := reader.Get(ctx, key, statefulSet)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, false, fmt.Errorf("проверить прежний StatefulSet: %w", err)
	}
	found := err == nil
	changed, updateErr := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		condition := apimeta.FindStatusCondition(status.Conditions, conditionTypeRecoveryRequired)
		if found {
			setCondition(
				instance,
				status,
				conditionTypeRecoveryRequired,
				metav1.ConditionTrue,
				"LegacyStatefulSet",
				"прежний пользовательский StatefulSet нужно штатно удалить совместимой версией оператора",
			)
			return
		}
		if condition != nil && condition.Reason == "LegacyStatefulSet" {
			apimeta.RemoveStatusCondition(&status.Conditions, conditionTypeRecoveryRequired)
		}
	})

	return found, changed, updateErr
}

func processCreationAllowed(instance *valkeyv1alpha1.ValkeyInstance, ordinal int32) bool {
	for _, previous := range instance.Status.PreviousProcesses {
		if previous.Ordinal == ordinal && previous.Termination == nil {
			return false
		}
	}
	current, found := nodeStatusAtOrdinal(instance.Status.Nodes, ordinal)
	return !found || current.Termination != nil
}

func (r *ValkeyInstanceReconciler) ensurePod(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	desired *corev1.Pod,
) (bool, bool, error) {
	r.Scheme.Default(desired)
	current := &corev1.Pod{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), current)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return false, false, fmt.Errorf("создать Pod %s: %w", desired.Name, err)
			}
			reader := r.APIReader
			if reader == nil {
				reader = r.Client
			}
			if err := reader.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
				return false, false, fmt.Errorf("перечитать Pod %s после конфликта создания: %w", desired.Name, err)
			}
		} else {
			return true, false, nil
		}
	} else if err != nil {
		return false, false, fmt.Errorf("прочитать Pod %s: %w", desired.Name, err)
	}
	if podOwnedByInstance(current, instance) {
		changed, err := r.clearPodOwnershipConflict(ctx, instance)
		return changed, false, err
	}

	changed, err := r.setPodOwnershipConflict(ctx, instance, current.Name)
	return changed, true, err
}

func podOwnedByInstance(pod *corev1.Pod, instance *valkeyv1alpha1.ValkeyInstance) bool {
	owner := metav1.GetControllerOf(pod)
	return owner != nil && owner.APIVersion == valkeyv1alpha1.GroupVersion.String() &&
		owner.Kind == "ValkeyInstance" && owner.Name == instance.Name && owner.UID == instance.UID
}

func (r *ValkeyInstanceReconciler) setPodOwnershipConflict(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
	podName string,
) (bool, error) {
	return r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		setCondition(
			instance,
			status,
			conditionTypeRecoveryRequired,
			metav1.ConditionTrue,
			"PodOwnershipConflict",
			fmt.Sprintf("имя Pod %s занято объектом другого владельца", podName),
		)
	})
}

func (r *ValkeyInstanceReconciler) clearPodOwnershipConflict(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
	return r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		condition := apimeta.FindStatusCondition(status.Conditions, conditionTypeRecoveryRequired)
		if condition != nil && condition.Reason == "PodOwnershipConflict" {
			apimeta.RemoveStatusCondition(&status.Conditions, conditionTypeRecoveryRequired)
		}
	})
}

func (r *ValkeyInstanceReconciler) requestWorkloadStop(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (bool, error) {
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
			return false, fmt.Errorf("прочитать Pod для полной остановки: %w", err)
		}
		if !podOwnedByInstance(pod, instance) {
			_, err := r.setPodOwnershipConflict(ctx, instance, pod.Name)
			return false, err
		}
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		uid := pod.UID
		resourceVersion := pod.ResourceVersion
		if err := r.Delete(ctx, pod, &client.DeleteOptions{
			GracePeriodSeconds: ptr.To(config.ProcessDeletionGracePeriod),
			Preconditions:      &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
		}); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("остановить Pod %s: %w", pod.Name, err)
		}

		return true, nil
	}

	return false, nil
}

func (r *ValkeyInstanceReconciler) ensureService(ctx context.Context, desired *corev1.Service) (bool, error) {
	r.Scheme.Default(desired)
	current := &corev1.Service{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("прочитать Service %s: %w", desired.Name, err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("создать Service %s: %w", desired.Name, err)
		}

		return true, nil
	}

	before := current.DeepCopy()
	mergeManagedMetadata(current, desired)
	current.OwnerReferences = slices.Clone(desired.OwnerReferences)
	current.Spec.Selector = maps.Clone(desired.Spec.Selector)
	current.Spec.Ports = slices.Clone(desired.Spec.Ports)
	current.Spec.PublishNotReadyAddresses = desired.Spec.PublishNotReadyAddresses
	if desired.Spec.ClusterIP == corev1.ClusterIPNone {
		current.Spec.ClusterIP = corev1.ClusterIPNone
	}
	if reflect.DeepEqual(before, current) {
		return false, nil
	}
	if err := r.Patch(ctx, current, client.MergeFrom(before)); err != nil {
		return false, fmt.Errorf("обновить Service %s: %w", desired.Name, err)
	}

	return current.ResourceVersion != before.ResourceVersion, nil
}

func (r *ValkeyInstanceReconciler) ensureNetworkPolicy(
	ctx context.Context,
	desired *networkingv1.NetworkPolicy,
) (bool, error) {
	current := &networkingv1.NetworkPolicy{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("прочитать NetworkPolicy: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("создать NetworkPolicy: %w", err)
		}

		return true, nil
	}

	before := current.DeepCopy()
	mergeManagedMetadata(current, desired)
	current.OwnerReferences = slices.Clone(desired.OwnerReferences)
	current.Spec = *desired.Spec.DeepCopy()
	if reflect.DeepEqual(before, current) {
		return false, nil
	}
	if err := r.Patch(ctx, current, client.MergeFrom(before)); err != nil {
		return false, fmt.Errorf("обновить NetworkPolicy: %w", err)
	}

	return current.ResourceVersion != before.ResourceVersion, nil
}

func (r *ValkeyInstanceReconciler) ensurePodDisruptionBudget(
	ctx context.Context,
	desired *policyv1.PodDisruptionBudget,
) (bool, error) {
	current := &policyv1.PodDisruptionBudget{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("прочитать PodDisruptionBudget: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("создать PodDisruptionBudget: %w", err)
		}

		return true, nil
	}

	before := current.DeepCopy()
	mergeManagedMetadata(current, desired)
	current.OwnerReferences = slices.Clone(desired.OwnerReferences)
	current.Spec = *desired.Spec.DeepCopy()
	if reflect.DeepEqual(before, current) {
		return false, nil
	}
	if err := r.Patch(ctx, current, client.MergeFrom(before)); err != nil {
		return false, fmt.Errorf("обновить PodDisruptionBudget: %w", err)
	}

	return current.ResourceVersion != before.ResourceVersion, nil
}

func workloadLabels(slug string) map[string]string {
	return map[string]string{
		applicationNameLabel: "valkey",
		instanceLabelKey:     slug,
	}
}

func workloadResourceLabels(instance *valkeyv1alpha1.ValkeyInstance) map[string]string {
	labels := workloadLabels(instance.Status.AcceptedConfiguration.Slug)
	if instanceID := instance.Status.AcceptedConfiguration.InstanceID; instanceID != "" {
		labels[valkeyv1alpha1.InstanceIDLabelKey] = instanceID
	}
	if userID := instance.Labels[valkeyv1alpha1.UserIDLabelKey]; userID != "" {
		labels[valkeyv1alpha1.UserIDLabelKey] = userID
	}

	return labels
}

func workloadAnnotations(instance *valkeyv1alpha1.ValkeyInstance) map[string]string {
	email := instance.Annotations[valkeyv1alpha1.UserEmailAnnotationKey]
	if email == "" {
		return nil
	}

	return map[string]string{valkeyv1alpha1.UserEmailAnnotationKey: email}
}

func mergeManagedMetadata(current, desired metav1.Object) {
	labels := maps.Clone(current.GetLabels())
	if labels == nil {
		labels = make(map[string]string, len(desired.GetLabels()))
	}
	maps.Copy(labels, desired.GetLabels())
	current.SetLabels(labels)

	if email := desired.GetAnnotations()[valkeyv1alpha1.UserEmailAnnotationKey]; email != "" {
		annotations := maps.Clone(current.GetAnnotations())
		if annotations == nil {
			annotations = make(map[string]string, 1)
		}
		annotations[valkeyv1alpha1.UserEmailAnnotationKey] = email
		current.SetAnnotations(annotations)
	}
}

func managedMetadataMatches(current, desired metav1.Object) bool {
	for key, value := range desired.GetLabels() {
		if current.GetLabels()[key] != value {
			return false
		}
	}
	if email := desired.GetAnnotations()[valkeyv1alpha1.UserEmailAnnotationKey]; email != "" &&
		current.GetAnnotations()[valkeyv1alpha1.UserEmailAnnotationKey] != email {
		return false
	}

	return true
}

func ownedObjectMeta(
	instance *valkeyv1alpha1.ValkeyInstance,
	name string,
	labels map[string]string,
) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   instance.Namespace,
		Labels:      maps.Clone(labels),
		Annotations: workloadAnnotations(instance),
		OwnerReferences: []metav1.OwnerReference{
			*metav1.NewControllerRef(instance, valkeyv1alpha1.GroupVersion.WithKind("ValkeyInstance")),
		},
	}
}

func namespaceSelector(key, value string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{key: value}}
}
