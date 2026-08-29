package model

// User-facing feature models: profile, notifications, drives, search engines.
// Consolidated from user.go, notify.go, drive.go, search.go.

// UserInfo is the public profile data for a user.
type UserInfo struct {
	NickName string `json:"nick_name"`
	Desc     string `json:"desc"`
	ShareId  string `json:"share_id"`
	Avatar   string `json:"avatar"`
	Version  int    `json:"version,omitempty"`
}

// NotifyMssage is a WebSocket notification payload.
// (Name kept with original spelling to preserve wire-format compatibility.)
type NotifyMssage struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

// Drive describes a cloud drive provider (Google Drive, OneDrive, Dropbox, etc.)
// that users can connect for remote file sync.
type Drive struct {
	Name    string `json:"name"`
	Icon    string `json:"icon"`
	AuthUrl string `json:"auth_url"`
}

// SearchEngine is a configurable search provider shown in the dashboard.
type SearchEngine struct {
	Name      string   `json:"name"`
	Icon      string   `json:"icon"`
	SearchUrl string   `json:"search_url"`
	RecoUrl   string   `json:"reco_url"`
	Data      []string `json:"data"`
}
