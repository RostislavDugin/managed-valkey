package operator

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

const (
	instanceLabelKey = valkeyv1alpha1.InstanceLabelKey
	userIDLabelKey   = valkeyv1alpha1.UserIDLabelKey
)

var (
	errIncompleteIntent = errors.New("намерение создания неполно")
	errInvalidIdentity  = errors.New("идентичность инстанса не совпадает")
	errUnsupportedMode  = errors.New("режим пока не поддерживается")
	errInvalidIntent    = errors.New("намерение создания некорректно")
)

func acceptedConfiguration(
	instance *valkeyv1alpha1.ValkeyInstance,
	namespace *corev1.Namespace,
) (*valkeyv1alpha1.AcceptedConfiguration, error) {
	if err := validateIdentity(instance, namespace); err != nil {
		return nil, err
	}

	spec := instance.Spec
	if spec.Mode == "" || spec.VCPU == 0 || spec.RAMGB == 0 || spec.PublicPort == 0 || spec.Whitelist == nil ||
		spec.PasswordVersion == 0 || spec.DesiredGeneration == 0 {
		return nil, errIncompleteIntent
	}
	if spec.Mode != valkeyv1alpha1.ValkeyModeSingle {
		if spec.Mode == valkeyv1alpha1.ValkeyModeHA {
			return nil, errUnsupportedMode
		}

		return nil, errInvalidIntent
	}
	if !validSize(spec.VCPU, spec.RAMGB) {
		return nil, errInvalidIntent
	}

	whitelist, err := normalizedWhitelist(spec.Whitelist)
	if err != nil {
		return nil, errInvalidIntent
	}

	return &valkeyv1alpha1.AcceptedConfiguration{
		InstanceID:        spec.InstanceID,
		Slug:              spec.Slug,
		Mode:              spec.Mode,
		VCPU:              spec.VCPU,
		RAMGB:             spec.RAMGB,
		PublicPort:        spec.PublicPort,
		Whitelist:         whitelist,
		PasswordVersion:   spec.PasswordVersion,
		DesiredGeneration: spec.DesiredGeneration,
	}, nil
}

func validateIdentity(instance *valkeyv1alpha1.ValkeyInstance, namespace *corev1.Namespace) error {
	instanceID, err := uuid.Parse(instance.Spec.InstanceID)
	if err != nil || instanceID.Version() != uuid.Version(7) {
		return errInvalidIdentity
	}
	if !validSlug(instance.Spec.Slug) || instance.Name != instance.Spec.Slug ||
		instance.Namespace != "valkey-"+instance.Spec.Slug || namespace.Name != instance.Namespace {
		return errInvalidIdentity
	}
	if namespace.Labels[instanceLabelKey] != instance.Spec.Slug {
		return errInvalidIdentity
	}

	userID, err := uuid.Parse(namespace.Labels[userIDLabelKey])
	if err != nil || userID.Version() != uuid.Version(7) {
		return errInvalidIdentity
	}

	return nil
}

func validSlug(slug string) bool {
	separator := strings.LastIndexByte(slug, '-')
	if separator < 0 {
		return false
	}

	prefix, suffix := slug[:separator], slug[separator+1:]
	if len(prefix) < 3 || len(prefix) > 20 || len(suffix) != 6 {
		return false
	}

	prefixValid, err := regexp.MatchString(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])$`, prefix)
	if err != nil || !prefixValid {
		return false
	}

	suffixValid, err := regexp.MatchString(`^[a-z0-9]{6}$`, suffix)
	return err == nil && suffixValid
}

func validSize(vcpu, ramGB int32) bool {
	validVCPU := slices.Contains([]int32{1, 2, 4, 8, 16}, vcpu)
	validRAM := slices.Contains([]int32{1, 2, 4, 8, 16, 32, 64, 128}, ramGB)

	return validVCPU && validRAM && ramGB >= vcpu && ramGB <= 16*vcpu
}

func normalizedWhitelist(whitelist *valkeyv1alpha1.WhitelistSpec) (valkeyv1alpha1.WhitelistSpec, error) {
	if whitelist == nil {
		return valkeyv1alpha1.WhitelistSpec{}, errIncompleteIntent
	}

	result := valkeyv1alpha1.WhitelistSpec{IsEnabled: whitelist.IsEnabled}
	seen := make(map[netip.Prefix]struct{}, len(whitelist.CIDRs))

	for _, value := range whitelist.CIDRs {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return valkeyv1alpha1.WhitelistSpec{}, fmt.Errorf("разобрать CIDR: %w", errInvalidIntent)
		}
		seen[prefix.Masked()] = struct{}{}
	}

	for prefix := range seen {
		result.CIDRs = append(result.CIDRs, prefix.String())
	}
	slices.Sort(result.CIDRs)

	return result, nil
}

func configurationDifferences(
	spec valkeyv1alpha1.ValkeyInstanceSpec,
	accepted valkeyv1alpha1.AcceptedConfiguration,
) []string {
	differences := make([]string, 0, 8)
	if spec.InstanceID != accepted.InstanceID {
		differences = append(differences, "instanceId")
	}
	if spec.Slug != accepted.Slug {
		differences = append(differences, "slug")
	}
	if spec.Mode != accepted.Mode {
		differences = append(differences, "mode")
	}
	if spec.VCPU != accepted.VCPU {
		differences = append(differences, "vcpu")
	}
	if spec.RAMGB != accepted.RAMGB {
		differences = append(differences, "ramGb")
	}
	if spec.PublicPort != accepted.PublicPort {
		differences = append(differences, "publicPort")
	}
	if spec.PasswordVersion != accepted.PasswordVersion {
		differences = append(differences, "passwordVersion")
	}
	if spec.DesiredGeneration != accepted.DesiredGeneration {
		differences = append(differences, "desiredGeneration")
	}

	whitelist, err := normalizedWhitelist(spec.Whitelist)
	if err != nil || whitelist.IsEnabled != accepted.Whitelist.IsEnabled ||
		!slices.Equal(whitelist.CIDRs, accepted.Whitelist.CIDRs) {
		differences = append(differences, "whitelist")
	}

	return differences
}
