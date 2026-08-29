package common

const (
	SERVICENAME = "casanova"
	BODY        = " "
	RANW_NAME   = "CasaNova-RemoteAccess"
)

// VERSION is the base semantic version reported by the API and used by
// IsNeedUpdate's component-wise comparison. v0.1 (26.06) is the first
// publishable CasaNova release; earlier 5.0.0.devN tarballs were
// internal test builds and the version reset is intentional.
//
// Upgrade compat: the self-update script (casanova-update.sh) accepts
// any tarball regardless of semver direction, so an existing test
// install on 5.0.0.devN can still apply this build cleanly.
const VERSION = "0.1.0"

// DISPLAY_VERSION is the user-facing release label shown on the About
// page and update notifications. Calendar-versioned YY.MM derived from
// the release month.
const DISPLAY_VERSION = "26.06"

// CHANNEL is the default release channel. Overridden by -ldflags.
// Values: "dev", "stable", "beta"
var CHANNEL = "dev"

// BUILD_VERSION is the full build identifier. It can be overridden at
// compile time via:
//
//	go build -ldflags "-X github.com/LeewayOpenSoftware/CasaNova/common.BUILD_VERSION=0.1.0.dev1.0612260700"
//
// Format: MAJOR.MINOR.PATCH.CHANNEL_N.MMDDHHMM
//   - dev1 / st1 / beta1 = channel + build number within that channel
//   - MMDDHHMM = month-day-hour-minute of the build
//
// The default value is used for local / untagged builds.
var BUILD_VERSION = "0.1.0.dev1.06122607"

// FullVersion returns the most specific version string available.
func FullVersion() string {
	if BUILD_VERSION != "" {
		return BUILD_VERSION
	}
	return VERSION
}
