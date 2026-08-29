package oauthflow

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The claim this package exists to make, in one sentence: a callback that
// completes a privileged, persistent action must present a token that THIS box
// issued to an identified principal, for THIS provider, recently, and that has
// never been spent before.
//
// Every test below breaks exactly one clause of that sentence.

func newTestAuthority(t *testing.T) (*Authority, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "oauth")
	a := New(dir)
	if !a.durable {
		t.Fatalf("test authority is not durable (%s); the persistence invariants below "+
			"would silently measure nothing", a.storeFailure)
	}
	return a, dir
}

func mustBegin(t *testing.T, a *Authority, p Provider) string {
	t.Helper()
	token, _, err := a.Begin(p, 7, "admin", "https://box.example")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return token
}

// A token nobody issued is not a token.
func TestConsumeRejectsAStateThatWasNeverIssued(t *testing.T) {
	a, _ := newTestAuthority(t)
	// Well-formed shape, never issued — this is the whole attack: a stranger
	// arriving at the callback with something that looks right.
	forged := strings.Repeat("A", 43)
	if _, err := a.Consume(forged, ProviderGoogleDrive); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("a forged but well-formed state was accepted (err=%v). The callback would "+
			"have gone on to persist a provider credential and mount it as root.", err)
	}
	if _, err := a.Consume("", ProviderGoogleDrive); !errors.Is(err, ErrMalformedState) {
		t.Fatalf("an empty state was accepted (err=%v) — that is the exact pre-fix request: "+
			"GET /v1/recover/GoogleDrive?code=…", err)
	}
}

// One authorization, one completion. Replay is the difference between "a user
// connected their drive" and "an attacker can re-run that at will".
func TestStateIsSingleUse(t *testing.T) {
	a, _ := newTestAuthority(t)
	token := mustBegin(t, a, ProviderGoogleDrive)

	if _, err := a.Consume(token, ProviderGoogleDrive); err != nil {
		t.Fatalf("first use of a freshly issued state failed: %v", err)
	}
	if _, err := a.Consume(token, ProviderGoogleDrive); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("the same state was spent twice (err=%v); the callback is replayable", err)
	}
}

// :type is a free path parameter. A token issued for one provider must not open
// the credential store of another.
func TestStateIsBoundToItsProvider(t *testing.T) {
	a, _ := newTestAuthority(t)
	token := mustBegin(t, a, ProviderGoogleDrive)

	if _, err := a.Consume(token, ProviderDropbox); !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("a GoogleDrive state completed a Dropbox callback (err=%v)", err)
	}
	// A mismatch means the state was tampered with in transit, so it is burned
	// rather than left live for the attacker to try against every provider.
	if _, err := a.Consume(token, ProviderGoogleDrive); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("a state survived a provider mismatch (err=%v); it can be probed against "+
			"each provider in turn", err)
	}
}

func TestStateExpires(t *testing.T) {
	a, _ := newTestAuthority(t)
	base := time.Now()
	a.now = func() time.Time { return base }

	token := mustBegin(t, a, ProviderDropbox)
	a.now = func() time.Time { return base.Add(DefaultTTL + time.Second) }

	if _, err := a.Consume(token, ProviderDropbox); err == nil {
		t.Fatal("a state older than its TTL still completed a callback")
	}
	if got := a.Pending(); got != 0 {
		t.Fatalf("expired flows are still held: %d", got)
	}
}

