package valkey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"gorm.io/gorm"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

const idempotencyTTL = 24 * time.Hour

func (s *Service) Create(ctx context.Context, actor Actor, input CreateInput) (InstanceResult, error) {
	normalizedCIDRs, err := s.validateCreate(input)
	if err != nil {
		return InstanceResult{}, err
	}

	requestHash, err := canonicalHash(http.MethodPost, "/v1/managed/valkey/instances", struct {
		Name             string                    `json:"name"`
		Prefix           string                    `json:"prefix"`
		Mode             domain.ValkeyInstanceMode `json:"mode"`
		VCPU             int                       `json:"vcpu"`
		RAMGB            int                       `json:"ram_gb"`
		Password         string                    `json:"password"`
		WhitelistEnabled bool                      `json:"is_whitelist_enabled"`
		WhitelistCIDRs   []string                  `json:"whitelist_cidrs"`
	}{
		Name: input.Name, Prefix: input.Prefix, Mode: input.Mode,
		VCPU: input.Size.VCPU, RAMGB: input.Size.RAMGB, Password: input.Password,
		WhitelistEnabled: input.WhitelistEnabled, WhitelistCIDRs: normalizedCIDRs,
	})
	if err != nil {
		return InstanceResult{}, apierr.WrapInternal(err)
	}

	passwordDigest := passwordHash(input.Password)
	var result InstanceResult
	err = s.beginMutation(ctx, func(tx *gorm.DB, now time.Time) error {
		replayed, found, replayErr := s.replayInstance(ctx, tx, actor.ID, input.IdempotencyKey, requestHash, now)
		if replayErr != nil {
			return replayErr
		}
		if found {
			result = replayed

			return nil
		}

		exists, existsErr := s.repository.ValkeyNameExists(ctx, tx, actor.ID, input.Name, nil)
		if existsErr != nil {
			return existsErr
		}
		if exists {
			return nameConflict()
		}

		if quotaErr := s.checkQuota(
			ctx,
			tx,
			actor.ID,
			Size{},
			reserveFor(input.Mode, input.Size, Size{}),
			true,
		); quotaErr != nil {
			return quotaErr
		}

		record, createErr := s.createWithUniqueSlug(
			ctx,
			tx,
			actor,
			input,
			normalizedCIDRs,
			passwordDigest,
			now,
		)
		if createErr != nil {
			return createErr
		}

		if auditErr := s.audit.Write(
			ctx,
			tx,
			auditEvent(actor, audit.ActionInstanceCreate, input.RequestID, record.ID, now),
		); auditErr != nil {
			return auditErr
		}
		if billingErr := s.openBillingPeriod(
			ctx,
			tx,
			record,
			domain.BillingPeriodStartReasonCreated,
			now,
		); billingErr != nil {
			return billingErr
		}

		result = InstanceResult{Status: http.StatusAccepted, Instance: instanceDTO(record, now)}

		return s.saveInstanceResponse(ctx, tx, actor.ID, input.IdempotencyKey, requestHash, result, now)
	})

	return result, err
}

