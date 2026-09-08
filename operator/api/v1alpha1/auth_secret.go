package v1alpha1

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	AuthSecretSuffix           = "-auth"
	AppPasswordHashKeyPrefix   = "app-password-hash."
	OperatorPasswordKey        = "operator-password"
	ReplicaPasswordKey         = "replica-password"
	HealthPasswordKey          = "health-password"
	UsersACLKey                = "users.acl"
	ServicePasswordRandomBytes = 24
)

var (
	ErrInvalidPasswordVersion    = errors.New("версия пароля должна быть положительной")
	ErrAppPasswordHashMissing    = errors.New("хеш пароля app отсутствует")
	ErrAppPasswordHashInvalid    = errors.New("хеш пароля app повреждён")
	ErrServiceCredentialsPartial = errors.New("комплект служебных учётных данных неполон")
	ErrServiceCredentialsInvalid = errors.New("комплект служебных учётных данных повреждён")
)

type ServiceCredentials struct {
	OperatorPassword []byte
	ReplicaPassword  []byte
	HealthPassword   []byte
	UsersACL         []byte
}

func AuthSecretName(slug string) string {
	return slug + AuthSecretSuffix
}

func AppPasswordHashKey(version int64) (string, error) {
	if version < 1 {
		return "", ErrInvalidPasswordVersion
	}

	return AppPasswordHashKeyPrefix + strconv.FormatInt(version, 10), nil
}

func ParseAppPasswordHash(data map[string][]byte, version int64) (string, error) {
	key, err := AppPasswordHashKey(version)
	if err != nil {
		return "", err
	}

	value, exists := data[key]
	if !exists {
		return "", ErrAppPasswordHashMissing
	}
	if len(value) != 64 {
		return "", ErrAppPasswordHashInvalid
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", rune(character)) {
			return "", ErrAppPasswordHashInvalid
		}
	}

	return string(value), nil
}

func ParseServiceCredentials(data map[string][]byte) (*ServiceCredentials, error) {
	keys := [...]string{OperatorPasswordKey, ReplicaPasswordKey, HealthPasswordKey, UsersACLKey}
	present := 0
	for _, key := range keys {
		if _, exists := data[key]; exists {
			present++
		}
	}

	if present == 0 {
		return nil, nil
	}
	if present != len(keys) {
		return nil, ErrServiceCredentialsPartial
	}

	credentials := &ServiceCredentials{
		OperatorPassword: cloneBytes(data[OperatorPasswordKey]),
		ReplicaPassword:  cloneBytes(data[ReplicaPasswordKey]),
		HealthPassword:   cloneBytes(data[HealthPasswordKey]),
		UsersACL:         cloneBytes(data[UsersACLKey]),
	}
	if !validServicePassword(credentials.OperatorPassword) ||
		!validServicePassword(credentials.ReplicaPassword) ||
		!validServicePassword(credentials.HealthPassword) ||
		len(credentials.UsersACL) == 0 {
		return nil, ErrServiceCredentialsInvalid
	}

	return credentials, nil
}

func validServicePassword(value []byte) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(string(value))
	return err == nil && len(decoded) == ServicePasswordRandomBytes
}

func cloneBytes(value []byte) []byte {
	return append([]byte(nil), value...)
}

func ServiceCredentialsData(credentials ServiceCredentials) (map[string][]byte, error) {
	if !validServicePassword(credentials.OperatorPassword) ||
		!validServicePassword(credentials.ReplicaPassword) ||
		!validServicePassword(credentials.HealthPassword) ||
		len(credentials.UsersACL) == 0 {
		return nil, fmt.Errorf("сформировать данные Secret: %w", ErrServiceCredentialsInvalid)
	}

	return map[string][]byte{
		OperatorPasswordKey: cloneBytes(credentials.OperatorPassword),
		ReplicaPasswordKey:  cloneBytes(credentials.ReplicaPassword),
		HealthPasswordKey:   cloneBytes(credentials.HealthPassword),
		UsersACLKey:         cloneBytes(credentials.UsersACL),
	}, nil
}
