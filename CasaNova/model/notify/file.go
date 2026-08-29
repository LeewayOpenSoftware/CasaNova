/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-05-26 14:21:57
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-06-02 11:14:15
 * @FilePath: /CasaOS/model/notify/file.go
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package notify

type File struct {
	Finished       bool   `json:"finished"`
	ProcessedSize  int64  `json:"processed_size"`
	ProcessingPath string `json:"processing_path"`
	Status         string `json:"status"`
	TotalSize      int64  `json:"total_size"`
	Id             string `json:"id"`
	To             string `json:"to"`
	Type           string `json:"type"`
}