// The replay window is a FIXED number of minutes, not "whatever DefaultTTL
// happens to say today".
//
// TestStateExpires above advances the clock by DefaultTTL+1s, so it re-derives
// its own deadline from the very constant it is supposed to be guarding:
// raising DefaultTTL to sixteen hours moves the test's clock with it and the
// test still passes. The certification pass proved that empirically — a x100
// mutation of DefaultTTL survived the entire suite, so the replay window was
// anchored by nothing.
//
// A short TTL is half the defence against replay: the other half (single use,
// provider binding, principal binding) all assume the token stops existing
// soon. So the bound is stated here in absolute time, and from BOTH sides,
// because the two failure modes are not symmetric in the way that matters:
//
//   - too long is the security failure — an authorization in flight is a live
//     capability to write a provider credential and root-mount it;
//   - too short is the availability failure, and it is the one that would
//     actually break users: a human on a provider consent screen with 2FA and
//     an account chooser routinely takes more than a minute, and a TTL tuned
//     down to look safe would refuse legitimate callbacks with the same
//     indistinguishable error the attacker gets. Pinning the lower bound is
//     what stops "make the test pass" from becoming "make the feature fail".
//
// Mutations that must fail this test: DefaultTTL*100 (upper bound fires),
// DefaultTTL/100 (lower bound fires).
func TestReplayWindowIsBoundedInAbsoluteTime(t *testing.T) {
	const (
		// A consent round-trip that takes this long is a normal user, not an
		// attacker sitting on a stolen state.
		minConsentWindow = 2 * time.Minute
		// Past this, an unspent authorization is not "in flight" any more.
		maxReplayWindow = 15 * time.Minute
	)

	t.Run("still valid inside a real consent round-trip", func(t *testing.T) {
		a, _ := newTestAuthority(t)
		base := time.Now()
		a.now = func() time.Time { return base }
		token := mustBegin(t, a, ProviderGoogleDrive)

		a.now = func() time.Time { return base.Add(minConsentWindow) }
		if _, err := a.Consume(token, ProviderGoogleDrive); err != nil {
			t.Fatalf("a callback returning %v after the flow started was refused (err=%v); "+
				"the TTL is now shorter than a human consent screen and the feature is "+
				"broken for legitimate users", minConsentWindow, err)
		}
	})

	t.Run("refused past the outer bound", func(t *testing.T) {
		a, _ := newTestAuthority(t)
		base := time.Now()
		a.now = func() time.Time { return base }
		token := mustBegin(t, a, ProviderGoogleDrive)

		a.now = func() time.Time { return base.Add(maxReplayWindow) }
		// Either refusal is correct and which one appears is timing-dependent:
		// pruneLocked drops the flow before the lookup when it is already past
		// its expiry, so the caller sees ErrUnknownState rather than
		// ErrExpiredState. Both burn the token; accepting either keeps this
		// test on the invariant instead of on the pruning implementation.
		_, err := a.Consume(token, ProviderGoogleDrive)
		if !errors.Is(err, ErrExpiredState) && !errors.Is(err, ErrUnknownState) {
			t.Fatalf("a state issued %v earlier still completed a callback (err=%v). The "+
				"replay window for a credential-writing capability must not be that "+
				"wide — DefaultTTL is %v", maxReplayWindow, err, DefaultTTL)
		}
	})
}

func TestBeginRefusesAnUnidentifiedPrincipal(t *testing.T) {
	a, _ := newTestAuthority(t)
	// RoleFromContext fails closed to guest with user id 0. A flow with no
	// principal is exactly the state machine that had no principal to begin
	// with, so it must not be constructible.
	if _, _, err := a.Begin(ProviderGoogleDrive, 0, "guest", "https://box.example"); !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("a flow was registered with no principal (err=%v)", err)
	}
	if _, _, err := a.Begin(Provider("Nope"), 1, "admin", "https://box.example"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("a flow was registered for an unknown provider (err=%v)", err)
	}
	if _, _, err := a.Begin(ProviderGoogleDrive, 1, "admin", "  "); !errors.Is(err, ErrNoOrigin) {
		t.Fatalf("a flow was registered with no origin (err=%v)", err)
	}
}

// A restart mid-flow must not strand the user...
func TestPendingFlowSurvivesARestart(t *testing.T) {
	a, dir := newTestAuthority(t)
	token := mustBegin(t, a, ProviderOnedrive)

	restarted := New(dir)
	if _, err := restarted.Consume(token, ProviderOnedrive); err != nil {
		t.Fatalf("a flow in flight across a restart was lost: %v", err)
	}
}

// ...and a SPENT flow must not come back to life. This is the replay window a
// memory-only implementation would have left open across a service restart.
func TestSpentFlowDoesNotSurviveARestart(t *testing.T) {
	a, dir := newTestAuthority(t)
	token := mustBegin(t, a, ProviderOnedrive)
	if _, err := a.Consume(token, ProviderOnedrive); err != nil {
		t.Fatalf("first use failed: %v", err)
	}

	restarted := New(dir)
	if _, err := restarted.Consume(token, ProviderOnedrive); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("a spent state was resurrected by a restart (err=%v)", err)
	}
}