func (s *Service) Patch(
	ctx context.Context,
	actor Actor,
	instanceID uuid.UUID,
	input PatchInput,
) (InstanceResult, error) {
	if input.Name == nil && !input.MaintenanceSet {
		return InstanceResult{}, validation(map[string]string{"body": "empty"})
	}
	if input.Name != nil {
		if err := validateName(*input.Name); err != nil {
			return InstanceResult{}, err
		}
	}
	if input.MaintenanceSet {
		if err := validateMaintenance(input.Maintenance); err != nil {
			return InstanceResult{}, err
		}
	}

	var result InstanceResult
	err := s.beginMutation(ctx, func(tx *gorm.DB, now time.Time) error {
		record, findErr := s.repository.FindOwnedValkeyInstanceForUpdate(ctx, tx, actor.ID, instanceID)
		if findErr != nil {
			return publicStoreError(findErr)
		}
		if record.DeletionRequestedAt != nil {
			return instanceNotReady(record, "deleting")
		}

		values := map[string]any{}
		if input.Name != nil && *input.Name != record.Name {
			exists, existsErr := s.repository.ValkeyNameExists(ctx, tx, actor.ID, *input.Name, &record.ID)
			if existsErr != nil {
				return existsErr
			}
			if exists {
				return nameConflict()
			}
			values["name"] = *input.Name
		}
		if input.MaintenanceSet && !maintenanceMatches(record, input.Maintenance) {
			if input.Maintenance == nil {
				values["maintenance_dow"] = nil
				values["maintenance_hour_utc"] = nil
				values["maintenance_duration_min"] = nil
			} else {
				values["maintenance_dow"] = input.Maintenance.DOW
				values["maintenance_hour_utc"] = input.Maintenance.HourUTC
				values["maintenance_duration_min"] = input.Maintenance.DurationMin
			}
		}
		if len(values) == 0 {
			result = InstanceResult{Status: http.StatusOK, Instance: instanceDTO(record, now)}

			return nil
		}

		values["updated_at"] = now
		if updateErr := s.repository.UpdateValkeyIntent(ctx, tx, record.ID, values); updateErr != nil {
			if errors.Is(updateErr, store.ErrValkeyNameConflict) {
				return nameConflict()
			}

			return updateErr
		}
		applyIntentValues(&record, values)

		if auditErr := s.audit.Write(
			ctx,
			tx,
			auditEvent(actor, audit.ActionInstanceUpdate, input.RequestID, record.ID, now),
		); auditErr != nil {
			return auditErr
		}

		result = InstanceResult{Status: http.StatusOK, Instance: instanceDTO(record, now)}

		return nil
	})

	return result, err
}

func (s *Service) Resize(
	ctx context.Context,
	actor Actor,
	instanceID uuid.UUID,
	input ResizeInput,
) (InstanceResult, error) {
	if !s.catalog.HasSize(input.Size) {
		return InstanceResult{}, validation(map[string]string{"size": "unsupported"})
	}

	path := "/v1/managed/valkey/instances/" + instanceID.String() + "/resize"
	requestHash, err := canonicalHash(http.MethodPost, path, struct {
		VCPU  int `json:"vcpu"`
		RAMGB int `json:"ram_gb"`
	}{VCPU: input.Size.VCPU, RAMGB: input.Size.RAMGB})
	if err != nil {
		return InstanceResult{}, apierr.WrapInternal(err)
	}

	var result InstanceResult
	err = s.beginMutation(ctx, func(tx *gorm.DB, now time.Time) error {
		record, findErr := s.repository.FindOwnedValkeyInstanceForUpdate(ctx, tx, actor.ID, instanceID)
		if findErr != nil {
			return publicStoreError(findErr)
		}

		replayed, found, replayErr := s.replayInstance(ctx, tx, actor.ID, input.IdempotencyKey, requestHash, now)
		if replayErr != nil {
			return replayErr
		}
		if found {
			result = replayed

			return nil
		}
		if record.DeletionRequestedAt != nil {
			return instanceNotReady(record, "deleting")
		}
		if record.VCPU == input.Size.VCPU && record.RAMGB == input.Size.RAMGB {
			result = InstanceResult{Status: http.StatusOK, Instance: instanceDTO(record, now)}

			return s.saveInstanceResponse(ctx, tx, actor.ID, input.IdempotencyKey, requestHash, result, now)
		}
		if readyErr := ensureReady(record, now); readyErr != nil {
			return readyErr
		}

		current := reserveFor(record.Mode, Size{VCPU: record.VCPU, RAMGB: record.RAMGB}, Size{
			VCPU: record.AppliedVCPU, RAMGB: record.AppliedRAMGB,
		})
		candidate := reserveFor(record.Mode, input.Size, Size{
			VCPU: record.AppliedVCPU, RAMGB: record.AppliedRAMGB,
		})
		if quotaErr := s.checkQuota(ctx, tx, actor.ID, current, candidate, false); quotaErr != nil {
			return quotaErr
		}

		values := map[string]any{
			"vcpu": input.Size.VCPU, "ram_gb": input.Size.RAMGB,
			"desired_generation": record.DesiredGeneration + 1,
			"updated_at":         now, "configuration_requested_at": now,
		}
		if updateErr := s.repository.UpdateValkeyIntent(ctx, tx, record.ID, values); updateErr != nil {
			return updateErr
		}
		applyIntentValues(&record, values)

		if billingErr := s.repository.CloseBillingPeriod(
			ctx,
			tx,
			record.ID,
			now,
			domain.BillingPeriodEndReasonResized,
		); billingErr != nil {
			return billingErr
		}
		if billingErr := s.openBillingPeriod(
			ctx,
			tx,
			record,
			domain.BillingPeriodStartReasonResized,
			now,
		); billingErr != nil {
			return billingErr
		}
		if auditErr := s.audit.Write(
			ctx,
			tx,
			auditEvent(actor, audit.ActionInstanceResize, input.RequestID, record.ID, now),
		); auditErr != nil {
			return auditErr
		}

		result = InstanceResult{Status: http.StatusAccepted, Instance: instanceDTO(record, now)}

		return s.saveInstanceResponse(ctx, tx, actor.ID, input.IdempotencyKey, requestHash, result, now)
	})

	return result, err
}

