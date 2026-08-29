package common

const (
	Version           = "5.0.0"
	ServiceName       = "local-storage"
	DefaultMountPoint = "/DATA"
)

var BUILD_VERSION = ""

func FullVersion() string {
	if BUILD_VERSION != "" {
		return BUILD_VERSION
	}
	return Version
}
