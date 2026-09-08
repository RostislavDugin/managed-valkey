// Package v1alpha1 импортируют оба приложения: API записывает spec и читает
// status, оператор владеет поведением reconcile.
//
// +kubebuilder:object:generate=true
// +groupName=valkey.h3llo-demo.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// Имя SchemeGroupVersion нужно генераторам applyconfiguration.
	SchemeGroupVersion = schema.GroupVersion{Group: "valkey.h3llo-demo.com", Version: "v1alpha1"}

	GroupVersion = SchemeGroupVersion

	SchemeBuilder = &scheme.Builder{GroupVersion: SchemeGroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)
