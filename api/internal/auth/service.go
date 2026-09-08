package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

type Repository interface {
	UserExists(context.Context, string) (bool, error)
	FindUserByEmail(context.Context, string) (store.User, error)
	FindUserByID(context.Context, uuid.UUID) (store.User, error)
	Register(context.Context, store.Registration) (store.User, bool, error)
	RecordLogin(context.Context, store.User, string, time.Time) error
	GetQuota(context.Context, uuid.UUID) (store.UserQuota, error)
}

type Service struct {
	repository Repository
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

func NewService(repository Repository, tokens *TokenService, clock Clock) (*Service, error) {
	dummyHash, err := HashPassword("unknown-user-password")
	if err != nil {
		return nil, err
	}

	return &Service{repository: repository, tokens: tokens, clock: clock, dummyHash: dummyHash}, nil
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

	user, replay, err := s.repository.Register(ctx, store.Registration{
		Key: input.Key, Email: email, PasswordHash: passwordHash,
		RequestID: input.RequestID, Now: s.clock.Now().UTC(),
	})
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

	if err := s.repository.RecordLogin(ctx, user, requestID, s.clock.Now().UTC()); err != nil {
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

	quota, err := s.repository.GetQuota(ctx, userID)
	if err != nil {
		return CurrentUser{}, apierr.WrapInternal(err)
	}

	return CurrentUser{
		ID: user.ID, Email: user.Email, MaxVCPU: quota.MaxVCPU, MaxRAMGB: quota.MaxRAMGB,
		UsedVCPU: 0, UsedRAMGB: 0,
	}, nil
}
