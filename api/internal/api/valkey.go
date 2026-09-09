package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

const instanceIDContextKey = "valkey_instance_id"

type ValkeyService interface {
	Sizes() valkeydomain.Catalog
	Owns(context.Context, valkeydomain.Actor, uuid.UUID) error
	List(context.Context, valkeydomain.Actor) ([]valkeydomain.Instance, error)
	Get(context.Context, valkeydomain.Actor, uuid.UUID) (valkeydomain.Instance, error)
	GetCredentials(context.Context, valkeydomain.Actor, uuid.UUID) (valkeydomain.Credentials, error)
	Create(context.Context, valkeydomain.Actor, valkeydomain.CreateInput) (valkeydomain.InstanceResult, error)
	Patch(context.Context, valkeydomain.Actor, uuid.UUID, valkeydomain.PatchInput) (valkeydomain.InstanceResult, error)
	Resize(
		context.Context,
		valkeydomain.Actor,
		uuid.UUID,
		valkeydomain.ResizeInput,
	) (valkeydomain.InstanceResult, error)
	UpdateWhitelist(
		context.Context,
		valkeydomain.Actor,
		uuid.UUID,
		valkeydomain.WhitelistInput,
	) (valkeydomain.InstanceResult, error)
	RotatePassword(
		context.Context,
		valkeydomain.Actor,
		uuid.UUID,
		valkeydomain.RotateInput,
	) (valkeydomain.CredentialsResult, error)
	Delete(context.Context, valkeydomain.Actor, uuid.UUID, string) error
}

type AuditService interface {
	ListResource(
		context.Context,
		domain.ManagedService,
		uuid.UUID,
		audit.PageRequest,
	) (audit.Page, error)
}

type createValkeyRequest struct {
	Name             string                    `json:"name"`
	Prefix           string                    `json:"prefix"`
	Mode             domain.ValkeyInstanceMode `json:"mode"`
	VCPU             int                       `json:"vcpu"`
	RAMGB            int                       `json:"ram_gb"`
	Password         string                    `json:"password"`
	WhitelistEnabled bool                      `json:"is_whitelist_enabled"`
	WhitelistCIDRs   []string                  `json:"whitelist_cidrs"`
}

type resizeValkeyRequest struct {
	VCPU  int `json:"vcpu"`
	RAMGB int `json:"ram_gb"`
}

type whitelistValkeyRequest struct {
	Enabled *bool     `json:"is_whitelist_enabled"`
	CIDRs   *[]string `json:"whitelist_cidrs"`
}

type rotateValkeyPasswordRequest struct {
	Password                string `json:"password"`
	ExpectedPasswordVersion int    `json:"expected_password_version"`
}

type patchValkeyRequest struct {
	Name        json.RawMessage `json:"name"`
	Maintenance json.RawMessage `json:"maintenance"`
}

type maintenanceRequest struct {
	DOW         *int `json:"dow"`
	HourUTC     *int `json:"hour_utc"`
	DurationMin *int `json:"duration_min"`
}

func RegisterValkey(
	engine *gin.Engine,
	authService AuthService,
	service ValkeyService,
	auditService AuditService,
) {
	managed := engine.Group("/v1/managed/valkey", BearerAuth(authService))
	managed.GET("/sizes", valkeySizesHandler(service))
	managed.GET("/instances", valkeyListHandler(service))
	managed.POST("/instances", valkeyCreateHandler(service))

	owned := managed.Group("/instances/:id", valkeyOwner(service))
	owned.GET("", valkeyGetHandler(service))
	owned.PATCH("", valkeyPatchHandler(service))
	owned.POST("/resize", valkeyResizeHandler(service))
	owned.PUT("/whitelist", valkeyWhitelistHandler(service))
	owned.GET("/credentials", valkeyCredentialsHandler(service))
	owned.POST("/credentials/rotate", valkeyRotateHandler(service))
	owned.GET("/audit", valkeyAuditHandler(auditService))
	owned.DELETE("", valkeyDeleteHandler(service))
}

func valkeyOwner(service ValkeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		instanceID, err := uuid.Parse(c.Param("id"))
		if err != nil {
			apierr.Write(c, apierr.New(
				apierr.CodeValidationFailed,
				"Проверьте идентификатор инстанса",
				map[string]any{"fields": map[string]string{"id": "invalid_uuid"}},
			))

			return
		}

		actor, err := authenticatedActor(c)
		if err != nil {
			apierr.Write(c, err)

			return
		}
		if err := service.Owns(c.Request.Context(), actor, instanceID); err != nil {
			apierr.Write(c, err)

			return
		}

		c.Set(instanceIDContextKey, instanceID)
		c.Next()
	}
}

func valkeySizesHandler(service ValkeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, service.Sizes())
	}
}

