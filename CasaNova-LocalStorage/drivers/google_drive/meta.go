package google_drive

import (
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/internal/driver"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/internal/op"
)

import "os"

const ICONURL = "./img/driver/GoogleDrive.svg"

// OAuth credentials - set via environment variables or build-time ldflags
// DO NOT hardcode secrets in source code
var CLIENTID = envOrDefault("CASANOVA_GDRIVE_CLIENT_ID", "")
var CLIENTSECRET = envOrDefault("CASANOVA_GDRIVE_CLIENT_SECRET", "")

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type Addition struct {
	driver.RootID
	RefreshToken   string `json:"refresh_token" required:"true" omit:"true"`
	OrderBy        string `json:"order_by" type:"string" help:"such as: folder,name,modifiedTime" omit:"true"`
	OrderDirection string `json:"order_direction" type:"select" options:"asc,desc" omit:"true"`
	ClientID       string `json:"client_id" required:"true" omit:"true"`
	ClientSecret   string `json:"client_secret" required:"true" omit:"true"`
	ChunkSize      int64  `json:"chunk_size" type:"number" help:"chunk size while uploading (unit: MB)" omit:"true"`
	AuthUrl        string `json:"auth_url" type:"string"`
	Icon           string `json:"icon" type:"string" default:"./img/driver/GoogleDrive.svg"`
	Code           string `json:"code" type:"string" help:"code from auth_url" omit:"true"`
}

var config = driver.Config{
	Name:        "GoogleDrive",
	OnlyProxy:   true,
	DefaultRoot: "root",
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &GoogleDrive{}
	})
}
