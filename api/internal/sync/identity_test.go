package sync

import (
	"maps"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func Test_ReconcileObjectAnnotations_WithMissingOrWrongOwnerEmail_RepairsOnlyManagedAnnotation(t *testing.T) {
	instance := store.ValkeyInstance{UserEmail: "owner@example.com"}

	for _, annotations := range []map[string]string{
		{"other.example.com/value": "kept"},
		{valkeyv1alpha1.UserEmailAnnotationKey: "wrong@example.com", "other.example.com/value": "kept"},
	} {
		object := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Annotations: maps.Clone(annotations)}}
		if !reconcileObjectAnnotations(object, instance) {
			t.Fatalf("аннотация не исправлена: %v", annotations)
		}
		if object.Annotations[valkeyv1alpha1.UserEmailAnnotationKey] != instance.UserEmail ||
			object.Annotations["other.example.com/value"] != "kept" {
			t.Fatalf("получены неверные аннотации: %v", object.Annotations)
		}
	}
}

func Test_ReconcileObjectAnnotations_WhenOwnerEmailMatches_DoesNotChangeObject(t *testing.T) {
	annotations := map[string]string{
		valkeyv1alpha1.UserEmailAnnotationKey: "owner@example.com",
		"other.example.com/value":             "kept",
	}
	object := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Annotations: maps.Clone(annotations)}}
	if reconcileObjectAnnotations(object, store.ValkeyInstance{UserEmail: "owner@example.com"}) {
		t.Fatal("совпадающие аннотации отмечены как изменённые")
	}
	if !maps.Equal(object.Annotations, annotations) {
		t.Fatalf("совпадающие аннотации изменены: %v", object.Annotations)
	}
}
