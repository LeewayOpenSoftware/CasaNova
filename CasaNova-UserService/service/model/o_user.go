/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-05-13 18:15:46
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-07-11 17:57:00
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package model

import "time"

// Soon to be removed
type UserDBModel struct {
	Id          int       `gorm:"column:id;primary_key" json:"id"`
	Username    string    `json:"username"`
	Password    string    `json:"-"` // Never serialize — callers needing it access the field directly
	Role        string    `json:"role"`
	Email       string    `json:"email"`
	Nickname    string    `json:"nickname"`
	Avatar      string    `json:"avatar"`
	Description string    `json:"description"`
	CreatedAt   time.Time `gorm:"<-:create;autoCreateTime" json:"created_at,omitempty"`
	UpdatedAt   time.Time `gorm:"<-:create;<-:update;autoUpdateTime" json:"updated_at,omitempty"`
}

func (p *UserDBModel) TableName() string {
	return "o_users"
}
