/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-07-15 10:43:00
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-07-15 10:56:17
 * @FilePath: /CasaOS/model/notify/storage.go
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package notify

type StorageMessage struct {
	Type   string `json:"type"`   //sata,usb
	Action string `json:"action"` //remove add
	Path   string `json:"path"`
	Volume string `json:"volume"`
	Size   uint64 `json:"size"`
}
