package api

import (
	"context"
	"log/slog"

	"github.com/gin-gonic/gin"
)

// Технические проверки не входят в версионированный продуктовый API.
const (
	PathLive  = "/livez"
	PathReady = "/readyz"
)

type Probe interface {
	Ping(ctx context.Context) error
}

// Логгер Gin не подключается: записи о запросах идут через общий slog.
func NewRouter(logger *slog.Logger, database Probe) (*gin.Engine, error) {
	gin.SetMode(gin.ReleaseMode)

	engine := gin.New()
	if err := engine.SetTrustedProxies(nil); err != nil {
		return nil, err
	}

	engine.Use(RequestID(), RequestLog(logger), Recovery())

	RegisterHealth(engine, database)

	return engine, nil
}
