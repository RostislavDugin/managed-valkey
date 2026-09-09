package audit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

const (
	DefaultPageLimit = 50
	MaxPageLimit     = 200
)

type LogEntry struct {
	ID        uuid.UUID   `json:"id"`
	Action    EventAction `json:"action"`
	UserEmail string      `json:"user_email"`
	CreatedAt time.Time   `json:"created_at"`
}

type Position struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

type PageRequest struct {
	Limit  int
	Before *Position
}

type Page struct {
	Items      []LogEntry `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

type ReadRepository interface {
	ListAudit(
		context.Context,
		domain.ManagedService,
		uuid.UUID,
		*Position,
		int,
	) ([]LogEntry, error)
}

type cursorPayload struct {
	CreatedAt time.Time `json:"created_at"`
	ID        uuid.UUID `json:"id"`
}

func ParsePageRequest(limitValue, beforeValue *string) (PageRequest, error) {
	limit := DefaultPageLimit
	if limitValue != nil {
		parsed, err := strconv.Atoi(*limitValue)
		if err != nil || parsed < 1 || parsed > MaxPageLimit {
			return PageRequest{}, invalidPageParameter("limit", "out_of_range")
		}

		limit = parsed
	}

	var before *Position
	if beforeValue != nil {
		parsed, err := decodeCursor(*beforeValue)
		if err != nil {
			return PageRequest{}, invalidPageParameter("before", "invalid_cursor")
		}

		before = &parsed
	}

	return PageRequest{Limit: limit, Before: before}, nil
}

func (s *Service) ListResource(
	ctx context.Context,
	service domain.ManagedService,
	resourceID uuid.UUID,
	request PageRequest,
) (Page, error) {
	if request.Limit < 1 || request.Limit > MaxPageLimit {
		return Page{}, invalidPageParameter("limit", "out_of_range")
	}

	items, err := s.reader.ListAudit(ctx, service, resourceID, request.Before, request.Limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("прочитать журнал аудита: %w", err)
	}
	if items == nil {
		items = []LogEntry{}
	}
	if len(items) <= request.Limit {
		return Page{Items: items}, nil
	}

	items = items[:request.Limit]
	cursor, err := encodeCursor(items[len(items)-1])
	if err != nil {
		return Page{}, fmt.Errorf("закодировать курсор аудита: %w", err)
	}

	return Page{Items: items, NextCursor: &cursor}, nil
}

func encodeCursor(entry LogEntry) (string, error) {
	payload, err := json.Marshal(cursorPayload{
		CreatedAt: entry.CreatedAt.UTC().Truncate(time.Microsecond),
		ID:        entry.ID,
	})
	if err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeCursor(value string) (Position, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) == 0 {
		return Position{}, fmt.Errorf("декодировать base64url")
	}

	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()

	var payload cursorPayload
	if err := decoder.Decode(&payload); err != nil {
		return Position{}, fmt.Errorf("разобрать JSON курсора: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Position{}, fmt.Errorf("проверить конец JSON курсора")
	}
	if payload.CreatedAt.IsZero() || payload.ID == uuid.Nil {
		return Position{}, fmt.Errorf("проверить поля курсора")
	}

	return Position{CreatedAt: payload.CreatedAt.UTC(), ID: payload.ID}, nil
}

func invalidPageParameter(field, reason string) error {
	return apierr.New(
		apierr.CodeValidationFailed,
		"Проверьте параметры страницы аудита",
		map[string]any{"fields": map[string]string{field: reason}},
	)
}