func TestExpiredFlowIsNotRestoredByARestart(t *testing.T) {
	a, dir := newTestAuthority(t)
	base := time.Now()
	a.now = func() time.Time { return base }
	token := mustBegin(t, a, ProviderDropbox)

	restarted := New(dir)
	restarted.now = func() time.Time { return base.Add(DefaultTTL + time.Minute) }
	// Reload under the shifted clock: loadLocked already ran at real time, so
	// drive the assertion through Consume, which re-checks expiry.
	if _, err := restarted.Consume(token, ProviderDropbox); err == nil {
		t.Fatal("a state that outlived its TTL was honoured after a restart")
	}
}

// The mirror is a durability aid, never a place the live secret sits. Anyone who
// can read the file must not be able to complete a callback with it.
func TestTheMirrorNeverHoldsTheStateToken(t *testing.T) {
	a, dir := newTestAuthority(t)
	token := mustBegin(t, a, ProviderGoogleDrive)

	raw, err := os.ReadFile(filepath.Join(dir, storeFileName))
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("the state token is written to disk verbatim; reading the store file is " +
			"enough to complete someone else's authorization")
	}
	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("mirror is not valid JSON: %v", err)
	}
	if len(p.Flows) != 1 || p.Flows[0].Hash == "" {
		t.Fatalf("mirror does not hold the flow's hash: %+v", p.Flows)
	}
}

func TestMirrorIsNotReadableByGroupOrOther(t *testing.T) {
	a, dir := newTestAuthority(t)
	mustBegin(t, a, ProviderGoogleDrive)

	for _, path := range []string{dir, filepath.Join(dir, storeFileName)} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is mode %#o; the pending-flow store must not be group/other "+
				"readable", path, fi.Mode().Perm())
		}
	}
}

// Self-validation is of the PROPERTY, not of existence: a store file that is
// there but is not exclusively ours is discarded, not adopted.
func TestAForeignLookingMirrorIsDiscardedAndDurabilityIsDisabled(t *testing.T) {
	a, dir := newTestAuthority(t)
	mustBegin(t, a, ProviderGoogleDrive)
	store := filepath.Join(dir, storeFileName)
	if err := os.Chmod(store, 0o666); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	restarted := New(dir)
	if restarted.durable {
		t.Fatal("a world-writable pending-flow store was adopted; a local unprivileged " +
			"process could seed flows for us to honour after a restart")
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Fatalf("the rejected store was left on disk (err=%v); the next boot would try to "+
			"load it again", err)
	}
	h := restarted.Health(nil, nil)
	if h.Healthy || h.FailedInvariant != "state_store_ownership" {
		t.Fatalf("health does not name the invariant that failed: %+v", h)
	}
}

// Provisioning must not ACT on a path before it has decided it trusts it.
//
// secureDir used to chmod 0700 through the path and only afterwards Lstat it to
// see whether it was a symlink. os.Chmod follows symlinks, so a symlink planted
// at the state directory got its TARGET re-permissioned — we refused the path
// one line after having already modified something on the other end of it. The
// check now runs on a descriptor opened with O_NOFOLLOW|O_DIRECTORY, so there
// is no path resolution left between deciding and acting.
//
// Mutation that must fail this test: restore the old order, i.e. os.Chmod(dir,
// 0o700) before the ownership/type checks.
func TestSecureDirDoesNotChmodThroughAPlantedSymlink(t *testing.T) {
	root := t.TempDir()

	// The directory an attacker points us at. 0755 is the interesting mode:
	// it is what the victim chose, and 0700 is what we would impose on it.
	victim := filepath.Join(root, "victim")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	if err := os.Chmod(victim, 0o755); err != nil { // defeat umask
		t.Fatalf("chmod victim: %v", err)
	}

	link := filepath.Join(root, "oauth")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	a := New(link)

	fi, err := os.Stat(victim)
	if err != nil {
		t.Fatalf("stat victim: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("provisioning changed the symlink TARGET's mode to %#o; a symlink planted "+
			"at the state directory re-permissions any directory on this box that the "+
			"service can reach", fi.Mode().Perm())
	}
	if ents, err := os.ReadDir(victim); err != nil || len(ents) != 0 {
		t.Fatalf("provisioning wrote into the symlink target (entries=%v, err=%v)", ents, err)
	}
	if a.durable {
		t.Fatal("a symlinked state directory was adopted as ours; the mirror would be " +
			"written wherever the symlink points")
	}

	// And the half that must NOT break: refusing the store is a loss of
	// durability, not of the authority. A reporting failure that took the
	// callback authority down with it would hand an attacker a way to disable
	// cloud authorization by planting one symlink.
	token, _, err := a.Begin(ProviderGoogleDrive, 7, "admin", "https://box.example")
	if err != nil {
		t.Fatalf("the authority stopped issuing flows because its mirror was refused: %v", err)
	}
	if _, err := a.Consume(token, ProviderGoogleDrive); err != nil {
		t.Fatalf("the authority stopped completing legitimate callbacks because its mirror "+
			"was refused: %v", err)
	}
	if _, err := a.Consume(token, ProviderGoogleDrive); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("single-use stopped holding without the mirror (err=%v)", err)
	}
	if h := a.Health(nil, nil); h.Healthy || h.FailedInvariant != "state_store_ownership" {
		t.Fatalf("health does not name the invariant that failed: %+v", h)
	}
}

