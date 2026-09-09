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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

const (
	applicationNameLabel = "app.kubernetes.io/name"
	applicationRoleLabel = "role"
	processFinalizer     = "valkey.h3llo-demo.com/process-stopped"
	valkeyPort           = int32(6379)
	envoyNamespace       = "envoy-gateway-system"
)

func (r *ValkeyInstanceReconciler) reconcileSingleResources(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (ctrl.Result, error) {
	configMap, err := r.desiredConfigMap(instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	valkeyImage, changed, err := r.protectedValkeyImage(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	statefulSetChanged, err := r.ensureStatefulSet(ctx, desiredStatefulSet(instance, configMap.Name, valkeyImage))
	if err != nil {
		return ctrl.Result{}, err
	}
	changed = changed || statefulSetChanged

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

	return requeueIf(changed), nil
}

func (r *ValkeyInstanceReconciler) protectedValkeyImage(
	ctx context.Context,
	instance *valkeyv1alpha1.ValkeyInstance,
) (string, bool, error) {
	statefulSet := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: instance.Namespace, Name: instance.Status.AcceptedConfiguration.Slug}
	if err := r.Get(ctx, key, statefulSet); err != nil {
		if apierrors.IsNotFound(err) {
			return r.ValkeyImage, false, nil
		}

		return "", false, fmt.Errorf("прочитать StatefulSet перед проверкой образа: %w", err)
	}

	currentImage := ""
	for _, container := range statefulSet.Spec.Template.Spec.Containers {
		if container.Name == "valkey" {
			currentImage = container.Image
			break
		}
	}
	if currentImage == "" {
		return r.ValkeyImage, false, nil
	}
	if currentImage != r.ValkeyImage {
		changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
			setCondition(
				instance,
				status,
				conditionTypeRecoveryRequired,
				metav1.ConditionTrue,
				"ValkeyImageChanged",
				"VALKEY_IMAGE отличается от образа существующего StatefulSet",
			)
		})

		return currentImage, changed, err
	}

	changed, err := r.updateStatus(ctx, instance, func(status *valkeyv1alpha1.ValkeyInstanceStatus) {
		condition := apimeta.FindStatusCondition(status.Conditions, conditionTypeRecoveryRequired)
		if condition != nil && condition.Reason == "ValkeyImageChanged" {
			apimeta.RemoveStatusCondition(&status.Conditions, conditionTypeRecoveryRequired)
		}
	})

	return currentImage, changed, err
}

func desiredStatefulSet(
	instance *valkeyv1alpha1.ValkeyInstance,
	configMapName string,
	image string,
) *appsv1.StatefulSet {
	accepted := instance.Status.AcceptedConfiguration
	labels := workloadLabels(accepted.Slug)
	defaultMode := int32(0o555)
	terminationGracePeriod := int64(30)
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(int64(accepted.VCPU), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(int64(accepted.RAMGB)*gibibyte, resource.BinarySI),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(int64(accepted.VCPU), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(int64(accepted.RAMGB)*gibibyte, resource.BinarySI),
		},
	}
	readinessProbe := new(corev1.Probe)
	readinessProbe.Exec = &corev1.ExecAction{
		Command: []string{"/bin/sh", "/etc/valkey-config/readiness.sh"},
	}
	readinessProbe.InitialDelaySeconds = 1
	readinessProbe.PeriodSeconds = 5
	readinessProbe.TimeoutSeconds = 3
	readinessProbe.FailureThreshold = 3
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

	statefulSet := &appsv1.StatefulSet{
		Spec: appsv1.StatefulSetSpec{
			Replicas:            ptr.To[int32](1),
			ServiceName:         accepted.Slug + "-hl",
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy:      appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType},
			Selector:            &metav1.LabelSelector{MatchLabels: maps.Clone(labels)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:     maps.Clone(labels),
					Finalizers: []string{processFinalizer},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyAlways,
					TerminationGracePeriodSeconds: &terminationGracePeriod,
					Containers: []corev1.Container{{
						Name:            "valkey",
						Image:           image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"/bin/sh", "/etc/valkey-config/start.sh"},
						Ports: []corev1.ContainerPort{{
							Name: "valkey", ContainerPort: valkeyPort, Protocol: corev1.ProtocolTCP,
						}},
						Resources:      resources,
						RestartPolicy:  ptr.To(corev1.ContainerRestartPolicyNever),
						ReadinessProbe: readinessProbe,
						VolumeMounts: []corev1.VolumeMount{
							{Name: "config", MountPath: "/etc/valkey-config", ReadOnly: true},
							{Name: "auth", MountPath: "/etc/valkey-auth", ReadOnly: true},
							{Name: "runtime", MountPath: "/run/valkey"},
						},
					}},
					Volumes: []corev1.Volume{configVolume, authVolume, runtimeVolume},
				},
			},
		},
	}
	statefulSet.Name = accepted.Slug
	statefulSet.Namespace = instance.Namespace
	statefulSet.Labels = maps.Clone(labels)
	statefulSet.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(instance, valkeyv1alpha1.GroupVersion.WithKind("ValkeyInstance")),
	}

	return statefulSet
}

