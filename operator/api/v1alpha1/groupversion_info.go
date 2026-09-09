// Package v1alpha1 импортируют оба приложения: API записывает spec и читает
// status, оператор владеет поведением reconcile.
//
// +kubebuilder:object:generate=true
// +groupName=valkey.h3llo-demo.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// Имя SchemeGroupVersion нужно генераторам applyconfiguration.
	SchemeGroupVersion = schema.GroupVersion{Group: "valkey.h3llo-demo.com", Version: "v1alpha1"}

	GroupVersion = SchemeGroupVersion

	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, &ValkeyInstance{}, &ValkeyInstanceList{})
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)

	return nil
}