// Losing durability must not lose the authorization decision. This is the
// "fail-closed with no insecure fallback" clause: degradation removes a
// convenience, never a check.
func TestAuthorizationStillHoldsWithoutDurability(t *testing.T) {
	a, dir := newTestAuthority(t)
	a.disableDurabilityLocked("forced for test")

	token := mustBegin(t, a, ProviderGoogleDrive)
	if _, err := a.Consume(token, ProviderDropbox); !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("provider binding stopped holding once durability was off: %v", err)
	}
	if _, err := a.Consume(strings.Repeat("B", 43), ProviderGoogleDrive); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("an unissued state was accepted once durability was off: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, storeFileName)); !os.IsNotExist(err) {
		t.Fatal("durability was disabled but the stale mirror was left behind")
	}
}

// Overflow handling must never become a way to attack someone else. This is the
// lesson from the gateway anti-abuse subsystem that handed an attacker a
// targeted-ban primitive.
func TestOverflowEvictsOnlyTheSamePrincipal(t *testing.T) {
	a, _ := newTestAuthority(t)
	victim, _, err := a.Begin(ProviderGoogleDrive, 1, "admin", "https://box.example")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// A second principal churns well past its own cap.
	for i := 0; i < maxPendingPerPrincipal*3; i++ {
		if _, _, err := a.Begin(ProviderDropbox, 2, "user", "https://box.example"); err != nil {
			t.Fatalf("Begin #%d: %v", i, err)
		}
	}

	if _, err := a.Consume(victim, ProviderGoogleDrive); err != nil {
		t.Fatalf("another principal's churn cancelled an in-flight authorization: %v", err)
	}
	var mine int
	a.mu.Lock()
	for _, f := range a.flows {
		if f.PrincipalID == 2 {
			mine++
		}
	}
	a.mu.Unlock()
	if mine > maxPendingPerPrincipal {
		t.Fatalf("a principal holds %d pending flows; the per-principal cap is %d",
			mine, maxPendingPerPrincipal)
	}
}

func TestGlobalCapRefusesRatherThanEvicting(t *testing.T) {
	a, _ := newTestAuthority(t)
	principal := 0
	var refused bool
	// Each principal can hold maxPendingPerPrincipal; walk enough of them to
	// cross the global cap.
	for i := 0; i < maxPendingTotal+maxPendingPerPrincipal*2; i++ {
		if i%maxPendingPerPrincipal == 0 {
			principal++
		}
		if _, _, err := a.Begin(ProviderGoogleDrive, principal, "admin", "https://box.example"); err != nil {
			if !errors.Is(err, ErrTooManyPending) {
				t.Fatalf("unexpected Begin error: %v", err)
			}
			refused = true
			break
		}
	}
	if !refused {
		t.Fatalf("the pending set grew past the global cap (%d held)", a.Pending())
	}
	if got := a.Pending(); got > maxPendingTotal {
		t.Fatalf("pending set is unbounded: %d > %d", got, maxPendingTotal)
	}
}

