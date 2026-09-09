package valkey

import (
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"regexp"
	"slices"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

var (
	namePattern     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$`)
	prefixPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,18}[a-z0-9])$`)
	passwordPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32}$`)
)

func validateName(value string) error {
	if !namePattern.MatchString(value) {
		return validation(map[string]string{"name": "invalid_format"})
	}

	return nil
}

func validatePrefix(value string) error {
	if !prefixPattern.MatchString(value) {
		return validation(map[string]string{"prefix": "invalid_format"})
	}

	return nil
}

func validateMode(value domain.ValkeyInstanceMode) error {
	if value != domain.ValkeyInstanceModeSingle && value != domain.ValkeyInstanceModeHA {
		return validation(map[string]string{"mode": "unsupported"})
	}

	return nil
}

func validatePassword(value string) error {
	if !passwordPattern.MatchString(value) {
		return validation(map[string]string{"password": "invalid_format"})
	}

	return nil
}

func validateMaintenance(value *Maintenance) error {
	if value == nil {
		return nil
	}
	fields := map[string]string{}
	if value.DOW < 0 || value.DOW > 6 {
		fields["maintenance.dow"] = "out_of_range"
	}
	if value.HourUTC < 0 || value.HourUTC > 23 {
		fields["maintenance.hour_utc"] = "out_of_range"
	}
	if value.DurationMin < 1 || value.DurationMin > 1440 {
		fields["maintenance.duration_min"] = "out_of_range"
	}
	if len(fields) > 0 {
		return validation(fields)
	}

	return nil
}

func normalizeWhitelist(values []string) ([]string, error) {
	if len(values) > 100 {
		return nil, validation(map[string]string{"whitelist_cidrs": "too_many"})
	}

	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			address, addressErr := netip.ParseAddr(value)
			if addressErr != nil || !address.Is4() {
				return nil, validation(map[string]string{"whitelist_cidrs": "invalid_ipv4_cidr"})
			}

			prefix = netip.PrefixFrom(address, 32)
		}
		if !prefix.Addr().Is4() {
			return nil, validation(map[string]string{"whitelist_cidrs": "invalid_ipv4_cidr"})
		}

		unique[prefix.Masked().String()] = struct{}{}
	}

	normalized := make([]string, 0, len(unique))
	for value := range unique {
		normalized = append(normalized, value)
	}
	slices.Sort(normalized)

	return normalized, nil
}

func passwordHash(value string) string {
	digest := sha256.Sum256([]byte(value))

	return hex.EncodeToString(digest[:])
}

func validation(fields map[string]string) error {
	return apierr.New(
		apierr.CodeValidationFailed,
		"Проверьте параметры запроса",
		map[string]any{"fields": fields},
	)
}
