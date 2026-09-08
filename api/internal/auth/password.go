package auth

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
)

const (
	BcryptCost       = 12
	MinPasswordRunes = 8
	MaxPasswordBytes = 72
)

var emailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func ValidateEmail(email string) error {
	if !emailPattern.MatchString(email) {
		return apierr.New(
			apierr.CodeValidationFailed,
			"Проверьте адрес электронной почты",
			map[string]any{"field": "email"},
		)
	}

	return nil
}

func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordRunes || len([]byte(password)) > MaxPasswordBytes {
		return apierr.New(
			apierr.CodeValidationFailed,
			"Пароль должен содержать не меньше 8 символов и занимать не больше 72 байт",
			map[string]any{"field": "password"},
		)
	}

	return nil
}

func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return "", apierr.WrapInternal(err)
	}

	return string(hash), nil
}

func PasswordMatches(hash, password string) bool {
	return errors.Is(bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)), nil)
}
