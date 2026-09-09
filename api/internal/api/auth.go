package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/auth"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

const (
	userIDContextKey = "authenticated_user_id"
	userContextKey   = "authenticated_user"
)

type AuthService interface {
	CheckEmail(context.Context, string) (bool, error)
	Register(context.Context, auth.Registration) (string, error)
	Login(context.Context, auth.Credentials, string) (string, error)
	Authenticate(context.Context, string) (store.User, error)
	Me(context.Context, uuid.UUID) (auth.CurrentUser, error)
}

type emailRequest struct {
	Email string `json:"email"`
}

type credentialsRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func RegisterAuth(engine *gin.Engine, service AuthService, limiter *RateLimiter) {
	authRoutes := engine.Group("/v1/auth", RateLimit(limiter))
	authRoutes.POST("/check-email", checkEmailHandler(service))
	authRoutes.POST("/register", registerHandler(service))
	authRoutes.POST("/login", loginHandler(service))

	protected := engine.Group("/v1", BearerAuth(service))
	protected.GET("/me", meHandler(service))
}

func checkEmailHandler(service AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request emailRequest
		if err := decodeJSON(c, &request); err != nil {
			apierr.Write(c, err)

			return
		}

		exists, err := service.CheckEmail(c.Request.Context(), request.Email)
		if err != nil {
			apierr.Write(c, err)

			return
		}

		c.JSON(http.StatusOK, gin.H{"exists": exists})
	}
}

func registerHandler(service AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		key, err := uuid.Parse(c.GetHeader("Idempotency-Key"))
		if err != nil {
			apierr.Write(
				c,
				apierr.New(
					apierr.CodeValidationFailed,
					"Передайте Idempotency-Key в формате UUID",
					map[string]any{"field": "Idempotency-Key"},
				),
			)

			return
		}

		var request credentialsRequest
		if err := decodeJSON(c, &request); err != nil {
			apierr.Write(c, err)

			return
		}

		credentials := auth.Credentials{Email: request.Email, Password: request.Password}
		token, err := service.Register(c.Request.Context(), auth.Registration{
			Credentials: credentials,
			Key:         key, RequestID: RequestIDFrom(c.Request.Context()),
		})
		if err != nil {
			apierr.Write(c, err)

			return
		}

		c.JSON(http.StatusOK, gin.H{"token": token})
	}
}

func loginHandler(service AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request credentialsRequest
		if err := decodeJSON(c, &request); err != nil {
			apierr.Write(c, err)

			return
		}

		token, err := service.Login(
			c.Request.Context(),
			auth.Credentials{Email: request.Email, Password: request.Password},
			RequestIDFrom(c.Request.Context()),
		)
		if err != nil {
			apierr.Write(c, err)

			return
		}

		c.JSON(http.StatusOK, gin.H{"token": token})
	}
}

func BearerAuth(service AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")) == "" {
			apierr.Write(c, apierr.New(apierr.CodeUnauthorized, "Требуется авторизация", nil))

			return
		}

		user, err := service.Authenticate(c.Request.Context(), strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")))
		if err != nil {
			apierr.Write(c, err)

			return
		}

		c.Set(userIDContextKey, user.ID)
		c.Set(userContextKey, user)
		requestLogger := LoggerFrom(c.Request.Context()).With("user_id", user.ID.String())
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), loggerKey, requestLogger))
		c.Next()
	}
}

func meHandler(service AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userIDValue, ok := c.Get(userIDContextKey)
		if !ok {
			apierr.Write(c, apierr.New(apierr.CodeUnauthorized, "Требуется авторизация", nil))

			return
		}
		userID, ok := userIDValue.(uuid.UUID)
		if !ok {
			apierr.Write(c, apierr.New(apierr.CodeInternal, "Внутренняя ошибка сервера", nil))

			return
		}

		current, err := service.Me(c.Request.Context(), userID)
		if err != nil {
			apierr.Write(c, err)

			return
		}

		c.JSON(http.StatusOK, gin.H{
			"user":  gin.H{"id": current.ID, "email": current.Email},
			"quota": gin.H{"max_vcpu": current.MaxVCPU, "max_ram_gb": current.MaxRAMGB},
			"usage": gin.H{"used_vcpu": current.UsedVCPU, "used_ram_gb": current.UsedRAMGB},
		})
	}
}
