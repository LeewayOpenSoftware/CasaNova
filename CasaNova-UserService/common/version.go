package common

const Version = "5.0.0"
const SERVICENAME = "CasaNova-UserService"

var BUILD_VERSION = ""

func FullVersion() string {
	if BUILD_VERSION != "" {
		return BUILD_VERSION
	}
	return Version
}