func (s *Service) UpdateWhitelist(
	ctx context.Context,
	actor Actor,
	instanceID uuid.UUID,
	input WhitelistInput,
) (InstanceResult, error) {
	normalized, err := normalizeWhitelist(input.CIDRs)
	if err != nil {
		return InstanceResult{}, err
	}

	var result InstanceResult
	err = s.beginMutation(ctx, func(tx *gorm.DB, now time.Time) error {
		record, findErr := s.repository.FindOwnedValkeyInstanceForUpdate(ctx, tx, actor.ID, instanceID)
		if findErr != nil {
			return publicStoreError(findErr)
		}
		if record.DeletionRequestedAt != nil {
			return instanceNotReady(record, "deleting")
		}
		if record.IsWhitelistEnabled == input.Enabled && slices.Equal([]string(record.WhitelistCIDRs), normalized) {
			result = InstanceResult{Status: http.StatusOK, Instance: instanceDTO(record, now)}

			return nil
		}
		if readyErr := ensureReady(record, now); readyErr != nil {
			return readyErr
		}

		values := map[string]any{
			"is_whitelist_enabled": input.Enabled, "whitelist_cidrs": pq.StringArray(normalized),
			"desired_generation": record.DesiredGeneration + 1,
			"updated_at":         now, "configuration_requested_at": now,
		}
		if updateErr := s.repository.UpdateValkeyIntent(ctx, tx, record.ID, values); updateErr != nil {
			return updateErr
		}
		applyIntentValues(&record, values)

		if auditErr := s.audit.Write(
			ctx,
			tx,
			auditEvent(actor, audit.ActionInstanceWhitelistUpdate, input.RequestID, record.ID, now),
		); auditErr != nil {
			return auditErr
		}

		result = InstanceResult{Status: http.StatusAccepted, Instance: instanceDTO(record, now)}

		return nil
	})

	return result, err
}

