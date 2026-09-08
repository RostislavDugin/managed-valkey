package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Пока это только идентификаторы, которые API сверяет с записью PostgreSQL
// перед доставкой. Размер, режим, публичный порт, whitelist и версия пароля
// появятся вместе с поведением reconcile.
type ValkeyInstanceSpec struct {
	// instanceId равен valkey_instances.id и не меняется после создания.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="instanceId изменить нельзя"
	// +required
	InstanceID string `json:"instanceId"`

	// slug равен имени ресурса и входит в публичный адрес инстанса.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="slug изменить нельзя"
	// +required
	Slug string `json:"slug"`
}

// Status пишет только оператор.
type ValkeyInstanceStatus struct {
	// observedGeneration это поколение spec, полностью применённое оператором.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vki
// +kubebuilder:printcolumn:name="Slug",type=string,JSONPath=`.spec.slug`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type ValkeyInstance struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec ValkeyInstanceSpec `json:"spec"`

	// +optional
	Status ValkeyInstanceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

type ValkeyInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ValkeyInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ValkeyInstance{}, &ValkeyInstanceList{})
}