// The identity used to name a remote comes from the PROVIDER's user-info
// response. It reaches "/mnt/"+name and a root FUSE mount, so nothing that can
// steer a path may survive it.
func TestSanitizeRemoteNameStripsEverythingThatCouldSteerAPath(t *testing.T) {
	cases := []string{
		"../../../etc/cron.d/pwn@evil.com",
		"a/b/c",
		"..",
		"",
		"   ",
		"-rf",
		"name with spaces and \x00 nul",
		"$(id)`id`;id",
		strings.Repeat("x", 500),
	}
	for _, in := range cases {
		got := SanitizeRemoteName(in, ProviderGoogleDrive, 1700000000)
		for _, bad := range []string{"/", "\\", "..", " ", "\x00", "$", "`", ";"} {
			if strings.Contains(got, bad) {
				t.Fatalf("SanitizeRemoteName(%q) = %q, which still contains %q", in, got, bad)
			}
		}
		if strings.HasPrefix(got, "-") {
			t.Fatalf("SanitizeRemoteName(%q) = %q, which reads as a command-line flag", in, got)
		}
		mp, err := MountPointFor(got)
		if err != nil {
			t.Fatalf("MountPointFor(%q) rejected a sanitized name: %v", got, err)
		}
		if !strings.HasPrefix(mp, MountRoot+"/") || strings.Contains(mp, "..") {
			t.Fatalf("SanitizeRemoteName(%q) produced mount point %q", in, mp)
		}
	}
}

func TestMountPointForRefusesAnythingOutsideMountRoot(t *testing.T) {
	for _, bad := range []string{"", "..", "../etc", "a/b", "/abs", "."} {
		if mp, err := MountPointFor(bad); err == nil {
			t.Fatalf("MountPointFor(%q) returned %q instead of refusing", bad, mp)
		}
	}
}

func TestWellFormedStateRejectsJunk(t *testing.T) {
	if WellFormedState(strings.Repeat("A", 1<<20)) {
		t.Fatal("a megabyte of junk passed the shape check and would reach the hash function")
	}
	for _, bad := range []string{"", "short", strings.Repeat("A", 42), strings.Repeat("A", 44),
		strings.Repeat("A", 42) + "/", strings.Repeat("A", 42) + "+"} {
		if WellFormedState(bad) {
			t.Fatalf("WellFormedState(%q) = true", bad)
		}
	}
	if !WellFormedState(strings.Repeat("-", 43)) {
		t.Fatal("WellFormedState rejected a valid base64url token shape")
	}
}

// Provenance: an empty remote list caused by rclone being down must never be
// committed as the baseline, or every real remote is reclassified forever.
func TestBaselineIsNotCapturedWhenTheRemoteListFailed(t *testing.T) {
	a, _ := newTestAuthority(t)
	a.EnsureBaseline(nil, errors.New("rclone socket unreachable"))
	if a.ledger.Captured {
		t.Fatal("the baseline was captured from a failed listing")
	}

	existing := []RemoteInfo{{Name: "alice_google_drive_1", Type: "drive", MountPoint: "/mnt/alice_google_drive_1"}}
	a.EnsureBaseline(existing, nil)
	if !a.ledger.Captured {
		t.Fatal("the baseline was not captured from a successful listing")
	}

	h := a.Health(existing, nil)
	if h.PreAuthorityRemotes != 1 || len(h.UnattributedRemotes) != 0 {
		t.Fatalf("a remote that predates the authority was reported as unattributed: %+v", h)
	}
}

func TestHealthNamesARemoteNoFlowCreated(t *testing.T) {
	a, _ := newTestAuthority(t)
	a.EnsureBaseline([]RemoteInfo{}, nil)

	appeared := []RemoteInfo{{Name: "mallory_google_drive_9", Type: "drive", MountPoint: "/mnt/mallory_google_drive_9"}}
	h := a.Health(appeared, nil)
	if h.Healthy || h.FailedInvariant != "remote_provenance" {
		t.Fatalf("a mounted cloud remote that no flow created did not raise the provenance "+
			"invariant: %+v", h)
	}
	if len(h.UnattributedRemotes) != 1 || h.UnattributedRemotes[0] != "mallory_google_drive_9" {
		t.Fatalf("health does not name the remote: %+v", h)
	}

	// A remote a completed flow created is attributed, not alarming.
	a.RecordRemote("mallory_google_drive_9", RemoteRecord{Provider: ProviderGoogleDrive, FlowID: "abc", PrincipalID: 3})
	h = a.Health(appeared, nil)
	if !h.Healthy || h.AttributedRemotes != 1 {
		t.Fatalf("a remote created by a completed flow was still reported: %+v", h)
	}

	// Rollback un-stamps it again.
	a.ForgetRemote("mallory_google_drive_9")
	if h := a.Health(appeared, nil); h.Healthy {
		t.Fatalf("ForgetRemote left the stamp behind: %+v", h)
	}
}