func (s *Service) RotatePassword(
	ctx context.Context,
	actor Actor,
	instanceID uuid.UUID,
	input RotateInput,
) (CredentialsResult, error) {
	if err := validatePassword(input.Password); err != nil {
		return CredentialsResult{}, err
	}
	if input.ExpectedPasswordVersion < 1 {
		return CredentialsResult{}, validation(map[string]string{"expected_password_version": "out_of_range"})
	}

	path := "/v1/managed/valkey/instances/" + instanceID.String() + "/credentials/rotate"
	requestHash, err := canonicalHash(http.MethodPost, path, struct {
		Password                string `json:"password"`
		ExpectedPasswordVersion int    `json:"expected_password_version"`
	}{Password: input.Password, ExpectedPasswordVersion: input.ExpectedPasswordVersion})
	if err != nil {
		return CredentialsResult{}, apierr.WrapInternal(err)
	}
	digest := passwordHash(input.Password)

	var result CredentialsResult
	err = s.beginMutation(ctx, func(tx *gorm.DB, now time.Time) error {
		record, findErr := s.repository.FindOwnedValkeyInstanceForUpdate(ctx, tx, actor.ID, instanceID)
		if findErr != nil {
			return publicStoreError(findErr)
		}

		replayed, found, replayErr := s.replayCredentials(ctx, tx, actor.ID, input.IdempotencyKey, requestHash, now)
		if replayErr != nil {
			return replayErr
		}
		if found {
			result = replayed

			return nil
		}
		if record.DeletionRequestedAt != nil {
			return instanceNotReady(record, "deleting")
		}
		if readyErr := ensureReady(record, now); readyErr != nil {
			return readyErr
		}
		if record.PasswordVersion != input.ExpectedPasswordVersion {
			return apierr.New(
				apierr.CodeConflict,
				"Версия пароля изменилась",
				map[string]any{
					"reason":                    "password_version_mismatch",
					"expected_password_version": input.ExpectedPasswordVersion,
					"password_version":          record.PasswordVersion,
				},
			)
		}
		if record.AppPasswordHash == digest {
			return validation(map[string]string{"password": "same_as_current"})
		}

		values := map[string]any{
			"app_password_hash": digest, "password_prefix": input.Password[:4],
			"password_version":   record.PasswordVersion + 1,
			"desired_generation": record.DesiredGeneration + 1,
			"updated_at":         now, "configuration_requested_at": now,
		}
		if updateErr := s.repository.UpdateValkeyIntent(ctx, tx, record.ID, values); updateErr != nil {
			return updateErr
		}
		applyIntentValues(&record, values)

		if auditErr := s.audit.Write(
			ctx,
			tx,
			auditEvent(actor, audit.ActionInstancePasswordRotate, input.RequestID, record.ID, now),
		); auditErr != nil {
			return auditErr
		}

		result = CredentialsResult{Status: http.StatusAccepted, Credentials: credentialsDTO(record)}

		return s.saveCredentialsResponse(ctx, tx, actor.ID, input.IdempotencyKey, requestHash, result, now)
	})

	return result, err
}

func (s *Service) Delete(ctx context.Context, actor Actor, instanceID uuid.UUID, requestID string) error {
	return s.beginMutation(ctx, func(tx *gorm.DB, now time.Time) error {
		record, findErr := s.repository.FindOwnedValkeyInstanceForUpdate(ctx, tx, actor.ID, instanceID)
		if findErr != nil {
			return publicStoreError(findErr)
		}
		if record.DeletionRequestedAt != nil {
			return nil
		}

		if updateErr := s.repository.UpdateValkeyIntent(ctx, tx, record.ID, map[string]any{
			"deletion_requested_at": now,
			"updated_at":            now,
		}); updateErr != nil {
			return updateErr
		}
		if billingErr := s.repository.CloseBillingPeriod(
			ctx,
			tx,
			record.ID,
			now,
			domain.BillingPeriodEndReasonDeleted,
		); billingErr != nil {
			return billingErr
		}

		return s.audit.Write(
			ctx,
			tx,
			auditEvent(actor, audit.ActionInstanceDelete, requestID, record.ID, now),
		)
	})
}

func (s *Service) validateCreate(input CreateInput) ([]string, error) {
	if err := validateName(input.Name); err != nil {
		return nil, err
	}
	if err := validatePrefix(input.Prefix); err != nil {
		return nil, err
	}
	if err := validateMode(input.Mode); err != nil {
		return nil, err
	}
	if !s.catalog.HasSize(input.Size) {
		return nil, validation(map[string]string{"size": "unsupported"})
	}
	if err := validatePassword(input.Password); err != nil {
		return nil, err
	}

	return normalizeWhitelist(input.WhitelistCIDRs)
}

