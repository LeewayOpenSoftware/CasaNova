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

// DISPLAY_VERSION is the user-facing release label (calendar YY.MM).
//
// NOTHING reads this constant. The label users actually see comes from the
// Makefile's DISPLAY_VERSION, injected into the frontend build as
// VITE_DISPLAY_VERSION — and the two had drifted apart (26.06 here, 26.08
// there, with 26.07 hardcoded as the UI's fallback). Three copies of one
// fact, none of them agreeing, is how a release ships announcing the wrong
// version. Kept in step with the Makefile and marked as the mirror it is;
// the Makefile is the source.
const DISPLAY_VERSION = "26.10.1"

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
