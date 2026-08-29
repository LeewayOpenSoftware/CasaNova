package tokenrevocation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// use points the authority at a temp file for one test.
func use(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "state", "token-revocations.json")
	SetPath(p)
	t.Cleanup(func() { SetPath(DefaultPath) })
	return p
}

// ---------------------------------------------------------------------------
// The security property. These assert that a revoked principal is ACTUALLY
// refused — not that a function was called or a component exists.
// ---------------------------------------------------------------------------

// Mutation that must fail this test: make IsRevoked compare with After
// instead of !After, or have it return false whenever the map lookup misses
// the exact instant.
func TestRevokedPrincipalIsRefused(t *testing.T) {
	use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	issued := time.Now().Add(-time.Hour)

	if IsRevoked("42", issued) {
		t.Fatal("principal was refused before anything revoked it")
	}
	if err := RevokeID(42); err != nil {
		t.Fatalf("RevokeID: %v", err)
	}
	if !IsRevoked("42", issued) {
		t.Fatal("a token issued before revocation was accepted — revocation does not work")
	}
}

// Revocation must not be a permanent ban: a principal who logs in again gets a
// token issued after the revocation instant, and that one has to work.
func TestTokenIssuedAfterRevocationIsAccepted(t *testing.T) {
	use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := RevokeID(7); err != nil {
		t.Fatalf("RevokeID: %v", err)
	}
	if IsRevoked("7", time.Now().Add(time.Minute)) {
		t.Fatal("a token issued after revocation was refused; the principal can never log in again")
	}
	if IsRevoked("8", time.Now().Add(-time.Hour)) {
		t.Fatal("revoking 7 also refused 8")
	}
}

// A token with no iat claim cannot be placed relative to the revocation, so
// for a principal someone deliberately revoked it must be refused.
func TestTokenWithoutIssuedAtIsRefusedForRevokedPrincipal(t *testing.T) {
	use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := RevokeID(9); err != nil {
		t.Fatalf("RevokeID: %v", err)
	}
	if !IsRevoked("9", time.Time{}) {
		t.Fatal("a token with no issue time was accepted for a revoked principal")
	}
	if IsRevoked("10", time.Time{}) {
		t.Fatal("a token with no issue time was refused for a principal nobody revoked")
	}
}

// THE defect this package exists to fix: the decision was in memory, so a
// restart forgot it while the signed token stayed valid. A fresh process must
// still refuse the token.
func TestRevocationSurvivesRestart(t *testing.T) {
	path := use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	issued := time.Now().Add(-time.Hour)
	if err := RevokeID(55); err != nil {
		t.Fatalf("RevokeID: %v", err)
	}

	// Simulate a restart: drop every scrap of in-memory state, then come up
	// again against the same file exactly as a rebooted service would.
	SetPath(path)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure after restart: %v", err)
	}

	if !IsRevoked("55", issued) {
		t.Fatal("the revocation was forgotten across a restart — the token works again")
	}
}

// The cross-service property: a second reader that never performed the
// revocation must still refuse the token. This stands in for the six services
// that had no revocation check at all.
func TestASecondReaderSeesTheRevocation(t *testing.T) {
	path := use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	issued := time.Now().Add(-time.Hour)

	// The writer (user service) revokes.
	if err := RevokeID(101); err != nil {
		t.Fatalf("RevokeID: %v", err)
	}

	// A different process comes up and only ever reads.
	SetPath(path)
	if err := Ensure(); err != nil {
		t.Fatalf("reader Ensure: %v", err)
	}
	if !IsRevoked("101", issued) {
		t.Fatal("a service that did not perform the revocation still accepted the token")
	}
}

// ---------------------------------------------------------------------------
// Fail-closed. The contract says a broken authority must refuse, never fall
// back to accepting tokens unchecked.
// ---------------------------------------------------------------------------

