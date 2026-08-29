/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-05-26 14:21:11
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-05-27 11:15:59
 * @FilePath: /CasaOS/model/notify/result.go
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */

package notify

// Notify struct for Notify
type NotifyModel struct {
	Data  interface{} `json:"data"`
	State string      `json:"state"`
}
