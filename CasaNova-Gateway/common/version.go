package common

const Version = "5.0.0"

// BUILD_VERSION is injected at compile time via -ldflags. If empty,
// falls back to Version.
var BUILD_VERSION = ""

func FullVersion() string {
	if BUILD_VERSION != "" {
		return BUILD_VERSION
	}
	return Version
}
