package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
)

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
