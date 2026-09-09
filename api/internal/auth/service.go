package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

type Repository interface {
	UserExists(context.Context, string) (bool, error)
	FindUserByEmail(context.Context, string) (store.User, error)
	FindUserByID(context.Context, uuid.UUID) (store.User, error)
	FindUserByIDInTx(context.Context, *gorm.DB, uuid.UUID) (store.User, error)
	DeleteExpiredRegistrationKeys(context.Context, *gorm.DB, time.Time) error
	FindRegistrationKey(context.Context, *gorm.DB, uuid.UUID) (store.AuthRegistrationKey, bool, error)
	CreateUser(context.Context, *gorm.DB, *store.User) error
	CreateUserQuota(context.Context, *gorm.DB, *store.UserQuota) error
	CreateRegistrationKey(context.Context, *gorm.DB, *store.AuthRegistrationKey) error
	FindRegistration(context.Context, uuid.UUID, time.Time) (store.User, bool, error)
	GetQuotaUsage(context.Context, uuid.UUID) (store.QuotaUsage, error)
}

type TxRunner interface {
	WithinTransaction(context.Context, func(*gorm.DB) error) error
}

type AuditWriter interface {
	Write(context.Context, *gorm.DB, audit.Event) error
}

type Service struct {
	repository Repository
	txRunner   TxRunner
	audit      AuditWriter
	tokens     *TokenService
	clock      Clock
	dummyHash  string
}

type Credentials struct {
	Email    string
	Password string
}

type Registration struct {
	Credentials
	Key       uuid.UUID
	RequestID string
}

type CurrentUser struct {
	ID        uuid.UUID
	Email     string
	MaxVCPU   int
	MaxRAMGB  int
	UsedVCPU  int
	UsedRAMGB int
}

func NewService(
	repository Repository,
	txRunner TxRunner,
	auditWriter AuditWriter,
	tokens *TokenService,
	clock Clock,
) (*Service, error) {
	dummyHash, err := HashPassword("unknown-user-password")
	if err != nil {
		return nil, err
	}

	return &Service{
		repository: repository,
		txRunner:   txRunner,
		audit:      auditWriter,
		tokens:     tokens,
		clock:      clock,
		dummyHash:  dummyHash,
	}, nil
}

func (s *Service) CheckEmail(ctx context.Context, input string) (bool, error) {
	email := NormalizeEmail(input)
	if err := ValidateEmail(email); err != nil {
		return false, err
	}

	exists, err := s.repository.UserExists(ctx, email)
	if err != nil {
		return false, apierr.WrapInternal(err)
	}

	return exists, nil
}

