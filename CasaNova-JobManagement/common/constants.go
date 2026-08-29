package common

const (
	ServiceName = "CasaNova-JobManagement"
	Version     = "5.0.0"
)

var BUILD_VERSION = ""

func FullVersion() string {
	if BUILD_VERSION != "" {
		return BUILD_VERSION
	}
	return Version
}
