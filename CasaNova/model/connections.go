/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-07-27 10:30:43
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-08-04 20:06:04
 * @FilePath: /CasaOS/model/connections.go
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package model

type Connections struct {
	ID         uint   `json:"id"`
	Username   string `json:"username"`
	Password   string `json:"-"` // Never serialize — callers needing it access the field directly
	Host       string `json:"host"`
	Port       string `json:"port"`
	MountPoint string `json:"mount_point"`
}
