package common

// ServiceName is the logical name used in gateway registration, message-bus
// event source IDs and log file names. It matches the CasaOS ecosystem
// convention (the ZimaOS oracle used "virt-management" as its LogSaveName and
// the "virt-management.url" gateway address file), and is already generic — no
// vendor branding leaks into it.
const (
	ServiceName = "virt-management"
	Version     = "0.1.0"
)

var BUILD_VERSION = ""

func FullVersion() string {
	if BUILD_VERSION != "" {
		return BUILD_VERSION
	}
	return Version
}
