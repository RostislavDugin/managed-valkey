package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/platformhealth"
)

const (
	HeaderValkeyPrimary = "X-Valkey-Primary"
	HeaderValkeyRead    = "X-Valkey-Read"
)

type PlatformHealthService interface {
	Check(context.Context) platformhealth.PlatformReport
}

type ValkeyHealthService interface {
	Check(context.Context, string, string) (platformhealth.ValkeyReport, error)
}

// Готовность зависит только от PostgreSQL. Недоступный Kubernetes не делает API
// неготовым: вход и чтение сохранённого состояния должны работать, пока
// control plane недоступен, а фоновые циклы повторяют обращения сами.
func RegisterHealth(engine *gin.Engine, database Probe) {
	engine.GET(PathLive, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	engine.GET(PathReady, func(c *gin.Context) {
		if err := database.Ping(c.Request.Context()); err != nil {
			LoggerFrom(c.Request.Context()).Warn("проверка готовности не прошла", "error", err)

			apierr.Write(
				c,
				apierr.New(
					apierr.CodeUnavailable,
					"Сервис временно недоступен",
					map[string]any{"dependency": "postgresql"},
				),
			)

			return
		}

		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
}

func RegisterOperationalHealth(
	engine *gin.Engine,
	platformService PlatformHealthService,
	valkeyService ValkeyHealthService,
) {
	engine.GET(PathHealth, func(c *gin.Context) {
		report := platformService.Check(c.Request.Context())
		c.Header("Cache-Control", "no-store")
		c.JSON(healthStatus(report.Status), report)
	})

	engine.GET(PathValkeyHealth, func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		primary, read, err := valkeyHealthHeaders(c.Request.Header)
		if err != nil {
			apierr.Write(c, err)

			return
		}

		report, err := valkeyService.Check(c.Request.Context(), primary, read)
		if err != nil {
			if uriError, ok := errors.AsType[*platformhealth.URIError](err); ok {
				apierr.Write(c, invalidValkeyHealthHeader(uriError.Field, "invalid_value"))

				return
			}

			apierr.Write(c, err)

			return
		}

		logValkeyHealthFailures(c, report)
		c.JSON(healthStatus(report.Status), report)
	})
}

func healthStatus(status platformhealth.Status) int {
	if status == platformhealth.StatusFail {
		return http.StatusServiceUnavailable
	}

	return http.StatusOK
}

func valkeyHealthHeaders(header http.Header) (string, string, error) {
	primary := header.Values(HeaderValkeyPrimary)
	if len(primary) == 0 || primary[0] == "" {
		return "", "", invalidValkeyHealthHeader(HeaderValkeyPrimary, "required")
	}
	if len(primary) != 1 {
		return "", "", invalidValkeyHealthHeader(HeaderValkeyPrimary, "repeated_header")
	}

	read := header.Values(HeaderValkeyRead)
	if len(read) > 1 {
		return "", "", invalidValkeyHealthHeader(HeaderValkeyRead, "repeated_header")
	}
	if len(read) == 1 {
		if read[0] == "" {
			return "", "", invalidValkeyHealthHeader(HeaderValkeyRead, "invalid_value")
		}

		return primary[0], read[0], nil
	}

	return primary[0], "", nil
}

func invalidValkeyHealthHeader(field, reason string) error {
	return apierr.New(
		apierr.CodeValidationFailed,
		"Проверьте заголовки Valkey",
		map[string]any{"fields": map[string]string{field: reason}},
	)
}

func logValkeyHealthFailures(c *gin.Context, report platformhealth.ValkeyReport) {
	if report.Checks.Primary.Status == platformhealth.StatusFail {
		LoggerFrom(c.Request.Context()).Warn(
			"проверка Valkey не прошла",
			"role", "primary",
			"code", report.Checks.Primary.Code,
			"duration_ms", report.Checks.Primary.LatencyMS,
		)
	}
	if report.Checks.Read != nil && report.Checks.Read.Status == platformhealth.StatusFail {
		LoggerFrom(c.Request.Context()).Warn(
			"проверка Valkey не прошла",
			"role", "read",
			"code", report.Checks.Read.Code,
			"duration_ms", report.Checks.Read.LatencyMS,
		)
	}
}
