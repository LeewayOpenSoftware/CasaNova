package model

import (
	"time"
)

type RelyDBModel struct {
	Id                uint      `gorm:"column:id;primary_key" json:"id"`
	CustomId          string    ` json:"custom_id"`
	ContainerCustomId string    `json:"container_custom_id"`
	ContainerId       string    `json:"container_id,omitempty"`
	Type              int       `json:"type"` // currently unused
	CreatedAt         time.Time `gorm:"<-:create" json:"created_at"`
	UpdatedAt         time.Time `gorm:"<-:create;<-:update" json:"updated_at"`
}

/****************gorm[]string*******************/

func (p RelyDBModel) TableName() string {
	return "o_rely"
}