func valkeyListHandler(service ValkeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, err := authenticatedActor(c)
		if err != nil {
			apierr.Write(c, err)

			return
		}
		instances, err := service.List(c.Request.Context(), actor)
		if err != nil {
			apierr.Write(c, err)

			return
		}

		c.JSON(http.StatusOK, gin.H{"items": instances})
	}
}

func valkeyGetHandler(service ValkeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, instanceID, err := valkeyRequestContext(c)
		if err != nil {
			apierr.Write(c, err)

			return
		}
		instance, err := service.Get(c.Request.Context(), actor, instanceID)
		if err != nil {
			apierr.Write(c, err)

			return
		}

		c.JSON(http.StatusOK, instance)
	}
}

func valkeyCredentialsHandler(service ValkeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, instanceID, err := valkeyRequestContext(c)
		if err != nil {
			apierr.Write(c, err)

			return
		}
		credentials, err := service.GetCredentials(c.Request.Context(), actor, instanceID)
		if err != nil {
			apierr.Write(c, err)

			return
		}

		c.JSON(http.StatusOK, credentials)
	}
}

func valkeyCreateHandler(service ValkeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := rejectMutationQuery(c); err != nil {
			apierr.Write(c, err)

			return
		}
		key, err := idempotencyKey(c)
		if err != nil {
			apierr.Write(c, err)

			return
		}
		var request createValkeyRequest
		if err := decodeJSON(c, &request); err != nil {
			apierr.Write(c, err)

			return
		}
		actor, err := authenticatedActor(c)
		if err != nil {
			apierr.Write(c, err)

			return
		}

		result, err := service.Create(c.Request.Context(), actor, valkeydomain.CreateInput{
			Name: request.Name, Prefix: request.Prefix, Mode: request.Mode,
			Size:     valkeydomain.Size{VCPU: request.VCPU, RAMGB: request.RAMGB},
			Password: request.Password, WhitelistEnabled: request.WhitelistEnabled,
			WhitelistCIDRs: request.WhitelistCIDRs, IdempotencyKey: key,
			RequestID: RequestIDFrom(c.Request.Context()),
		})
		if err != nil {
			apierr.Write(c, err)

			return
		}

		c.JSON(result.Status, result.Instance)
	}
}

func valkeyPatchHandler(service ValkeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := rejectMutationQuery(c); err != nil {
			apierr.Write(c, err)

			return
		}
		var request patchValkeyRequest
		if err := decodeJSON(c, &request); err != nil {
			apierr.Write(c, err)

			return
		}

		input, err := parsePatchRequest(request)
		if err != nil {
			apierr.Write(c, err)

			return
		}
		actor, instanceID, err := valkeyRequestContext(c)
		if err != nil {
			apierr.Write(c, err)

			return
		}
		input.RequestID = RequestIDFrom(c.Request.Context())

		result, err := service.Patch(c.Request.Context(), actor, instanceID, input)
		if err != nil {
			apierr.Write(c, err)

			return
		}

		c.JSON(result.Status, result.Instance)
	}
}

func valkeyResizeHandler(service ValkeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		key, request, actor, instanceID, err := decodeIdempotentInstanceRequest[resizeValkeyRequest](c)
		if err != nil {
			apierr.Write(c, err)

			return
		}
		result, err := service.Resize(c.Request.Context(), actor, instanceID, valkeydomain.ResizeInput{
			Size:           valkeydomain.Size{VCPU: request.VCPU, RAMGB: request.RAMGB},
			IdempotencyKey: key, RequestID: RequestIDFrom(c.Request.Context()),
		})
		if err != nil {
			apierr.Write(c, err)

			return
		}

		c.JSON(result.Status, result.Instance)
	}
}

func valkeyWhitelistHandler(service ValkeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := rejectMutationQuery(c); err != nil {
			apierr.Write(c, err)

			return
		}
		var request whitelistValkeyRequest
		if err := decodeJSON(c, &request); err != nil {
			apierr.Write(c, err)

			return
		}
		if request.Enabled == nil || request.CIDRs == nil {
			apierr.Write(c, invalidJSON("invalid_json"))

			return
		}
		actor, instanceID, err := valkeyRequestContext(c)
		if err != nil {
			apierr.Write(c, err)

			return
		}

		result, err := service.UpdateWhitelist(c.Request.Context(), actor, instanceID, valkeydomain.WhitelistInput{
			Enabled: *request.Enabled, CIDRs: *request.CIDRs, RequestID: RequestIDFrom(c.Request.Context()),
		})
		if err != nil {
			apierr.Write(c, err)

			return
		}

		c.JSON(result.Status, result.Instance)
	}
}

func valkeyRotateHandler(service ValkeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		key, request, actor, instanceID, err := decodeIdempotentInstanceRequest[rotateValkeyPasswordRequest](c)
		if err != nil {
			apierr.Write(c, err)

			return
		}
		result, err := service.RotatePassword(c.Request.Context(), actor, instanceID, valkeydomain.RotateInput{
			Password: request.Password, ExpectedPasswordVersion: request.ExpectedPasswordVersion,
			IdempotencyKey: key, RequestID: RequestIDFrom(c.Request.Context()),
		})
		if err != nil {
			apierr.Write(c, err)

			return
		}

		c.JSON(result.Status, result.Credentials)
	}
}

