/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-07-26 11:17:17
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-07-27 15:25:07
 * @FilePath: /CasaOS/service/model/o_shares.go
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package model

type SharesDBModel struct {
	ID        uint   `gorm:"column:id;primary_key" json:"id"`
	Anonymous bool   `json:"anonymous"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	Updated   int64  `gorm:"autoUpdateTime"`
	Created   int64  `gorm:"autoCreateTime"`
}

func (p *SharesDBModel) TableName() string {
	return "o_shares"
}
