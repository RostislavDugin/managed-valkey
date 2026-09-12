package api

import (
	"context"
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/auth"
)

// Технические проверки не входят в версионированный продуктовый API.
const (
	PathLive         = "/livez"
	PathReady        = "/readyz"
	PathHealth       = "/health"
	PathValkeyHealth = "/valkey-health"
)

type Probe interface {
	Ping(ctx context.Context) error
}

// Логгер Gin не подключается: записи о запросах идут через общий slog.
func NewRouter(
	logger *slog.Logger,
	database Probe,
	authService AuthService,
	valkeyService ValkeyService,
	auditService AuditService,
	platformHealth PlatformHealthService,
	valkeyHealth ValkeyHealthService,
) (*gin.Engine, error) {
	gin.SetMode(gin.ReleaseMode)

	engine := gin.New()
	engine.HandleMethodNotAllowed = true
	if err := engine.SetTrustedProxies(nil); err != nil {
		return nil, err
	}

	engine.Use(RequestID(), RequestLog(logger), Recovery())
	engine.NoRoute(func(c *gin.Context) {
		apierr.Write(c, apierr.New(apierr.CodeNotFound, "Маршрут не найден", nil))
	})
	engine.NoMethod(func(c *gin.Context) {
		apierr.Write(c, apierr.New(apierr.CodeNotFound, "Метод не поддерживается", nil))
	})

	RegisterHealth(engine, database)
	RegisterOperationalHealth(engine, platformHealth, valkeyHealth)
	RegisterAuth(engine, authService, NewRateLimiter(auth.SystemClock{}, AuthRateLimit, AuthRateWindow))
	RegisterValkey(engine, authService, valkeyService, auditService)

	return engine, nil
}
