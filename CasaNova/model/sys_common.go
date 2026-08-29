/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-05-13 18:15:46
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-09-02 22:12:34
 * @FilePath: /CasaOS/model/sys_common.go
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package model

import "time"

// System configuration
type SysInfoModel struct {
	Name string //
}

type ServerModel struct {
	HttpPort     string
	RunMode      string
	ServerApi    string
	LockAccount  bool
	Token        string
	USBAutoMount string
	UpdateUrl    string
}

type APPModel struct {
	LogPath        string
	LogSaveName    string
	LogFileExt     string
	DateStrFormat  string
	DateTimeFormat string
	UserDataPath   string
	TimeFormat     string
	DateFormat     string
	DBPath         string
	ShellPath      string
}
type CommonModel struct {
	RuntimePath string
}

type Result struct {
	Success int         `json:"success" example:"200"`
	Message string      `json:"message" example:"ok"`
	Data    interface{} `json:"data" example:"result"`
}

// redis
type RedisModel struct {
	Host        string
	Password    string
	MaxIdle     int
	MaxActive   int
	IdleTimeout time.Duration
}

type SystemConfig struct {
	ConfigPath string `json:"config_path"`
}

type FileSetting struct {
	ShareDir    []string `json:"share_dir" delim:"|"`
	DownloadDir string   `json:"download_dir"`
}
type BaseInfo struct {
	Hash       string `json:"i"`
	Version    string `json:"v"`
	Channel    string `json:"c,omitempty"`
	DriveModel string `json:"m,omitempty"`
}