func valkeyDeleteHandler(service ValkeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := rejectMutationQuery(c); err != nil {
			apierr.Write(c, err)

			return
		}
		actor, instanceID, err := valkeyRequestContext(c)
		if err != nil {
			apierr.Write(c, err)

			return
		}
		if err := service.Delete(
			c.Request.Context(),
			actor,
			instanceID,
			RequestIDFrom(c.Request.Context()),
		); err != nil {
			apierr.Write(c, err)

			return
		}

		c.Status(http.StatusAccepted)
	}
}

func parsePatchRequest(request patchValkeyRequest) (valkeydomain.PatchInput, error) {
	var input valkeydomain.PatchInput
	if request.Name != nil {
		var name string
		if bytes.Equal(bytes.TrimSpace(request.Name), []byte("null")) || json.Unmarshal(request.Name, &name) != nil {
			return valkeydomain.PatchInput{}, invalidJSON("invalid_json")
		}
		input.Name = &name
	}
	if request.Maintenance != nil {
		input.MaintenanceSet = true
		if !bytes.Equal(bytes.TrimSpace(request.Maintenance), []byte("null")) {
			var maintenance maintenanceRequest
			decoder := json.NewDecoder(bytes.NewReader(request.Maintenance))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&maintenance); err != nil {
				return valkeydomain.PatchInput{}, invalidJSON("invalid_json")
			}
			if maintenance.DOW == nil || maintenance.HourUTC == nil || maintenance.DurationMin == nil {
				return valkeydomain.PatchInput{}, invalidJSON("invalid_json")
			}
			input.Maintenance = &valkeydomain.Maintenance{
				DOW: *maintenance.DOW, HourUTC: *maintenance.HourUTC, DurationMin: *maintenance.DurationMin,
			}
		}
	}

	return input, nil
}

func decodeIdempotentInstanceRequest[T any](
	c *gin.Context,
) (uuid.UUID, T, valkeydomain.Actor, uuid.UUID, error) {
	var request T
	if err := rejectMutationQuery(c); err != nil {
		return uuid.Nil, request, valkeydomain.Actor{}, uuid.Nil, err
	}
	key, err := idempotencyKey(c)
	if err != nil {
		return uuid.Nil, request, valkeydomain.Actor{}, uuid.Nil, err
	}
	if err := decodeJSON(c, &request); err != nil {
		return uuid.Nil, request, valkeydomain.Actor{}, uuid.Nil, err
	}
	actor, instanceID, err := valkeyRequestContext(c)

	return key, request, actor, instanceID, err
}

func idempotencyKey(c *gin.Context) (uuid.UUID, error) {
	key, err := uuid.Parse(c.GetHeader("Idempotency-Key"))
	if err != nil {
		return uuid.Nil, apierr.New(
			apierr.CodeValidationFailed,
			"Передайте Idempotency-Key в формате UUID",
			map[string]any{"fields": map[string]string{"Idempotency-Key": "invalid_uuid"}},
		)
	}

	return key, nil
}

func rejectMutationQuery(c *gin.Context) error {
	if len(c.Request.URL.Query()) == 0 {
		return nil
	}

	return apierr.New(
		apierr.CodeValidationFailed,
		"Параметры запроса не поддерживаются",
		map[string]any{"reason": "unknown_query_parameter"},
	)
}

func authenticatedActor(c *gin.Context) (valkeydomain.Actor, error) {
	value, ok := c.Get(userContextKey)
	if !ok {
		return valkeydomain.Actor{}, apierr.New(apierr.CodeUnauthorized, "Требуется авторизация", nil)
	}
	user, ok := value.(store.User)
	if !ok {
		return valkeydomain.Actor{}, apierr.New(apierr.CodeInternal, "Внутренняя ошибка сервера", nil)
	}

	return valkeydomain.Actor{ID: user.ID, Email: user.Email}, nil
}

func valkeyRequestContext(c *gin.Context) (valkeydomain.Actor, uuid.UUID, error) {
	actor, err := authenticatedActor(c)
	if err != nil {
		return valkeydomain.Actor{}, uuid.Nil, err
	}
	value, ok := c.Get(instanceIDContextKey)
	if !ok {
		return valkeydomain.Actor{}, uuid.Nil, apierr.New(apierr.CodeInternal, "Внутренняя ошибка сервера", nil)
	}
	instanceID, ok := value.(uuid.UUID)
	if !ok {
		return valkeydomain.Actor{}, uuid.Nil, apierr.New(apierr.CodeInternal, "Внутренняя ошибка сервера", nil)
	}

	return actor, instanceID, nil
}
