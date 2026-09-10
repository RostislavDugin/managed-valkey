package v1alpha1_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

func Test_ParseAppPasswordHash_WithValidAndInvalidSecretData_ReturnsHashOrValidationError(t *testing.T) {
	validHash := strings.Repeat("ab", 32)

	tests := []struct {
		name    string
		data    map[string][]byte
		version int64
		want    string
		wantErr error
	}{
		{
			name:    "при корректном хеше возвращает его без ошибки",
			data:    map[string][]byte{"app-password-hash.2": []byte(validHash)},
			version: 2,
			want:    validHash,
		},
		{
			name:    "при отсутствии хеша возвращает ошибку отсутствующего значения",
			data:    map[string][]byte{},
			version: 2,
			wantErr: valkeyv1alpha1.ErrAppPasswordHashMissing,
		},
		{
			name:    "при хеше в верхнем регистре возвращает ошибку неверного формата",
			data:    map[string][]byte{"app-password-hash.2": []byte(strings.ToUpper(validHash))},
			version: 2,
			wantErr: valkeyv1alpha1.ErrAppPasswordHashInvalid,
		},
		{
			name:    "при слишком коротком хеше возвращает ошибку неверного формата",
			data:    map[string][]byte{"app-password-hash.2": []byte("abcd")},
			version: 2,
			wantErr: valkeyv1alpha1.ErrAppPasswordHashInvalid,
		},
		{
			name:    "при нулевой версии возвращает ошибку неверной версии",
			data:    map[string][]byte{},
			version: 0,
			wantErr: valkeyv1alpha1.ErrInvalidPasswordVersion,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := valkeyv1alpha1.ParseAppPasswordHash(test.data, test.version)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("ошибка %v, ожидалась %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("хеш %q, ожидался %q", got, test.want)
			}
		})
	}
}

func Test_ParseServiceCredentials_WithCompleteEmptyPartialAndInvalidData_ReturnsIndependentCredentialsOrError(
	t *testing.T,
) {
	password := []byte(base64.RawURLEncoding.EncodeToString(make([]byte, 24)))
	complete := map[string][]byte{
		valkeyv1alpha1.OperatorPasswordKey: password,
		valkeyv1alpha1.ReplicaPasswordKey:  password,
		valkeyv1alpha1.HealthPasswordKey:   password,
		valkeyv1alpha1.UsersACLKey:         []byte("user default off\n"),
	}

	credentials, err := valkeyv1alpha1.ParseServiceCredentials(complete)
	if err != nil {
		t.Fatalf("разобрать полный комплект: %v", err)
	}
	if credentials == nil || string(credentials.UsersACL) != "user default off\n" {
		t.Fatalf("неверный комплект: %+v", credentials)
	}
	credentials.OperatorPassword[0] = 'x'
	if complete[valkeyv1alpha1.OperatorPasswordKey][0] == 'x' {
		t.Fatal("результат разделяет память с Secret")
	}

	empty, err := valkeyv1alpha1.ParseServiceCredentials(map[string][]byte{})
	if err != nil || empty != nil {
		t.Fatalf("пустой комплект: credentials=%+v error=%v", empty, err)
	}

	partial := map[string][]byte{valkeyv1alpha1.OperatorPasswordKey: password}
	if _, err := valkeyv1alpha1.ParseServiceCredentials(partial); !errors.Is(
		err,
		valkeyv1alpha1.ErrServiceCredentialsPartial,
	) {
		t.Fatalf("частичный комплект: %v", err)
	}

	broken := map[string][]byte{
		valkeyv1alpha1.OperatorPasswordKey: []byte("broken"),
		valkeyv1alpha1.ReplicaPasswordKey:  password,
		valkeyv1alpha1.HealthPasswordKey:   password,
		valkeyv1alpha1.UsersACLKey:         []byte("user default off\n"),
	}
	if _, err := valkeyv1alpha1.ParseServiceCredentials(broken); !errors.Is(
		err,
		valkeyv1alpha1.ErrServiceCredentialsInvalid,
	) {
		t.Fatalf("повреждённый комплект: %v", err)
	}
}
