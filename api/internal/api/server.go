package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Ограничивают медленных клиентов и от конфигурации окружения не зависят.
const (
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultReadTimeout       = 30 * time.Second
	DefaultWriteTimeout      = 60 * time.Second
	DefaultIdleTimeout       = 120 * time.Second
)

type Server struct {
	httpServer      *http.Server
	logger          *slog.Logger
	shutdownTimeout time.Duration
}

func NewServer(addr string, handler http.Handler, logger *slog.Logger, shutdownTimeout time.Duration) *Server {
	return &Server{
		httpServer: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: DefaultReadHeaderTimeout,
			ReadTimeout:       DefaultReadTimeout,
			WriteTimeout:      DefaultWriteTimeout,
			IdleTimeout:       DefaultIdleTimeout,
		},
		logger:          logger,
		shutdownTimeout: shutdownTimeout,
	}
}

func (s *Server) Run(ctx context.Context) error {
	var listenConfig net.ListenConfig

	listener, err := listenConfig.Listen(ctx, "tcp", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("занять адрес %s: %w", s.httpServer.Addr, err)
	}

	return s.Serve(ctx, listener)
}

// Отдельный метод с готовым listener нужен тестам и запуску на уже выбранном
// порту.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	served := make(chan error, 1)

	go func() {
		s.logger.Info("http-сервер запущен", "addr", listener.Addr().String())

		served <- s.httpServer.Serve(listener)
	}()

	select {
	case err := <-served:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("обслуживание http: %w", err)
		}

		return nil
	case <-ctx.Done():
	}

	s.logger.Info("остановка http-сервера", "timeout", s.shutdownTimeout.String())

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
	defer cancel()

	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("остановить http-сервер: %w", err)
	}

	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("обслуживание http: %w", err)
	}

	return nil
}
