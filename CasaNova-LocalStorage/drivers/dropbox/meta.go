package dropbox

import (
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/internal/driver"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/internal/op"
)

import "os"

const ICONURL = "./img/driver/Dropbox.svg"

// OAuth credentials - set via environment variables or build-time ldflags
var APPKEY = envOrDefault("CASANOVA_DROPBOX_APP_KEY", "")
var APPSECRET = envOrDefault("CASANOVA_DROPBOX_APP_SECRET", "")

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type Addition struct {
	driver.RootID
	RefreshToken   string `json:"refresh_token" required:"true" omit:"true"`
	AppKey         string `json:"app_key" type:"string" omit:"true"`
	AppSecret      string `json:"app_secret" type:"string" omit:"true"`
	OrderDirection string `json:"order_direction" type:"select" options:"asc,desc" omit:"true"`
	AuthUrl        string `json:"auth_url" type:"string"`
	Icon           string `json:"icon" type:"string" default:"./img/driver/Dropbox.svg"`
	Code           string `json:"code" type:"string" help:"code from auth_url" omit:"true"`
}

var config = driver.Config{
	Name:        "Dropbox",
	OnlyProxy:   true,
	DefaultRoot: "root",
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &Dropbox{}
	})
}
