package store

import (
	"context"
	"database/sql"
	"fmt"

	"gorm.io/gorm"
)

func (s *Store) WithinTransaction(ctx context.Context, fn func(*gorm.DB) error) error {
	if err := s.db.WithContext(ctx).Transaction(fn, &sql.TxOptions{Isolation: sql.LevelReadCommitted}); err != nil {
		return fmt.Errorf("выполнить транзакцию: %w", err)
	}

	return nil
}
