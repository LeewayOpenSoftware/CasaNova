package model

import "time"

// Common models used across the application — pagination, version info,
// heartbeat, etc. Consolidated from common.go, heart.go, req.go, version.go.

// PageResp is a standard paginated response envelope.
type PageResp struct {
	Content interface{} `json:"content"`
	Total   int64       `json:"total"`
}

// PageReq is a standard pagination request.
type PageReq struct {
	Index int `json:"page" form:"index"`
	Size  int `json:"size" form:"size"`
}

// Integer bound constants used for page size limits.
const MaxUint = ^uint(0)
const MinUint = 0
const MaxInt = int(MaxUint >> 1)
const MinInt = -MaxInt - 1

// Validate normalizes pagination parameters, clamping out-of-range values.
func (p *PageReq) Validate() {
	if p.Index < 1 {
		p.Index = 1
	}
	if p.Size < 1 {
		p.Size = 100000
	}
}

// CasaOSHeart is the heartbeat message used between peer CasaNova instances.
// (Name kept for wire-format compatibility during migration.)
type CasaOSHeart struct {
	UuId string `json:"uuid"`
	Type string `json:"type"`
}

// Version represents a released CasaNova version as stored in the DB.
type Version struct {
	Id        uint      `gorm:"column:id;primary_key" json:"id"`
	ChangeLog string    `json:"change_log"`
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
