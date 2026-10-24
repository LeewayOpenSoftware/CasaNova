module github.com/LeewayOpenSoftware/CasaNova-Driver

go 1.26.0

toolchain go1.26.10

// Runtime defaults stay those of 26.10 (go 1.24) while the go line follows
// the dependency floor; see docs/release/26.10.1/MAINTENANCE_CHANGELOG_26.10.1.md.
godebug default=go1.24

require (
	github.com/LeewayOpenSoftware/CasaNova-Common v0.4.9-alpha6
	github.com/coreos/go-systemd/v22 v22.5.0
	github.com/labstack/echo/v4 v4.12.0
	go.uber.org/zap v1.24.0
	gopkg.in/ini.v1 v1.67.0
)

require (
	github.com/golang-jwt/jwt v3.2.2+incompatible // indirect
	github.com/golang-jwt/jwt/v4 v4.5.2 // indirect
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
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/exp v0.0.0-20220303212507-bbda1eaf7a17 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.5.0 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)

// The Common module is a private repo; build against the local sibling checkout,
// exactly like every other CasaNova service does.
replace github.com/LeewayOpenSoftware/CasaNova-Common => ../CasaNova-Common