func (s *Service) createWithUniqueSlug(
	ctx context.Context,
	tx *gorm.DB,
	actor Actor,
	input CreateInput,
	cidrs []string,
	digest string,
	now time.Time,
) (store.ValkeyInstance, error) {
	for range 100 {
		suffix, err := s.slugs.Suffix()
		if err != nil {
			return store.ValkeyInstance{}, err
		}

		slug := input.Prefix + "-" + suffix
		record := store.ValkeyInstance{
			UserID: actor.ID, UserEmail: actor.Email, Name: input.Name, Slug: slug, Mode: input.Mode,
			VCPU: input.Size.VCPU, RAMGB: input.Size.RAMGB, DesiredGeneration: 1,
			CreatedAt: now, UpdatedAt: now, ConfigurationRequestedAt: now,
			AppPasswordHash: digest, PasswordPrefix: input.Password[:4], PasswordVersion: 1,
			Host: slug + "." + s.catalog.Connection.Domain, Port: s.catalog.Connection.Port,
			IsWhitelistEnabled: input.WhitelistEnabled, WhitelistCIDRs: pq.StringArray(cidrs),
			Phase:                     domain.ValkeyInstancePhaseProvisioning,
			NetworkVerificationStatus: domain.ValkeyNetworkVerificationPending,
		}
		if input.Mode == domain.ValkeyInstanceModeHA {
			hostRO := slug + "-ro." + s.catalog.Connection.Domain
			record.HostRO = &hostRO
		}

		created, createErr := s.repository.CreateValkeyInstance(ctx, tx, &record)
		if createErr != nil {
			if errors.Is(createErr, store.ErrValkeyNameConflict) {
				return store.ValkeyInstance{}, nameConflict()
			}

			return store.ValkeyInstance{}, createErr
		}
		if created {
			return record, nil
		}
	}

	return store.ValkeyInstance{}, errors.New("исчерпаны попытки подобрать slug Valkey")
}

func (s *Service) checkQuota(
	ctx context.Context,
	tx *gorm.DB,
	userID uuid.UUID,
	current Size,
	candidate Size,
	isCreate bool,
) error {
	quota, err := s.repository.GetQuotaInTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	userUsage, err := s.repository.ValkeyUsage(ctx, tx, &userID)
	if err != nil {
		return err
	}
	userRequested := Size{
		VCPU:  userUsage.VCPU - current.VCPU + candidate.VCPU,
		RAMGB: userUsage.RAMGB - current.RAMGB + candidate.RAMGB,
	}
	if exceeds(userRequested.VCPU, userUsage.VCPU, quota.MaxVCPU) ||
		exceeds(userRequested.RAMGB, userUsage.RAMGB, quota.MaxRAMGB) {
		return resourceError(
			apierr.CodeQuotaExceeded,
			"Недостаточно квоты пользователя",
			"user_quota",
			Size{VCPU: quota.MaxVCPU, RAMGB: quota.MaxRAMGB},
			Size{VCPU: userUsage.VCPU, RAMGB: userUsage.RAMGB},
			userRequested,
		)
	}

	clusterUsage, err := s.repository.ValkeyUsage(ctx, tx, nil)
	if err != nil {
		return err
	}
	if isCreate && clusterUsage.Instances+1 > InstanceLimit {
		return apierr.New(
			apierr.CodeNotEnoughResources,
			"Достигнут предел числа инстансов",
			map[string]any{
				"reason": "instance_limit", "limit": InstanceLimit,
				"used": clusterUsage.Instances, "requested": clusterUsage.Instances + 1,
			},
		)
	}

	clusterRequested := Size{
		VCPU:  clusterUsage.VCPU - current.VCPU + candidate.VCPU,
		RAMGB: clusterUsage.RAMGB - current.RAMGB + candidate.RAMGB,
	}
	if exceeds(clusterRequested.VCPU, clusterUsage.VCPU, s.clusterVCPU) ||
		exceeds(clusterRequested.RAMGB, clusterUsage.RAMGB, s.clusterRAM) {
		return resourceError(
			apierr.CodeNotEnoughResources,
			"Недостаточно свободной квоты кластера",
			"cluster_quota",
			Size{VCPU: s.clusterVCPU, RAMGB: s.clusterRAM},
			Size{VCPU: clusterUsage.VCPU, RAMGB: clusterUsage.RAMGB},
			clusterRequested,
		)
	}

	return nil
}

