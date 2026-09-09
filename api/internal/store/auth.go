package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

var (
	ErrNotFound                = errors.New("запись не найдена")
	ErrEmailConflict           = errors.New("почта уже занята")
	ErrRegistrationKeyConflict = errors.New("ключ регистрации уже занят")
)

type Registration struct {
	Key          uuid.UUID
	Email        string
	PasswordHash string
	RequestID    string
	Now          time.Time
}

func (s *Store) UserExists(ctx context.Context, email string) (bool, error) {
	var count int64
	if err := s.db.WithContext(ctx).Model(&User{}).Where("email = ?", email).Count(&count).Error; err != nil {
		return false, fmt.Errorf("проверить пользователя: %w", err)
	}

	return count > 0, nil
}

func (s *Store) FindUserByEmail(ctx context.Context, email string) (User, error) {
	return findUser(s.db.WithContext(ctx).Where("email = ?", email))
}

func (s *Store) FindUserByID(ctx context.Context, id uuid.UUID) (User, error) {
	return findUser(s.db.WithContext(ctx).Where("id = ?", id))
}

func findUser(query *gorm.DB) (User, error) {
	var user User
	if err := query.First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return User{}, ErrNotFound
		}

		return User{}, fmt.Errorf("прочитать пользователя: %w", err)
	}

	return user, nil
}

func (s *Store) DeleteExpiredRegistrationKeys(ctx context.Context, tx *gorm.DB, cutoff time.Time) error {
	if err := tx.WithContext(ctx).Where("created_at <= ?", cutoff).Delete(&AuthRegistrationKey{}).Error; err != nil {
		return fmt.Errorf("удалить истёкшие ключи регистрации: %w", err)
	}

	return nil
}

func (s *Store) FindRegistrationKey(
	ctx context.Context,
	tx *gorm.DB,
	keyID uuid.UUID,
) (AuthRegistrationKey, bool, error) {
	var key AuthRegistrationKey
	err := tx.WithContext(ctx).Where("key = ?", keyID).First(&key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AuthRegistrationKey{}, false, nil
	}
	if err != nil {
		return AuthRegistrationKey{}, false, fmt.Errorf("прочитать ключ регистрации: %w", err)
	}

	return key, true, nil
}

func (s *Store) FindUserByIDInTx(ctx context.Context, tx *gorm.DB, id uuid.UUID) (User, error) {
	return findUser(tx.WithContext(ctx).Where("id = ?", id))
}

func (s *Store) CreateUser(ctx context.Context, tx *gorm.DB, user *User) error {
	if err := tx.WithContext(ctx).Create(user).Error; err != nil {
		return classifyRegistrationError(err)
	}

	return nil
}

func (s *Store) CreateUserQuota(ctx context.Context, tx *gorm.DB, quota *UserQuota) error {
	if err := tx.WithContext(ctx).Create(quota).Error; err != nil {
		return fmt.Errorf("создать квоту пользователя: %w", err)
	}

	return nil
}

func (s *Store) CreateRegistrationKey(ctx context.Context, tx *gorm.DB, key *AuthRegistrationKey) error {
	if err := tx.WithContext(ctx).Create(key).Error; err != nil {
		return classifyRegistrationError(err)
	}

	return nil
}

func (s *Store) FindRegistration(
	ctx context.Context,
	keyID uuid.UUID,
	cutoff time.Time,
) (User, bool, error) {
	var key AuthRegistrationKey
	err := s.db.WithContext(ctx).Where("key = ? AND created_at > ?", keyID, cutoff).First(&key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, fmt.Errorf("прочитать ключ регистрации: %w", err)
	}

	user, err := s.FindUserByID(ctx, key.UserID)
	if err != nil {
		return User{}, false, err
	}

	return user, true, nil
}

func classifyRegistrationError(err error) error {
	if postgresError, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch postgresError.ConstraintName {
		case "users_email_key":
			return ErrEmailConflict
		case "auth_registration_keys_pkey":
			return ErrRegistrationKeyConflict
		}
	}

	return fmt.Errorf("создать регистрацию: %w", err)
}

func (s *Store) GetQuota(ctx context.Context, userID uuid.UUID) (UserQuota, error) {
	var quota UserQuota
	if err := s.db.WithContext(ctx).Where("user_id = ?", userID).First(&quota).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return UserQuota{}, ErrNotFound
		}

		return UserQuota{}, fmt.Errorf("прочитать квоту пользователя: %w", err)
	}

	return quota, nil
}
