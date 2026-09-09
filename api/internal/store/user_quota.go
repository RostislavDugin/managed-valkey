package store

import "github.com/google/uuid"

type UserQuota struct {
	UserID   uuid.UUID `gorm:"column:user_id;type:uuid;primaryKey"`
	MaxVCPU  int       `gorm:"column:max_vcpu"`
	MaxRAMGB int       `gorm:"column:max_ram_gb"`
}

func (UserQuota) TableName() string { return "user_quotas" }