func resourceError(code apierr.Code, message, reason string, limit, used, requested Size) error {
	return apierr.New(code, message, map[string]any{
		"reason":    reason,
		"limit":     map[string]int{"vcpu": limit.VCPU, "ram_gb": limit.RAMGB},
		"used":      map[string]int{"vcpu": used.VCPU, "ram_gb": used.RAMGB},
		"requested": map[string]int{"vcpu": requested.VCPU, "ram_gb": requested.RAMGB},
		"missing": map[string]int{
			"vcpu":   max(0, requested.VCPU-limit.VCPU),
			"ram_gb": max(0, requested.RAMGB-limit.RAMGB),
		},
	})
}

func exceeds(requested, used, limit int) bool {
	return requested > limit && requested > used
}

func reserveFor(mode domain.ValkeyInstanceMode, desired, applied Size) Size {
	reserved, _ := Reserve(mode, desired, applied)

	return reserved
}

func (s *Service) openBillingPeriod(
	ctx context.Context,
	tx *gorm.DB,
	record store.ValkeyInstance,
	reason domain.BillingPeriodStartReason,
	now time.Time,
) error {
	price, err := s.catalog.PriceCoinsPerHour(record.Mode, Size{VCPU: record.VCPU, RAMGB: record.RAMGB})
	if err != nil {
		return err
	}
	nodes, err := NodeCount(record.Mode)
	if err != nil {
		return err
	}

	return s.repository.CreateBillingPeriod(ctx, tx, &store.BillingPeriod{
		UserID: record.UserID, Service: domain.ManagedServiceValkey, ResourceID: record.ID,
		StartedAt: now, Mode: record.Mode, VCPU: record.VCPU, RAMGB: record.RAMGB,
		NodeCount: nodes, PriceCoinsPerHour: price, StartedReason: reason,
	})
}

func canonicalHash(method, path string, body any) (string, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("закодировать канонический запрос: %w", err)
	}
	digest := sha256.Sum256(append([]byte(method+"\n"+path+"\n"), encoded...))

	return hex.EncodeToString(digest[:]), nil
}

func (s *Service) replayInstance(
	ctx context.Context,
	tx *gorm.DB,
	userID uuid.UUID,
	keyID uuid.UUID,
	requestHash string,
	now time.Time,
) (InstanceResult, bool, error) {
	key, found, err := s.liveIdempotencyKey(ctx, tx, userID, keyID, requestHash, now)
	if err != nil || !found {
		return InstanceResult{}, false, err
	}

	var response Instance
	if err := json.Unmarshal(key.ResponseBody, &response); err != nil {
		return InstanceResult{}, false, fmt.Errorf("разобрать сохранённый ответ инстанса: %w", err)
	}

	return InstanceResult{Status: key.ResponseStatus, Instance: response, Replay: true}, true, nil
}

func (s *Service) replayCredentials(
	ctx context.Context,
	tx *gorm.DB,
	userID uuid.UUID,
	keyID uuid.UUID,
	requestHash string,
	now time.Time,
) (CredentialsResult, bool, error) {
	key, found, err := s.liveIdempotencyKey(ctx, tx, userID, keyID, requestHash, now)
	if err != nil || !found {
		return CredentialsResult{}, false, err
	}

	var response Credentials
	if err := json.Unmarshal(key.ResponseBody, &response); err != nil {
		return CredentialsResult{}, false, fmt.Errorf("разобрать сохранённый ответ credentials: %w", err)
	}

	return CredentialsResult{Status: key.ResponseStatus, Credentials: response, Replay: true}, true, nil
}

