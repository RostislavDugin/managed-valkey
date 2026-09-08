package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
)

const (
	HeaderRequestID = "X-Request-Id"
	AttrRequestID   = "request_id"
)

type contextKey struct{ name string }

var (
	requestIDKey = contextKey{name: AttrRequestID}
	loggerKey    = contextKey{name: "logger"}
)

// Идентификатор клиента переиспользуется, чтобы сквозная трассировка не
// рвалась на границе сервиса.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader(HeaderRequestID)
		if requestID == "" {
			requestID = uuid.NewString()
		}

		c.Header(HeaderRequestID, requestID)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), requestIDKey, requestID))

		c.Next()
	}
}

// В запись попадают только метаданные. Тело запроса, тело ответа и заголовки,
// включая Authorization и Cookie, не логируются ни на одном уровне.
func RequestLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()

		requestLogger := logger.With(AttrRequestID, RequestIDFrom(c.Request.Context()))
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), loggerKey, requestLogger))

		c.Next()

		LoggerFrom(c.Request.Context()).Info("http-запрос обработан",
			"method", c.Request.Method,
			"path", c.FullPath(),
			"status", c.Writer.Status(),
			"duration_ms", time.Since(started).Milliseconds(),
			"client_ip", c.ClientIP(),
		)
	}
}

func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}

			LoggerFrom(c.Request.Context()).Error("паника при обработке запроса",
				"method", c.Request.Method,
				"path", c.FullPath(),
			)

			apierr.Write(c, apierr.New(apierr.CodeInternal, "Внутренняя ошибка сервера", nil))
		}()

		c.Next()
	}
}

func RequestIDFrom(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey).(string)

	return requestID
}

// Вне запроса возвращается logger по умолчанию, чтобы вызывающий код не
// проверял nil.
func LoggerFrom(ctx context.Context) *slog.Logger {
	logger, ok := ctx.Value(loggerKey).(*slog.Logger)
	if !ok {
		return slog.Default()
	}

	return logger
}