// A backup remote created through /v1/sys/cloud/remotes is not mounted, so it
// must not show up as an unexplained cloud mount. A provenance signal that cries
// wolf is a provenance signal nobody reads.
func TestUnmountedRemotesAreNotAudited(t *testing.T) {
	a, _ := newTestAuthority(t)
	a.EnsureBaseline([]RemoteInfo{}, nil)
	h := a.Health([]RemoteInfo{
		{Name: "backup_target", Type: "drive", MountPoint: ""},
		{Name: "s3thing", Type: "s3", MountPoint: "/mnt/s3thing"},
	}, nil)
	if !h.Healthy || len(h.UnattributedRemotes) != 0 {
		t.Fatalf("non-OAuth-shaped remotes were audited: %+v", h)
	}
}

func TestCanonicalProviderRefusesUnknownTypes(t *testing.T) {
	for _, bad := range []string{"", "GoogleDrive/../Dropbox", "s3", "local", "ftp", "..", "/"} {
		if p, ok := CanonicalProvider(bad); ok {
			t.Fatalf("CanonicalProvider(%q) resolved to %q; :type must never reach a "+
				"permissive default", bad, p)
		}
	}
	for in, want := range map[string]Provider{
		"GoogleDrive": ProviderGoogleDrive, "googledrive": ProviderGoogleDrive,
		"gdrive": ProviderGoogleDrive, "Dropbox": ProviderDropbox,
		"onedrive": ProviderOnedrive, "Onedrive": ProviderOnedrive,
	} {
		if got, ok := CanonicalProvider(in); !ok || got != want {
			t.Fatalf("CanonicalProvider(%q) = %q,%v; want %q", in, got, ok, want)
		}
	}
}

// A rejected callback must not cost the appliance a durable write.
//
// GET /v1/recover/:type is unauthenticated and reachable from the internet, and
// an attacker needs nothing but a well-formed 43-character string. The first
// version of the fix rewrote the durable mirror on every rejection — temp file,
// write, fsync(file), rename, fsync(dir) — while holding the authority mutex,
// measured at 10.97 ms per request against 1.86 us without it. The fix that
// closed the original hole had handed out a 5884x remote amplification
// primitive.
//
// The invariant is structural, not a wall-clock budget: a rejection that
// changes nothing writes nothing. Asserted by counting writes, so it holds on
// a loaded CI box too.
//
// Mutation that must fail this test: call rewriteLocked() unconditionally on
// the unknown-state branch of Consume.
func TestRejectedCallbacksDoNotWriteToDisk(t *testing.T) {
	a, dir := newTestAuthority(t)

	// One live flow, so the mirror exists and has content worth rewriting.
	if _, _, err := a.Begin(ProviderGoogleDrive, 1, "admin", "https://box.example"); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	writesBefore := mirrorWriteCount(t, dir)

	// A flood of forged callbacks: well-formed, unknown, all rejected.
	const forged = 200
	for i := 0; i < forged; i++ {
		state := forgedState(i)
		if _, err := a.Consume(state, ProviderGoogleDrive); err == nil {
			t.Fatalf("a forged state was accepted: %q", state)
		}
	}

	writesAfter := mirrorWriteCount(t, dir)
	if writesAfter != writesBefore {
		// The inode identifies the file, so a change means WriteFileAtomic
		// renamed a new one over it. It proves the mirror was rewritten, not
		// how many times — the kernel reuses inodes — and "rewritten at all"
		// is the property under test.
		t.Fatalf("%d rejected callbacks rewrote the durable mirror (inode %d -> %d); an "+
			"unauthenticated internet-reachable endpoint must not be a disk-write "+
			"amplification primitive", forged, writesBefore, writesAfter)
	}
}

// mirrorWriteCount reports how many times the durable mirror has been
// replaced, using its inode: WriteFileAtomic renames a fresh temp file over it,
// so a new inode is exactly one rewrite.
func mirrorWriteCount(t *testing.T, dir string) uint64 {
	t.Helper()
	var st syscall.Stat_t
	path := filepath.Join(dir, "oauth-flows.json")
	if err := syscall.Stat(path, &st); err != nil {
		return 0 // not created yet
	}
	return st.Ino
}

// forgedState builds a well-formed but unregistered state token.
func forgedState(i int) string {
	raw := make([]byte, 32)
	for j := range raw {
		raw[j] = byte(i*7 + j)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
