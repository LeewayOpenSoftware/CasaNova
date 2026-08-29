module github.com/LeewayOpenSoftware/CasaNova-Virt

go 1.24

require (
	github.com/LeewayOpenSoftware/CasaNova-Common v0.4.9-alpha6
	github.com/digitalocean/go-libvirt v0.0.0-20240709142323-d8406205c752
	github.com/gorilla/websocket v1.5.3
	github.com/labstack/echo/v4 v4.12.0
	go.uber.org/zap v1.24.0
	gopkg.in/ini.v1 v1.67.0
)

require (
	github.com/golang-jwt/jwt v3.2.2+incompatible // indirect
	github.com/golang-jwt/jwt/v4 v4.5.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.0.0 // indirect
	github.com/labstack/echo-jwt/v4 v4.2.0 // indirect
	github.com/labstack/gommon v0.4.2 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/samber/lo v1.38.1 // indirect
	github.com/tidwall/gjson v1.17.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasttemplate v1.2.2 // indirect
	go.uber.org/atomic v1.10.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/crypto v0.25.0 // indirect
	golang.org/x/exp v0.0.0-20220303212507-bbda1eaf7a17 // indirect
	golang.org/x/net v0.25.0 // indirect
	golang.org/x/sys v0.22.0 // indirect
	golang.org/x/text v0.20.0 // indirect
	golang.org/x/time v0.5.0 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)

// CasaNova-Common is developed in-tree as a sibling checkout. Mirrors the
// replace directive used by every other CasaNova microservice (see
// ../CasaNova-LocalStorage/go.mod). The upstream module path is private.
replace github.com/LeewayOpenSoftware/CasaNova-Common => ../CasaNova-Common
