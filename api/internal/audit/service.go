package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/RostislavDugin/managed-valkey/api/internal/domain"
)

type EventAction string

const (
	ActionUserRegister            EventAction = "user.register"
	ActionUserLogin               EventAction = "user.login"
	ActionInstanceCreate          EventAction = "instance.create"
	ActionInstanceUpdate          EventAction = "instance.update"
	ActionInstanceResize          EventAction = "instance.resize"
	ActionInstanceWhitelistUpdate EventAction = "instance.whitelist.update"
	ActionInstancePasswordRotate  EventAction = "instance.password.rotate"
	ActionInstanceDelete          EventAction = "instance.delete"
)

type Event struct {
	UserID     uuid.UUID
	UserEmail  string
	Action     EventAction
	Service    *domain.ManagedService
	ResourceID *uuid.UUID
	RequestID  string
	CreatedAt  time.Time
}

type WriteRepository interface {
	WriteAudit(context.Context, *gorm.DB, Event) error
}

type Service struct {
	writer WriteRepository
	reader ReadRepository
}

func NewService(writer WriteRepository, reader ReadRepository) *Service {
	return &Service{writer: writer, reader: reader}
}

func (s *Service) Write(ctx context.Context, tx *gorm.DB, event Event) error {
	if err := s.writer.WriteAudit(ctx, tx, event); err != nil {
		return fmt.Errorf("записать событие аудита: %w", err)
	}

	return nil
}
