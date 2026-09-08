package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
)

const TokenLifetimeYears = 10

type Clock interface {
	Now() time.Time
}

type SystemClock struct{}

func (SystemClock) Now() time.Time {
	return time.Now().UTC()
}

type TokenService struct {
	secret []byte
	clock  Clock
}

func NewTokenService(secret string, clock Clock) *TokenService {
	return &TokenService{secret: []byte(secret), clock: clock}
}

func (s *TokenService) Issue(userID uuid.UUID) (string, error) {
	if userID.Version() != 7 {
		return "", apierr.WrapInternal(errors.New("идентификатор пользователя не UUIDv7"))
	}

	now := s.clock.Now().UTC()
	claims := jwt.RegisteredClaims{
		Subject:   userID.String(),
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.AddDate(TokenLifetimeYears, 0, 0)),
	}

	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return "", apierr.WrapInternal(err)
	}

	return token, nil
}

func (s *TokenService) Verify(raw string) (uuid.UUID, error) {
	claims := &jwt.RegisteredClaims{}
	parsed, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("недопустимый алгоритм JWT")
		}

		return s.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired(), jwt.WithTimeFunc(s.clock.Now))
	if err != nil || !parsed.Valid || claims.Subject == "" || claims.ExpiresAt == nil {
		return uuid.Nil, unauthorized()
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil || userID.Version() != 7 {
		return uuid.Nil, unauthorized()
	}

	return userID, nil
}

func unauthorized() *apierr.Error {
	return apierr.New(apierr.CodeUnauthorized, "Неверные учётные данные или сессия", nil)
}
