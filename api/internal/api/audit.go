package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

func valkeyAuditHandler(service AuditService) gin.HandlerFunc {
	return func(c *gin.Context) {
		request, err := auditPageRequest(c)
		if err != nil {
			apierr.Write(c, err)

			return
		}
		_, instanceID, err := valkeyRequestContext(c)
		if err != nil {
			apierr.Write(c, err)

			return
		}

		page, err := service.ListResource(
			c.Request.Context(),
			domain.ManagedServiceValkey,
			instanceID,
			request,
		)
		if err != nil {
			apierr.Write(c, err)

			return
		}

		c.JSON(http.StatusOK, page)
	}
}

func auditPageRequest(c *gin.Context) (audit.PageRequest, error) {
	query := c.Request.URL.Query()
	for key, values := range query {
		if key != "limit" && key != "before" {
			return audit.PageRequest{}, apierr.New(
				apierr.CodeValidationFailed,
				"Параметр запроса не поддерживается",
				map[string]any{"fields": map[string]string{key: "unknown_parameter"}},
			)
		}
		if len(values) != 1 {
			return audit.PageRequest{}, apierr.New(
				apierr.CodeValidationFailed,
				"Параметр запроса передан несколько раз",
				map[string]any{"fields": map[string]string{key: "repeated_parameter"}},
			)
		}
	}

	return audit.ParsePageRequest(queryValue(query, "limit"), queryValue(query, "before"))
}

func queryValue(query map[string][]string, key string) *string {
	values, ok := query[key]
	if !ok {
		return nil
	}

	return &values[0]
}
