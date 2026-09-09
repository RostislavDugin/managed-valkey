package store

import (
	"time"

	"github.com/google/uuid"
)

type IdempotencyKey struct {
	UserID         uuid.UUID `gorm:"column:user_id;type:uuid;primaryKey"`
	Key            uuid.UUID `gorm:"column:key;type:uuid;primaryKey"`
	RequestHash    string    `gorm:"column:request_hash"`
	ResponseStatus int       `gorm:"column:response_status"`
	ResponseBody   []byte    `gorm:"column:response_body;type:jsonb"`
	CreatedAt      time.Time `gorm:"column:created_at"`
}

func (IdempotencyKey) TableName() string { return "idempotency_keys" }