func desiredServices(instance *valkeyv1alpha1.ValkeyInstance) []*corev1.Service {
	accepted := instance.Status.AcceptedConfiguration
	labels := workloadLabels(accepted.Slug)
	headless := &corev1.Service{
		ObjectMeta: ownedObjectMeta(instance, accepted.Slug+"-hl", labels),
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 maps.Clone(labels),
			Ports: []corev1.ServicePort{{
				Name: "valkey", Port: valkeyPort, TargetPort: intstr.FromString("valkey"), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	primarySelector := maps.Clone(labels)
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

	return []*corev1.Service{headless, primary}
}

func desiredNetworkPolicy(
	instance *valkeyv1alpha1.ValkeyInstance,
	systemNamespace string,
	operatorCIDRs []string,
) *networkingv1.NetworkPolicy {
	accepted := instance.Status.AcceptedConfiguration
	labels := workloadLabels(accepted.Slug)
	namespaceNameLabel := "kubernetes.io/metadata.name"
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	port := intstr.FromInt32(valkeyPort)
	dnsPort := intstr.FromInt32(53)

	ingressPeers := []networkingv1.NetworkPolicyPeer{
		{NamespaceSelector: namespaceSelector(namespaceNameLabel, envoyNamespace)},
		{NamespaceSelector: namespaceSelector(namespaceNameLabel, systemNamespace)},
		{PodSelector: &metav1.LabelSelector{MatchLabels: maps.Clone(labels)}},
	}
	for _, cidr := range operatorCIDRs {
		ingressPeers = append(ingressPeers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: cidr},
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: ownedObjectMeta(instance, accepted.Slug, labels),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: maps.Clone(labels)},
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
						{PodSelector: &metav1.LabelSelector{MatchLabels: maps.Clone(labels)}},
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

func (r *ValkeyInstanceReconciler) ensureStatefulSet(
	ctx context.Context,
	desired *appsv1.StatefulSet,
) (bool, error) {
	r.Scheme.Default(desired)
	current := &appsv1.StatefulSet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), current); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("прочитать StatefulSet: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("создать StatefulSet: %w", err)
		}

		return true, nil
	}

	before := current.DeepCopy()
	current.Labels = maps.Clone(desired.Labels)
	current.OwnerReferences = slices.Clone(desired.OwnerReferences)
	current.Spec.Replicas = new(*desired.Spec.Replicas)
	current.Spec.ServiceName = desired.Spec.ServiceName
	current.Spec.PodManagementPolicy = desired.Spec.PodManagementPolicy
	current.Spec.UpdateStrategy = desired.Spec.UpdateStrategy
	current.Spec.Selector = desired.Spec.Selector.DeepCopy()
	current.Spec.Template = *desired.Spec.Template.DeepCopy()
	if reflect.DeepEqual(before, current) {
		return false, nil
	}
	if err := r.Patch(ctx, current, client.MergeFrom(before)); err != nil {
		return false, fmt.Errorf("обновить StatefulSet: %w", err)
	}

	return current.ResourceVersion != before.ResourceVersion, nil
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
	current.Labels = maps.Clone(desired.Labels)
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
	current.Labels = maps.Clone(desired.Labels)
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

func workloadLabels(slug string) map[string]string {
	return map[string]string{
		applicationNameLabel: "valkey",
		instanceLabelKey:     slug,
	}
}

func ownedObjectMeta(
	instance *valkeyv1alpha1.ValkeyInstance,
	name string,
	labels map[string]string,
) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: instance.Namespace,
		Labels:    maps.Clone(labels),
		OwnerReferences: []metav1.OwnerReference{
			*metav1.NewControllerRef(instance, valkeyv1alpha1.GroupVersion.WithKind("ValkeyInstance")),
		},
	}
}

func namespaceSelector(key, value string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{key: value}}
}
