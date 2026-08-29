package common

const (
	MessageBusVersion     = "5.0.0"
	MessageBusServiceName = "message-bus"

	MessageBusSourceID      = "message-bus"
	MessageBusHeartbeatName = "message-bus:heartbeat"
)

var BUILD_VERSION = ""

func FullVersion() string {
	if BUILD_VERSION != "" {
		return BUILD_VERSION
	}
	return MessageBusVersion
}