func (s *Service) liveIdempotencyKey(
	ctx context.Context,
	tx *gorm.DB,
	userID uuid.UUID,
	keyID uuid.UUID,
	requestHash string,
	now time.Time,
) (store.IdempotencyKey, bool, error) {
	key, found, err := s.repository.FindIdempotencyKey(ctx, tx, userID, keyID)
	if err != nil || !found {
		return store.IdempotencyKey{}, false, err
	}
	if !key.CreatedAt.After(now.Add(-idempotencyTTL)) {
		if err := s.repository.DeleteIdempotencyKey(ctx, tx, userID, keyID); err != nil {
			return store.IdempotencyKey{}, false, err
		}

		return store.IdempotencyKey{}, false, nil
	}
	if key.RequestHash != requestHash {
		return store.IdempotencyKey{}, false, apierr.New(
			apierr.CodeIdempotencyMismatch,
			"Ключ идемпотентности использован для другого запроса",
			nil,
		)
	}

	return key, true, nil
}

func (s *Service) saveInstanceResponse(
	ctx context.Context,
	tx *gorm.DB,
	userID uuid.UUID,
	keyID uuid.UUID,
	requestHash string,
	result InstanceResult,
	now time.Time,
) error {
	return s.saveResponse(ctx, tx, userID, keyID, requestHash, result.Status, result.Instance, now)
}

func (s *Service) saveCredentialsResponse(
	ctx context.Context,
	tx *gorm.DB,
	userID uuid.UUID,
	keyID uuid.UUID,
	requestHash string,
	result CredentialsResult,
	now time.Time,
) error {
	return s.saveResponse(ctx, tx, userID, keyID, requestHash, result.Status, result.Credentials, now)
}

func (s *Service) saveResponse(
	ctx context.Context,
	tx *gorm.DB,
	userID uuid.UUID,
	keyID uuid.UUID,
	requestHash string,
	status int,
	response any,
	now time.Time,
) error {
	body, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("закодировать ответ идемпотентности: %w", err)
	}

	return s.repository.CreateIdempotencyKey(ctx, tx, &store.IdempotencyKey{
		UserID: userID, Key: keyID, RequestHash: requestHash,
		ResponseStatus: status, ResponseBody: body, CreatedAt: now,
	})
}

func maintenanceMatches(record store.ValkeyInstance, value *Maintenance) bool {
	if value == nil {
		return record.MaintenanceDOW == nil && record.MaintenanceHourUTC == nil && record.MaintenanceDurationMin == nil
	}

	return record.MaintenanceDOW != nil && record.MaintenanceHourUTC != nil &&
		record.MaintenanceDurationMin != nil && *record.MaintenanceDOW == value.DOW &&
		*record.MaintenanceHourUTC == value.HourUTC && *record.MaintenanceDurationMin == value.DurationMin
}

func applyIntentValues(record *store.ValkeyInstance, values map[string]any) {
	if value, ok := values["name"].(string); ok {
		record.Name = value
	}
	if value, ok := values["vcpu"].(int); ok {
		record.VCPU = value
	}
	if value, ok := values["ram_gb"].(int); ok {
		record.RAMGB = value
	}
	if value, ok := values["desired_generation"].(int); ok {
		record.DesiredGeneration = value
	}
	if value, ok := values["app_password_hash"].(string); ok {
		record.AppPasswordHash = value
	}
	if value, ok := values["password_prefix"].(string); ok {
		record.PasswordPrefix = value
	}
	if value, ok := values["password_version"].(int); ok {
		record.PasswordVersion = value
	}
	if value, ok := values["is_whitelist_enabled"].(bool); ok {
		record.IsWhitelistEnabled = value
	}
	if value, ok := values["whitelist_cidrs"].(pq.StringArray); ok {
		record.WhitelistCIDRs = value
	}
	if value, ok := values["updated_at"].(time.Time); ok {
		record.UpdatedAt = value
	}
	if value, ok := values["configuration_requested_at"].(time.Time); ok {
		record.ConfigurationRequestedAt = value
	}
	if value, exists := values["maintenance_dow"]; exists {
		record.MaintenanceDOW = integerPointer(value)
		record.MaintenanceHourUTC = integerPointer(values["maintenance_hour_utc"])
		record.MaintenanceDurationMin = integerPointer(values["maintenance_duration_min"])
	}
}

func integerPointer(value any) *int {
	integer, ok := value.(int)
	if !ok {
		return nil
	}

	return &integer
}
