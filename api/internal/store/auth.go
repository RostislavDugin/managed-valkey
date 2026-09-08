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

type User struct {
	ID           uuid.UUID `gorm:"type:uuid;default:uuidv7();primaryKey"`
	Email        string
	PasswordHash string
	IsBlocked    bool
	CreatedAt    time.Time
}

func (User) TableName() string { return "users" }

type UserQuota struct {
	UserID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	MaxVCPU  int       `gorm:"column:max_vcpu"`
	MaxRAMGB int       `gorm:"column:max_ram_gb"`
}

func (UserQuota) TableName() string { return "user_quotas" }

type AuditLog struct {
	ID         uuid.UUID `gorm:"type:uuid;default:uuidv7();primaryKey"`
	UserID     uuid.UUID `gorm:"type:uuid"`
	UserEmail  string
	Action     string
	Service    *string
	ResourceID *uuid.UUID `gorm:"type:uuid"`
	RequestID  string
	CreatedAt  time.Time
}

func (AuditLog) TableName() string { return "audit_logs" }

type AuthRegistrationKey struct {
	Key       uuid.UUID `gorm:"type:uuid;primaryKey"`
	UserID    uuid.UUID `gorm:"type:uuid"`
	CreatedAt time.Time
}

func (AuthRegistrationKey) TableName() string { return "auth_registration_keys" }

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

func (s *Store) Register(ctx context.Context, input Registration) (User, bool, error) {
	var registered User
	var replay bool

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("created_at <= ?", input.Now.Add(-24*time.Hour)).
			Delete(&AuthRegistrationKey{}).
			Error; err != nil {
			return fmt.Errorf("удалить истёкшие ключи регистрации: %w", err)
		}

		var key AuthRegistrationKey
		err := tx.Where("key = ?", input.Key).First(&key).Error
		if err == nil {
			user, findErr := findUser(tx.Where("id = ?", key.UserID))
			if findErr != nil {
				return findErr
			}

			registered = user
			replay = true

			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("прочитать ключ регистрации: %w", err)
		}

		registered = User{Email: input.Email, PasswordHash: input.PasswordHash, CreatedAt: input.Now}
		if err := tx.Create(&registered).Error; err != nil {
			return classifyRegistrationError(err)
		}

		quota := UserQuota{UserID: registered.ID, MaxVCPU: 4, MaxRAMGB: 16}
		if err := tx.Create(&quota).Error; err != nil {
			return fmt.Errorf("создать квоту пользователя: %w", err)
		}

		audit := AuditLog{
			UserID: registered.ID, UserEmail: registered.Email, Action: "user.register",
			RequestID: input.RequestID, CreatedAt: input.Now,
		}
		if err := tx.Create(&audit).Error; err != nil {
			return fmt.Errorf("записать регистрацию в аудит: %w", err)
		}

		key = AuthRegistrationKey{Key: input.Key, UserID: registered.ID, CreatedAt: input.Now}
		if err := tx.Create(&key).Error; err != nil {
			return classifyRegistrationError(err)
		}

		return nil
	})
	if err == nil {
		return registered, replay, nil
	}

	if errors.Is(err, ErrRegistrationKeyConflict) || errors.Is(err, ErrEmailConflict) {
		user, found, findErr := s.findRegistration(ctx, input.Key, input.Now)
		if findErr != nil {
			return User{}, false, findErr
		}
		if found {
			return user, true, nil
		}
	}

	return User{}, false, err
}

func (s *Store) findRegistration(ctx context.Context, keyID uuid.UUID, now time.Time) (User, bool, error) {
	var key AuthRegistrationKey
	err := s.db.WithContext(ctx).Where("key = ? AND created_at > ?", keyID, now.Add(-24*time.Hour)).First(&key).Error
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

func (s *Store) RecordLogin(ctx context.Context, user User, requestID string, now time.Time) error {
	audit := AuditLog{
		UserID: user.ID, UserEmail: user.Email, Action: "user.login", RequestID: requestID, CreatedAt: now,
	}

	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return tx.Create(&audit).Error
	}); err != nil {
		return fmt.Errorf("записать вход в аудит: %w", err)
	}

	return nil
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
