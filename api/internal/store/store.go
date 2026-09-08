// Package store не меняет схему базы: её создают миграции goose из корневого
// каталога migrations.
package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Одна реплика API обслуживает HTTP и фоновые циклы, поэтому пул держится
// небольшим и с ограниченным временем жизни.
const (
	DefaultMaxOpenConns    = 20
	DefaultMaxIdleConns    = 5
	DefaultConnMaxLifetime = 30 * time.Minute
	DefaultPingTimeout     = 3 * time.Second
)

type Store struct {
	db *gorm.DB
}

func Open(ctx context.Context, databaseURL string, logger *slog.Logger) (*Store, error) {
	db, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{
		Logger:      gormlogger.Discard,
		NowFunc:     func() time.Time { return time.Now().UTC() },
		QueryFields: true,
	})
	if err != nil {
		return nil, fmt.Errorf("подключиться к PostgreSQL: %w", err)
	}

	pool, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("получить пул соединений: %w", err)
	}

	pool.SetMaxOpenConns(DefaultMaxOpenConns)
	pool.SetMaxIdleConns(DefaultMaxIdleConns)
	pool.SetConnMaxLifetime(DefaultConnMaxLifetime)

	store := &Store{db: db}

	if err := store.Ping(ctx); err != nil {
		// Пул уже создан, поэтому его нужно закрыть до возврата ошибки.
		_ = store.Close()

		return nil, err
	}

	logger.Info("соединение с postgresql установлено",
		"max_open_conns", DefaultMaxOpenConns,
		"max_idle_conns", DefaultMaxIdleConns,
	)

	return store, nil
}

func (s *Store) DB() *gorm.DB {
	return s.db
}

func (s *Store) Ping(ctx context.Context) error {
	pool, err := s.db.DB()
	if err != nil {
		return fmt.Errorf("получить пул соединений: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, DefaultPingTimeout)
	defer cancel()

	if err := pool.PingContext(ctx); err != nil {
		return fmt.Errorf("проверить соединение с PostgreSQL: %w", err)
	}

	return nil
}

func (s *Store) Close() error {
	pool, err := s.db.DB()
	if err != nil {
		return fmt.Errorf("получить пул соединений: %w", err)
	}

	if err := pool.Close(); err != nil {
		return fmt.Errorf("закрыть пул соединений: %w", err)
	}

	return nil
}
