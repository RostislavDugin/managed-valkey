package auth_test

import (
	"errors"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/auth"
)

func TestNormalizeEmail(t *testing.T) {
	if got := auth.NormalizeEmail("  User@Example.COM \t"); got != "user@example.com" {
		t.Errorf("почта %q, ожидалась нормализованная", got)
	}
}

func TestValidateCredentials(t *testing.T) {
	cases := []struct {
		name     string
		password string
		valid    bool
	}{
		{name: "восемь символов", password: "12345678", valid: true},
		{name: "семь символов", password: "1234567", valid: false},
		{name: "ровно 72 байта", password: string(make([]byte, 72)), valid: true},
		{
			name:     "больше 72 байт UTF-8",
			password: "ёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжёжё",
			valid:    false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := auth.ValidatePassword(testCase.password)
			if testCase.valid && err != nil {
				t.Fatalf("допустимый пароль отклонён: %v", err)
			}
			if !testCase.valid {
				var validation *apierr.Error
				if !errors.As(err, &validation) || validation.Code != apierr.CodeValidationFailed {
					t.Fatalf("ошибка %v, ожидалась VALIDATION_FAILED", err)
				}
			}
		})
	}
}

func TestPasswordHashUsesCost12(t *testing.T) {
	hash, err := auth.HashPassword("password1")
	if err != nil {
		t.Fatalf("создать хеш: %v", err)
	}

	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		t.Fatalf("прочитать cost: %v", err)
	}
	if cost != auth.BcryptCost {
		t.Errorf("cost %d, ожидался %d", cost, auth.BcryptCost)
	}
	if !auth.PasswordMatches(hash, "password1") || auth.PasswordMatches(hash, "неверный-пароль") {
		t.Error("сравнение пароля вернуло неверный результат")
	}
}