// A service that never calls Ensure must still enforce revocation rather than
// silently skipping it — this codebase has five prior cases of correct code
// that no caller ever reached.
func TestAuthorityProvisionsItselfOnFirstUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "token-revocations.json")
	SetPath(path)
	t.Cleanup(func() { SetPath(DefaultPath) })

	// No Ensure(), no Provision() — straight to a consultation.
	if IsRevoked("1", time.Now()) {
		t.Fatal("lazy provisioning failed; a healthy fresh install refused a valid token")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("first use did not create the list: %v", err)
	}
	if s := CurrentStatus(); s != StatusHealthy {
		t.Fatalf("status = %q after lazy provisioning, want %q", s, StatusHealthy)
	}

	// And it actually enforces, not merely exists.
	if err := RevokeID(1); err != nil {
		t.Fatalf("RevokeID: %v", err)
	}
	if !IsRevoked("1", time.Now().Add(-time.Hour)) {
		t.Fatal("a self-provisioned authority does not enforce revocation")
	}
}

// When the state cannot be established at all, the authority must refuse
// tokens rather than fall back to accepting them unchecked.
func TestUnprovisionableAuthorityRefusesEverything(t *testing.T) {
	// A regular file where the state directory belongs: MkdirAll cannot
	// succeed, so provisioning has no way to recover.
	base := t.TempDir()
	blocker := filepath.Join(base, "state")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	SetPath(filepath.Join(blocker, "token-revocations.json"))
	t.Cleanup(func() { SetPath(DefaultPath) })

	if !IsRevoked("1", time.Now()) {
		t.Fatal("an authority that cannot be provisioned accepted a token; it must fail closed")
	}
	h := CurrentHealth()
	if h.Status != StatusUnavailable {
		t.Fatalf("status = %q, want %q", h.Status, StatusUnavailable)
	}
	if h.Enforcing {
		t.Fatal("health claims it is enforcing while unprovisioned")
	}
	if h.Invariant == "" {
		t.Fatal("health does not name the invariant that failed")
	}
}

// A self-test that genuinely does not hold must refuse every token — and must
// keep refusing for as long as the cause stands. The background repair added
// for transient faults must not be able to talk a broken box back into
// enforcing.
func TestFailedSelfTestRefusesEverything(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the fault cannot be injected")
	}
	path := use(t)
	if err := Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Break it the way a real defect would: the self-test asserts that this
	// filesystem actually records a revocation, and here it cannot.
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := SelfTest(); err == nil {
		t.Fatal("self-test passed on a filesystem that cannot record a revocation")
	}
	if !IsRevoked("1", time.Now()) {
		t.Fatal("a failed self-test still accepted tokens; it must fail closed")
	}
	if h := CurrentHealth(); h.Status != StatusValidationFailed || h.Enforcing {
		t.Fatalf("health = %+v, want validation_failed and not enforcing", h)
	}

	// Let the paced repair run immediately, several times over.
	for i := 0; i < 3; i++ {
		global.mu.Lock()
		global.nextRepairAttempt = time.Time{}
		global.lastCheck = time.Time{}
		global.mu.Unlock()
		if !IsRevoked("1", time.Now()) {
			t.Fatal("the background repair restored enforcement while validation still fails")
		}
	}
}

// ---------------------------------------------------------------------------
// Provisioning: idempotent, repairing, and never silently fail-open.
// ---------------------------------------------------------------------------

