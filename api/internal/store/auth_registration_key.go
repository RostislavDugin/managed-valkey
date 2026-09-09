package store

import (
	"time"

	"github.com/google/uuid"
)

type AuthRegistrationKey struct {
	Key       uuid.UUID `gorm:"column:key;type:uuid;primaryKey"`
	UserID    uuid.UUID `gorm:"column:user_id;type:uuid"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (AuthRegistrationKey) TableName() string { return "auth_registration_keys" }