func (s *Service) Register(ctx context.Context, input Registration) (string, error) {
	email := NormalizeEmail(input.Email)
	if err := ValidateEmail(email); err != nil {
		return "", err
	}
	if err := ValidatePassword(input.Password); err != nil {
		return "", err
	}

	passwordHash, err := HashPassword(input.Password)
	if err != nil {
		return "", err
	}

	now := s.clock.Now().UTC()
	var user store.User
	var replay bool
	err = s.txRunner.WithinTransaction(ctx, func(tx *gorm.DB) error {
		if deleteErr := s.repository.DeleteExpiredRegistrationKeys(ctx, tx, now.Add(-24*time.Hour)); deleteErr != nil {
			return deleteErr
		}

		key, found, findErr := s.repository.FindRegistrationKey(ctx, tx, input.Key)
		if findErr != nil {
			return findErr
		}
		if found {
			registered, userErr := s.repository.FindUserByIDInTx(ctx, tx, key.UserID)
			if userErr != nil {
				return userErr
			}

			user = registered
			replay = true

			return nil
		}

		user = store.User{Email: email, PasswordHash: passwordHash, CreatedAt: now}
		if createErr := s.repository.CreateUser(ctx, tx, &user); createErr != nil {
			return createErr
		}

		quota := store.UserQuota{UserID: user.ID, MaxVCPU: 4, MaxRAMGB: 16}
		if quotaErr := s.repository.CreateUserQuota(ctx, tx, &quota); quotaErr != nil {
			return quotaErr
		}

		if auditErr := s.audit.Write(ctx, tx, audit.Event{
			UserID: user.ID, UserEmail: user.Email, Action: audit.ActionUserRegister,
			RequestID: input.RequestID, CreatedAt: now,
		}); auditErr != nil {
			return auditErr
		}

		key = store.AuthRegistrationKey{Key: input.Key, UserID: user.ID, CreatedAt: now}

		return s.repository.CreateRegistrationKey(ctx, tx, &key)
	})
	if err != nil {
		if errors.Is(err, store.ErrRegistrationKeyConflict) || errors.Is(err, store.ErrEmailConflict) {
			accepted, found, findErr := s.repository.FindRegistration(ctx, input.Key, now.Add(-24*time.Hour))
			if findErr != nil {
				return "", apierr.WrapInternal(findErr)
			}
			if found {
				user = accepted
				replay = true
				err = nil
			}
		}
	}
	if err != nil {
		if errors.Is(err, store.ErrEmailConflict) {
			existing, findErr := s.repository.FindUserByEmail(ctx, email)
			if findErr == nil && existing.IsBlocked {
				return "", unauthorized()
			}
			if findErr != nil && !errors.Is(findErr, store.ErrNotFound) {
				return "", apierr.WrapInternal(findErr)
			}

			return "", apierr.New(apierr.CodeConflict, "Аккаунт с такой почтой уже существует", nil)
		}

		return "", apierr.WrapInternal(err)
	}
	if replay && (user.Email != email || !PasswordMatches(user.PasswordHash, input.Password)) {
		return "", apierr.New(apierr.CodeIdempotencyMismatch, "Ключ регистрации использован с другими данными", nil)
	}
	if user.IsBlocked {
		return "", unauthorized()
	}

	return s.tokens.Issue(user.ID)
}

func (s *Service) Login(ctx context.Context, input Credentials, requestID string) (string, error) {
	email := NormalizeEmail(input.Email)
	if err := ValidateEmail(email); err != nil {
		return "", err
	}
	if err := ValidatePassword(input.Password); err != nil {
		return "", err
	}

	user, err := s.repository.FindUserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		PasswordMatches(s.dummyHash, input.Password)

		return "", unauthorized()
	}
	if err != nil {
		return "", apierr.WrapInternal(err)
	}
	if !PasswordMatches(user.PasswordHash, input.Password) || user.IsBlocked {
		return "", unauthorized()
	}

	now := s.clock.Now().UTC()
	if err := s.txRunner.WithinTransaction(ctx, func(tx *gorm.DB) error {
		return s.audit.Write(ctx, tx, audit.Event{
			UserID: user.ID, UserEmail: user.Email, Action: audit.ActionUserLogin,
			RequestID: requestID, CreatedAt: now,
		})
	}); err != nil {
		return "", apierr.WrapInternal(err)
	}

	return s.tokens.Issue(user.ID)
}

func (s *Service) Authenticate(ctx context.Context, rawToken string) (store.User, error) {
	userID, err := s.tokens.Verify(rawToken)
	if err != nil {
		return store.User{}, err
	}

	user, err := s.repository.FindUserByID(ctx, userID)
	if err != nil || user.IsBlocked {
		return store.User{}, unauthorized()
	}

	return user, nil
}

func (s *Service) Me(ctx context.Context, userID uuid.UUID) (CurrentUser, error) {
	user, err := s.repository.FindUserByID(ctx, userID)
	if err != nil || user.IsBlocked {
		return CurrentUser{}, unauthorized()
	}

	quota, err := s.repository.GetQuotaUsage(ctx, userID)
	if err != nil {
		return CurrentUser{}, apierr.WrapInternal(err)
	}

	return CurrentUser{
		ID: user.ID, Email: user.Email, MaxVCPU: quota.MaxVCPU, MaxRAMGB: quota.MaxRAMGB,
		UsedVCPU: quota.UsedVCPU, UsedRAMGB: quota.UsedRAMGB,
	}, nil
}
