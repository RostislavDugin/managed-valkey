package operator

import (
	"errors"
	"testing"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func TestCT04NextAcceptedConfiguration(t *testing.T) {
	accepted := valkeyv1alpha1.AcceptedConfiguration{
		InstanceID: "instance-1", Slug: "cache-a1b2c3", Mode: valkeyv1alpha1.ValkeyModeSingle,
		VCPU: 1, RAMGB: 4, PublicPort: 41379, Whitelist: valkeyv1alpha1.WhitelistSpec{},
		PasswordVersion: 2, DesiredGeneration: 7,
	}
	base := valkeyv1alpha1.ValkeyInstanceSpec{
		InstanceID: accepted.InstanceID, Slug: accepted.Slug, Mode: accepted.Mode,
		VCPU: accepted.VCPU, RAMGB: accepted.RAMGB, PublicPort: accepted.PublicPort,
		Whitelist: &valkeyv1alpha1.WhitelistSpec{}, PasswordVersion: accepted.PasswordVersion,
		DesiredGeneration: accepted.DesiredGeneration + 1,
	}

	t.Run("size and password", func(t *testing.T) {
		spec := base
		spec.VCPU = 2
		spec.RAMGB = 8
		spec.PasswordVersion = 3

		candidate, err := nextAcceptedConfiguration(spec, accepted)
		if err != nil || candidate.VCPU != 2 || candidate.RAMGB != 8 ||
			candidate.PasswordVersion != 3 || candidate.DesiredGeneration != 8 {
			t.Fatalf("поддерживаемое поколение отклонено: candidate=%+v error=%v", candidate, err)
		}
	})

	tests := []struct {
		name          string
		mutate        func(*valkeyv1alpha1.ValkeyInstanceSpec)
		conditionType string
		reason        string
	}{
		{
			name: "unsupported mixed fields",
			mutate: func(spec *valkeyv1alpha1.ValkeyInstanceSpec) {
				spec.RAMGB = 8
				spec.Whitelist = &valkeyv1alpha1.WhitelistSpec{
					IsEnabled: true, CIDRs: []string{"203.0.113.0/24"},
				}
			},
			conditionType: conditionTypeUnsupportedChange,
			reason:        "UnsupportedFields",
		},
		{
			name: "stale generation",
			mutate: func(spec *valkeyv1alpha1.ValkeyInstanceSpec) {
				spec.DesiredGeneration = 6
			},
			conditionType: conditionTypeInvalidIntent,
			reason:        "StaleDesiredGeneration",
		},
		{
			name: "accepted generation changed in place",
			mutate: func(spec *valkeyv1alpha1.ValkeyInstanceSpec) {
				spec.DesiredGeneration = 7
				spec.RAMGB = 8
			},
			conditionType: conditionTypeInvalidIntent,
			reason:        "AcceptedGenerationChanged",
		},
		{
			name: "password rollback",
			mutate: func(spec *valkeyv1alpha1.ValkeyInstanceSpec) {
				spec.PasswordVersion = 1
			},
			conditionType: conditionTypeInvalidIntent,
			reason:        "PasswordVersionRollback",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := base
			test.mutate(&spec)

			candidate, err := nextAcceptedConfiguration(spec, accepted)
			if candidate != nil || err == nil {
				t.Fatalf("некорректное поколение принято: candidate=%+v error=%v", candidate, err)
			}
			var validation *configurationValidationError
			if !errors.As(err, &validation) || validation.conditionType != test.conditionType ||
				validation.reason != test.reason {
				t.Fatalf("неверная диагностика: %+v", validation)
			}
		})
	}
}

func TestAcceptedConfigurationCompleteRequiresAllOperations(t *testing.T) {
	accepted := valkeyv1alpha1.AcceptedConfiguration{DesiredGeneration: 4}
	status := valkeyv1alpha1.ValkeyInstanceStatus{ObservedGeneration: 4}
	if !acceptedConfigurationComplete(status, accepted) {
		t.Fatal("завершённое поколение не распознано")
	}

	status.Rollout = &valkeyv1alpha1.RolloutStatus{}
	if acceptedConfigurationComplete(status, accepted) {
		t.Fatal("поколение подтверждено при незавершённом rollout")
	}
	status.Rollout = nil
	status.CredentialRotation = &valkeyv1alpha1.CredentialRotationStatus{}
	if acceptedConfigurationComplete(status, accepted) {
		t.Fatal("поколение подтверждено при незавершённой ротации")
	}
}
