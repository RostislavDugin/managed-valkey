package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

var errAuditRejected = errors.New("управляемый отказ аудита")

type failingAuditRepository struct {
	delegate audit.WriteRepository
	action   audit.EventAction
	after    bool
}

func (r failingAuditRepository) WriteAudit(ctx context.Context, tx *gorm.DB, event audit.Event) error {
	if event.Action != r.action {
		return r.delegate.WriteAudit(ctx, tx, event)
	}
	if r.after {
		if err := r.delegate.WriteAudit(ctx, tx, event); err != nil {
			return err
		}
	}

	return errAuditRejected
}

func TestRegistrationAuditFailureRollsBackOverHTTP(t *testing.T) {
	email := "audit-failure-" + uuid.NewString() + "@example.com"
	app := newHTTPTestAPI(t, testAPIConfig{
		wrapAuditRepository: failAudit(audit.ActionUserRegister, false),
	})
	app.cleanupUser(t, email)

	response := app.requestJSON(t, http.MethodPost, "/v1/auth/register", map[string]string{
		"email": email, "password": "password1",
	}, map[string]string{"Idempotency-Key": uuid.NewString()})
	assertError(t, response, http.StatusInternalServerError, string(apierr.CodeInternal))

	assertDatabaseCount(t, app.database.DB().Model(&store.User{}).Where("email = ?", email), 0)
	assertDatabaseCount(
		t,
		app.database.DB().
			Model(&store.AuditLog{}).
			Where("user_email = ? AND action = ?", email, audit.ActionUserRegister),
		0,
	)
}

func TestLoginAuditFailureDoesNotIssueJWTOverHTTP(t *testing.T) {
	setup := newHTTPTestAPI(t, testAPIConfig{})
	account := setup.registerAccount(t, "")
	app := newHTTPTestAPI(t, testAPIConfig{
		wrapAuditRepository: failAudit(audit.ActionUserLogin, false),
	})

	response := app.requestJSON(t, http.MethodPost, "/v1/auth/login", map[string]string{
		"email": account.Email, "password": account.Password,
	}, nil)
	assertError(t, response, http.StatusInternalServerError, string(apierr.CodeInternal))
	if string(response.Body) == "" || containsAny(string(response.Body), "token", account.Password) {
		t.Fatalf("ошибка входа раскрывает токен или пароль: %s", response.Body)
	}

	assertDatabaseCount(
		t,
		app.database.DB().
			Model(&store.AuditLog{}).
			Where("user_id = ? AND action = ?", account.ID, audit.ActionUserLogin),
		0,
	)
}

func failAudit(action audit.EventAction, after bool) func(audit.WriteRepository) audit.WriteRepository {
	return func(delegate audit.WriteRepository) audit.WriteRepository {
		return failingAuditRepository{delegate: delegate, action: action, after: after}
	}
}
