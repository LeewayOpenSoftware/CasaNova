package v1

import (
	"strings"
	"testing"
)

// Real `apt-get update` output from the production box (LC_ALL=C), where every
// repository refreshed except one dead third-party PPA. apt exits non-zero for
// this, and the panel used to render it as "package lists could not be
// refreshed" — an alarm the administrator cannot act on, over data that is in
// fact current.
const aptUpdatePartialFailure = `Hit:1 https://download.docker.com/linux/ubuntu noble InRelease
Hit:2 http://security.ubuntu.com/ubuntu noble-security InRelease
Hit:3 http://ar.archive.ubuntu.com/ubuntu noble InRelease
Hit:4 http://ar.archive.ubuntu.com/ubuntu noble-updates InRelease
Hit:5 https://esm.ubuntu.com/apps/ubuntu noble-apps-security InRelease
Hit:6 http://ar.archive.ubuntu.com/ubuntu noble-backports InRelease
Hit:7 https://esm.ubuntu.com/apps/ubuntu noble-apps-updates InRelease
Hit:8 https://ppa.launchpadcontent.net/cappelikan/ppa/ubuntu noble InRelease
Hit:9 https://esm.ubuntu.com/infra/ubuntu noble-infra-security InRelease
Ign:10 https://ppa.launchpadcontent.net/jonathonf/ffmpeg-4/ubuntu noble InRelease
Hit:11 https://esm.ubuntu.com/infra/ubuntu noble-infra-updates InRelease
Err:12 https://ppa.launchpadcontent.net/jonathonf/ffmpeg-4/ubuntu noble Release
  404  Not Found [IP: 185.125.189.188 443]
Reading package lists...
E: The repository 'https://ppa.launchpadcontent.net/jonathonf/ffmpeg-4/ubuntu noble Release' does not have a Release file.`

// Nothing answered: no network, or the lock was held for the whole timeout.
const aptUpdateTotalFailure = `Err:1 http://ar.archive.ubuntu.com/ubuntu noble InRelease
  Could not connect to ar.archive.ubuntu.com:80
Err:2 http://security.ubuntu.com/ubuntu noble-security InRelease
  Could not connect to security.ubuntu.com:80
E: Some index files failed to download.`

func TestAptPartialRefreshIsNotStale(t *testing.T) {
	refreshed, failed := parseAptUpdateOutput(aptUpdatePartialFailure)

	if refreshed != 10 {
		t.Fatalf("refreshed = %d, want 10", refreshed)
	}
	// Stale is "nothing came back", not "apt exited non-zero".
	if refreshed == 0 {
		t.Fatal("a refresh where 10 repositories answered must not count as stale")
	}
	if len(failed) != 1 {
		t.Fatalf("failed = %v, want exactly the dead PPA", failed)
	}
	if failed[0] != "https://ppa.launchpadcontent.net/jonathonf/ffmpeg-4/ubuntu" {
		t.Fatalf("failed[0] = %q — the administrator needs the offending URL", failed[0])
	}
}

func TestAptTotalRefreshFailureIsStale(t *testing.T) {
	refreshed, failed := parseAptUpdateOutput(aptUpdateTotalFailure)

	if refreshed != 0 {
		t.Fatalf("refreshed = %d, want 0", refreshed)
	}
	if len(failed) != 2 {
		t.Fatalf("failed = %v, want both repositories named", failed)
	}
}

// A clean run must report no failures at all, so the panel stays quiet.
func TestAptCleanRefresh(t *testing.T) {
	refreshed, failed := parseAptUpdateOutput(`Hit:1 http://ar.archive.ubuntu.com/ubuntu noble InRelease
Get:2 http://security.ubuntu.com/ubuntu noble-security InRelease [126 kB]
Reading package lists...`)

	if refreshed != 2 {
		t.Fatalf("refreshed = %d, want 2", refreshed)
	}
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
}

// The same repository failing twice (InRelease then Release, which is exactly
// what a dead PPA produces) must be reported once, not twice.
func TestAptFailedReposAreDeduplicated(t *testing.T) {
	_, failed := parseAptUpdateOutput(`Hit:1 http://ar.archive.ubuntu.com/ubuntu noble InRelease
Err:2 https://ppa.example.com/x/ubuntu noble InRelease
  404  Not Found
Err:3 https://ppa.example.com/x/ubuntu noble Release
  404  Not Found`)

	if len(failed) != 1 {
		t.Fatalf("failed = %v, want the repository listed once", failed)
	}
}

// The one flag that keeps the upgrade endpoints asynchronous.
//
// systemd-run without --no-block waits for the start job, and for
// Type=oneshot the start job is the command itself — so launchAptUnit,
// written to fire-and-forget, blocked for apt's entire runtime. The handler
// answered "Upgrade started" only after the upgrade finished; the browser
// client timed out at 10 s and reported "could not start the upgrade" while
// apt ran to a successful finish behind the error toast.
func TestAptUnitLaunchesWithoutBlocking(t *testing.T) {
	args := aptUnitArgs("desc", "apt-get -y upgrade")

	sawNoBlock := false
	for i, a := range args {
		if a == "--" {
			break // flags after the separator belong to the payload, not systemd-run
		}
		if a == "--no-block" {
			sawNoBlock = true
		}
		_ = i
	}
	if !sawNoBlock {
		t.Fatal("systemd-run is invoked without --no-block: with Type=oneshot the call blocks " +
			"for apt's full runtime, and the 10 s HTTP client reports a started upgrade as failed")
	}
}

// The exit-code file is how GetAptUpgradeStatus distinguishes success from
// failure once the unit is gone (--collect removes it). The wrapped command
// must write it no matter how apt exits.
func TestAptUnitRecordsExitCode(t *testing.T) {
	args := aptUnitArgs("desc", "apt-get -y upgrade")
	payload := args[len(args)-1]
	if !strings.Contains(payload, "echo $ec > "+aptUpgradeStatusFile) {
		t.Fatalf("wrapped command does not persist the exit code; status polling cannot tell success from failure: %q", payload)
	}
}
