package model

import "sync/atomic"

type CommonModel struct {
	RuntimePath string
}

type APPModel struct {
	LogPath      string
	LogSaveName  string
	LogFileExt   string
	AppStorePath string
	AppsPath     string
}

type ServerModel struct {
	AppStoreList []string `ini:"appstore,,allowshadow"`
}

type GlobalModel struct {
	OpenAIAPIKey string
}

type CasaOSGlobalVariables struct {
	AppChange atomic.Bool
}