func TestProvisionIsIdempotent(t *testing.T) {
	path := use(t)
	for i := 0; i < 3; i++ {
		if err := Provision(); err != nil {
			t.Fatalf("Provision #%d: %v", i+1, err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != fileMode {
		t.Fatalf("mode = %#o, want %#o", info.Mode().Perm(), fileMode)
	}
}

func TestReprovisionPreservesExistingRevocations(t *testing.T) {
	path := use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := RevokeID(3); err != nil {
		t.Fatalf("RevokeID: %v", err)
	}

	SetPath(path)
	if err := Provision(); err != nil {
		t.Fatalf("reprovision: %v", err)
	}
	global.mu.RLock()
	_, present := global.revoked["3"]
	global.mu.RUnlock()
	if !present {
		t.Fatal("reprovisioning discarded an existing revocation — that is a silent fail-open")
	}
}

func TestProvisionRepairsAWrongMode(t *testing.T) {
	path := use(t)
	if err := Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// A list other services cannot read takes all of them fail-closed.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := Provision(); err != nil {
		t.Fatalf("reprovision: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != fileMode {
		t.Fatalf("mode was not repaired: %#o", info.Mode().Perm())
	}
}

// A corrupt list must be quarantined and reported, never silently replaced
// with an empty one — that would un-revoke every principal on the box.
// A corrupt list must fail closed and STAY failed across restarts. Moving it
// aside and creating a fresh one would un-revoke every principal — the exact
// failure this package exists to prevent, just one restart later.
func TestCorruptListFailsClosedAndDoesNotSelfHealIntoAnEmptyList(t *testing.T) {
	path := use(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := Provision(); err == nil {
		t.Fatal("Provision accepted a corrupt list")
	}
	if !IsRevoked("1", time.Now()) {
		t.Fatal("a corrupt list left the authority accepting tokens")
	}
	h := CurrentHealth()
	if h.Enforcing {
		t.Fatal("health claims it is enforcing on a corrupt list")
	}
	if h.Invariant == "" {
		t.Fatal("health does not name the failed invariant")
	}

	// The evidence must survive: an operator has to be able to look at it.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the corrupt list was moved or deleted: %v", err)
	}
	if string(data) != "{not json" {
		t.Fatalf("the corrupt list was rewritten: %q", data)
	}

	// A restart must not turn it into an empty, healthy, everyone-un-revoked
	// list.
	SetPath(path)
	if !IsRevoked("1", time.Now()) {
		t.Fatal("restarting healed a corrupt list into an empty one; every revocation was silently dropped")
	}
}

// A file whose writer said it needs a newer reader must be refused rather than
// guessed at: misreading a revocation list fails open.
func TestFileRequiringANewerReaderIsRefused(t *testing.T) {
	path := use(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, _ := json.Marshal(fileFormat{
		Version:          schemaVersion + 1,
		MinReaderVersion: minReaderVersion + 1,
		Revoked:          map[string]int64{"1": 1},
	})
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Provision(); err == nil {
		t.Fatal("a file demanding a newer reader was accepted; this build cannot know what it means")
	}
	if !IsRevoked("2", time.Now()) {
		t.Fatal("an unreadable file left the authority accepting tokens")
	}
}

// The rollback property. The Rollback button exists, nothing in the update path
// backs this file up, and a total refusal here takes the whole UI down with
// only SSH left. So a later build that merely ADDS fields must stay readable:
// compatibility is decided by min_reader_version, not by the version number.
func TestALaterBuildThatAddedFieldsStaysReadableAfterRollback(t *testing.T) {
	path := use(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	at := time.Now().Add(-time.Minute).Unix()
	// What a future build writes when it adds a field but changes no meaning.
	future := `{"version":9,"min_reader_version":1,"revoked":{"4":` + itoa(at) + `},"some_future_field":{"a":1}}`
	if err := os.WriteFile(path, []byte(future), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := Ensure(); err != nil {
		t.Fatalf("a rollback onto this build was left with no administration path: %v", err)
	}
	if h := CurrentHealth(); !h.Enforcing {
		t.Fatalf("health after rollback = %+v, want enforcing", h)
	}
	if IsRevoked("5", time.Now()) {
		t.Fatal("a rolled-back build refuses every token; the box is only reachable by SSH")
	}
	if !IsRevoked("4", time.Unix(at, 0).Add(-time.Hour)) {
		t.Fatal("the rolled-back build dropped a revocation the newer build had recorded")
	}
}

// The forcing function for the contract above. If this fails, the on-disk shape
// changed: decide whether the change alters what an existing field MEANS. If it
// does, raise minReaderVersion in the same commit so older builds refuse the
// file instead of misreading it. If it only adds a field, leave
// minReaderVersion alone and update this golden string.
func TestOnDiskFormatIsPinned(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pinned.json")
	if err := writeFile(p, list{
		entries: map[string]time.Time{"12": time.Unix(1700000000, 0)},
		before:  time.Unix(1690000000, 0),
	}); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	const want = `{"version":1,"min_reader_version":1,"revoked_before":1690000000,"revoked":{"12":1700000000}}`
	if string(data) != want {
		t.Fatalf("on-disk format changed.\n got: %s\nwant: %s\n"+
			"If an existing field changed meaning, raise minReaderVersion so a rolled-back build refuses "+
			"this file. If a field was only added, update this golden string.", data, want)
	}
}

// The unversioned first draft must still be readable.
func TestLegacyUnversionedFileMigrates(t *testing.T) {
	path := use(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacy := []byte(`{"revoked":{"77":` + itoa(time.Now().Unix()) + `}}`)
	if err := os.WriteFile(path, legacy, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure on legacy file: %v", err)
	}
	if !IsRevoked("77", time.Now().Add(-time.Hour)) {
		t.Fatal("a revocation from the unversioned format was lost on upgrade")
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Self-validation actually exercises the boundary.
// ---------------------------------------------------------------------------

func TestSelfTestLeavesNoProbeBehind(t *testing.T) {
	path := use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := f.Revoked[probePrincipal]; present {
		t.Fatal("the self-test probe was left in the persisted list")
	}
	if h := CurrentHealth(); h.Entries != 0 {
		t.Fatalf("entries = %d after a clean start, want 0", h.Entries)
	}
}

func TestSelfTestRefusesWhenNotProvisioned(t *testing.T) {
	use(t)
	if err := SelfTest(); err == nil {
		t.Fatal("SelfTest passed without provisioning")
	}
	// Read the recorded state directly: calling CurrentStatus would trigger
	// lazy provisioning, which is correct behaviour but not what this asserts.
	global.mu.RLock()
	status, validated := global.status, global.validated
	global.mu.RUnlock()
	if status != StatusUnavailable {
		t.Fatalf("status = %q, want %q", status, StatusUnavailable)
	}
	if validated {
		t.Fatal("SelfTest marked the authority validated without provisioning")
	}
}

// ---------------------------------------------------------------------------
// Bounded state.
// ---------------------------------------------------------------------------

func TestEntriesOlderThanEveryTokenArePruned(t *testing.T) {
	use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if err := revokeAt("ancient", time.Now().Add(-retentionPeriod-time.Hour)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Any later write prunes.
	if err := RevokeID(1); err != nil {
		t.Fatalf("RevokeID: %v", err)
	}

	global.mu.RLock()
	_, present := global.revoked["ancient"]
	global.mu.RUnlock()
	if present {
		t.Fatal("an entry older than every possible token was not pruned; the file grows forever")
	}
}

func TestConcurrentReadsAndRevocations(t *testing.T) {
	use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_ = IsRevoked("1", time.Now())
		}
	}()
	for i := 0; i < 20; i++ {
		if err := RevokeID(i); err != nil {
			t.Errorf("RevokeID(%d): %v", i, err)
		}
	}
	<-done
}

// A refresh must never LOWER the revocation cutoff.
//
// Provisioning applies a sanitizing floor to a list written by a build that
// predates the reader contract, because such a file cannot be interpreted
// safely. The per-second refresh on the REQUEST path read the same file with no
// contract check at all — so a pre-contract file appearing on disk after
// provisioning silently lowered the floor a second later and handed back the
// sessions provisioning had just cut off.
//
// The obvious caller was protected while the equivalent primitive stayed open.
// That is this codebase's signature defect, and here it undid a security
// decision one second after it was made.
//
// Mutation that must fail this test: assign s.revokedBefore unconditionally in
// reload().
func TestARefreshCannotLowerTheRevocationCutoff(t *testing.T) {
	path := use(t)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// A cutoff is in force: everything issued before now is refused.
	cutoff := time.Now()
	global.mu.Lock()
	global.revokedBefore = cutoff
	global.mu.Unlock()
	stale := cutoff.Add(-time.Hour)
	if !IsRevoked("42", stale) {
		t.Fatal("precondition: a token issued before the cutoff should be refused")
	}

	// A pre-contract file (no version field) appears on disk carrying a much
	// older floor — exactly what an older build writes, or what a restore from
	// backup drops in.
	older := cutoff.Add(-24 * time.Hour)
	legacy := []byte(`{"revoked":{},"before":` + itoa(older.Unix()) + `}`)
	if err := os.WriteFile(path, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	// Expire the refresh window so the next consultation re-reads the file,
	// exactly as the request path does every second.
	global.mu.Lock()
	global.lastCheck = time.Time{}
	global.lastMod = time.Time{}
	global.lastSize = 0
	global.mu.Unlock()

	if !IsRevoked("42", stale) {
		t.Fatal("a refresh lowered the cutoff and un-revoked a session the authority had already " +
			"cut off; a refresh may tighten revocation, never loosen it")
	}
}
