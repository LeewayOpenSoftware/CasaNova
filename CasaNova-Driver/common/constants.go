package common

// ServiceName is the gateway/message-bus identity of this microservice.
// Naming policy: our own code is generic / casanova-*; we never emit "zima*"
// unless a client checks the exact string on the wire (not the case here).
const (
	Version     = "0.1.0"
	ServiceName = "driver"
)

// BUILD_VERSION is injected at link time via -ldflags.
var BUILD_VERSION = ""

func FullVersion() string {
	if BUILD_VERSION != "" {
		return BUILD_VERSION
	}
	return Version
}
