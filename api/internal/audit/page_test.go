package audit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

type pageReader struct {
	items  []LogEntry
	before *Position
	limit  int
}

func (r *pageReader) ListAudit(
	_ context.Context,
	_ domain.ManagedService,
	_ uuid.UUID,
	before *Position,
	limit int,
) ([]LogEntry, error) {
	r.before = before
	r.limit = limit

	return r.items, nil
}

func TestParsePageRequest(t *testing.T) {
	minimum := "1"
	maximum := "200"

	for _, testCase := range []struct {
		name  string
		value *string
		want  int
	}{
		{name: "по умолчанию", want: DefaultPageLimit},
		{name: "минимум", value: &minimum, want: 1},
		{name: "максимум", value: &maximum, want: MaxPageLimit},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request, err := ParsePageRequest(testCase.value, nil)
			if err != nil {
				t.Fatalf("разобрать страницу: %v", err)
			}
			if request.Limit != testCase.want || request.Before != nil {
				t.Fatalf("неожиданный запрос: %+v", request)
			}
		})
	}

	for _, value := range []string{"", "0", "201", "1.5", "abc"} {
		t.Run("неверный лимит "+value, func(t *testing.T) {
			_, err := ParsePageRequest(&value, nil)
			assertValidationError(t, err, "limit")
		})
	}
}

func TestPageCursorPreservesPosition(t *testing.T) {
	createdAt := time.Date(2026, time.September, 9, 10, 11, 12, 345678000, time.UTC)
	firstID := uuid.MustParse("01993000-0000-7000-8000-000000000002")
	reader := &pageReader{items: []LogEntry{
		{ID: firstID, CreatedAt: createdAt},
		{ID: uuid.MustParse("01993000-0000-7000-8000-000000000001"), CreatedAt: createdAt},
	}}
	service := NewService(nil, reader)

	page, err := service.ListResource(
		context.Background(),
		domain.ManagedServiceValkey,
		uuid.New(),
		PageRequest{Limit: 1},
	)
	if err != nil {
		t.Fatalf("прочитать страницу: %v", err)
	}
	if len(page.Items) != 1 || page.NextCursor == nil || strings.Contains(*page.NextCursor, "=") {
		t.Fatalf("неожиданная страница: %+v", page)
	}
	if reader.limit != 2 {
		t.Fatalf("репозиторий получил лимит %d, ожидался 2", reader.limit)
	}

	request, err := ParsePageRequest(nil, page.NextCursor)
	if err != nil {
		t.Fatalf("разобрать курсор: %v", err)
	}
	if request.Before == nil || request.Before.ID != firstID || !request.Before.CreatedAt.Equal(createdAt) {
		t.Fatalf("курсор изменил позицию: %+v", request.Before)
	}
}

func TestParsePageRequestRejectsInvalidCursor(t *testing.T) {
	for _, value := range []string{
		"",
		"broken",
		"e30",
		"eyJjcmVhdGVkX2F0IjpudWxsLCJpZCI6bnVsbH0",
		"eyJjcmVhdGVkX2F0IjoiMjAyNi0wOS0wOVQxMDoxMToxMi4zNDU2NzhaIiwiaWQiOiIwMTk5MzAwMC0wMDAwLTcwMDAtODAwMC0wMDAwMDAwMDAwMDIiLCJleHRyYSI6dHJ1ZX0",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := ParsePageRequest(nil, &value)
			assertValidationError(t, err, "before")
		})
	}
}

func assertValidationError(t *testing.T, err error, field string) {
	t.Helper()

	known, ok := err.(*apierr.Error)
	if !ok || known.Code != apierr.CodeValidationFailed {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	fields, ok := known.Details["fields"].(map[string]string)
	if !ok || fields[field] == "" {
		t.Fatalf("в ошибке нет поля %s: %+v", field, known.Details)
	}
}
