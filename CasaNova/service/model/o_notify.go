package model

type AppNotify struct {
	State     int    `json:"state"` // 0:constantly-changing unread 1:unread 2:read
	Message   string `json:"message"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Id        string `json:"id"`
	Type      int    `json:"type"`
	Icon      string `json:"icon"`
	Name      string `json:"name"`
	Class     int    `json:"class"`
	CustomId  string `gorm:"column:custom_id;primary_key" json:"custom_id"`
}

func (p *AppNotify) TableName() string {
	return "o_notify"
}
