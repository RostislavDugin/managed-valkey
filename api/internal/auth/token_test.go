package auth_test

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/auth"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func TestTokenIssueAndVerify(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	userID, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("создать UUIDv7: %v", err)
	}
	service := auth.NewTokenService("secret", fixedClock{now: now})

	raw, err := service.Issue(userID)
	if err != nil {
		t.Fatalf("выпустить JWT: %v", err)
	}
	verified, err := service.Verify(raw)
	if err != nil {
		t.Fatalf("проверить JWT: %v", err)
	}
	if verified != userID {
		t.Errorf("sub %s, ожидался %s", verified, userID)
	}

	claims := &jwt.RegisteredClaims{}
	_, err = jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) { return []byte("secret"), nil })
	if err != nil {
		t.Fatalf("прочитать JWT: %v", err)
	}
	wantExpiry := now.AddDate(auth.TokenLifetimeYears, 0, 0)
	if got := claims.ExpiresAt.Time; !got.Equal(wantExpiry) {
		t.Errorf("срок до %s, ожидался до %s", got, wantExpiry)
	}
}

func TestTokenRejectsInvalidValues(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	service := auth.NewTokenService("secret", fixedClock{now: now})
	userID, _ := uuid.NewV7()

	valid, _ := service.Issue(userID)
	expired := signedToken(t, jwt.SigningMethodHS256, "secret", userID.String(), now.Add(-time.Second))
	wrongSignature := signedToken(t, jwt.SigningMethodHS256, "other-secret", userID.String(), now.Add(time.Hour))
	wrongAlgorithm := signedToken(t, jwt.SigningMethodHS384, "secret", userID.String(), now.Add(time.Hour))
	missingSubject := signedToken(t, jwt.SigningMethodHS256, "secret", "", now.Add(time.Hour))
	missingExpiry := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{Subject: userID.String()})
	missingExpiryRaw, _ := missingExpiry.SignedString([]byte("secret"))

	for name, raw := range map[string]string{
		"истёкший": expired, "неверная подпись": wrongSignature, "другой алгоритм": wrongAlgorithm,
		"нет sub": missingSubject, "нет exp": missingExpiryRaw,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Verify(raw); err == nil {
				t.Fatal("повреждённый JWT принят")
			}
		})
	}

	if _, err := service.Verify(valid); err != nil {
		t.Fatalf("действующий JWT отклонён: %v", err)
	}
}

func signedToken(t *testing.T, method jwt.SigningMethod, secret, subject string, expiresAt time.Time) string {
	t.Helper()

	token := jwt.NewWithClaims(method, jwt.RegisteredClaims{Subject: subject, ExpiresAt: jwt.NewNumericDate(expiresAt)})
	raw, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("подписать JWT: %v", err)
	}

	return raw
}
