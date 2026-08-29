package tokenrevocation

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The upgrade that signs everyone out must SAY SO.
//
// Adopting a pre-contract list refuses every token issued before that moment
// (see sanitizeLegacyList). That is deliberate — a list written by a build that
// could lose revocations cannot be trusted, and nothing on disk says which
// entries went missing — but on a live box it looks exactly like a break-in
// response: everybody is signed out at once, on a start nobody announced.
//
// So these tests do not re-litigate the cutoff (TestAListFromABuildThatCould
// LoseRevocationsIsSanitizedOnce fixes that). They fix its VISIBILITY: the
// operator can find out, from the journal and from health, that this box did it
// and why — and, just as important, is not told so when it did not happen.
// ---------------------------------------------------------------------------

// captured is one notice the package emitted.
type captured struct {
	path    string
	at      time.Time
	entries int
}

// captureNotices swaps the announcement seam for the duration of one test.
func captureNotices(t *testing.T) *[]captured {
	t.Helper()
	got := []captured{}
	prev := announceLegacyCutoff
	announceLegacyCutoff = func(path string, at time.Time, entries int) {
		got = append(got, captured{path, at, entries})
	}
	t.Cleanup(func() { announceLegacyCutoff = prev })
	return &got
}

func seedLegacy(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// The start that signs everyone out announces it, records it in health, and
// really does refuse the sessions it says it refuses.
//
// Mutations that must fail this test: make sanitizeLegacyList a no-op (or have
// it report false); drop the announceLegacyCutoff call in Provision; stop
// recording legacyCutoffApplied/legacyCutoffAt on the state.
func TestTheStartThatSignsEveryoneOutAnnouncesIt(t *testing.T) {
	path := use(t)
	notices := captureNotices(t)

	// Exactly what a build that could lose revocations wrote: no
	// min_reader_version, and one revocation that did survive.
	old := time.Now().Add(-2 * time.Hour).Unix()
	seedLegacy(t, path, `{"version":1,"revoked":{"9":`+itoa(old)+`}}`)

	liveSession := time.Now().Add(-30 * time.Minute)

	if err := Ensure(); err != nil {
		t.Fatalf("Ensure on a pre-contract list: %v", err)
	}

	// The thing being announced actually happened. Asserted here so a build
	// that announces without cutting — or cuts without announcing — fails.
	if !IsRevoked("42", liveSession) {
		t.Fatal("no cutoff was applied, so there is nothing to announce")
	}

	if len(*notices) != 1 {
		t.Fatalf("the box signed everyone out and emitted %d notices; the operator has no way to learn why", len(*notices))
	}
	n := (*notices)[0]
	if n.path != path {
		t.Fatalf("notice names %q, want the list it migrated %q", n.path, path)
	}

	h := CurrentHealth()
	if !h.LegacyCutoffApplied {
		t.Fatalf("health does not report that this start applied the cutoff: %+v", h)
	}
	if h.LegacyCutoffAt == "" {
		t.Fatalf("health reports a cutoff with no instant: %+v", h)
	}
	if h.RevokedBefore == "" {
		t.Fatalf("health does not report the floor now in force: %+v", h)
	}
	// The two must agree, or the operator cannot line the notice up with the
	// sessions that stopped working.
	if h.LegacyCutoffAt != h.RevokedBefore {
		t.Fatalf("health reports cutoff at %s but a floor at %s", h.LegacyCutoffAt, h.RevokedBefore)
	}
	if want := n.at.Format(time.RFC3339); want != h.LegacyCutoffAt {
		t.Fatalf("the notice says %s and health says %s", want, h.LegacyCutoffAt)
	}
	// The floor persisted on disk is the one that was reported.
	l, err := readFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if l.before.Unix() != n.at.Unix() {
		t.Fatalf("announced a cutoff at %v, the file carries %v", n.at, l.before)
	}
	// Counted the way health counts, so notice and health cannot disagree.
	if n.entries != 1 {
		t.Fatalf("notice counts %d revocations, the migrated list has 1", n.entries)
	}
}

// A box that did NOT sign anyone out must not be told that it did: a false
// cutoff notice sends an operator hunting a security event that never happened,
// and one that cries wolf on every start is one nobody reads on the start that
// matters.
//
// Mutation that must fail this test: apply/announce the cutoff unconditionally
// rather than only for a list below the reader contract.
func TestAFreshInstallAnnouncesNoCutoff(t *testing.T) {
	use(t)
	notices := captureNotices(t)

	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(*notices) != 0 {
		t.Fatalf("a fresh install announced %d cutoffs: %+v", len(*notices), *notices)
	}
	h := CurrentHealth()
	if h.LegacyCutoffApplied || h.LegacyCutoffAt != "" {
		t.Fatalf("a fresh install reports a cutoff it never made: %+v", h)
	}
	if IsRevoked("1", time.Now().Add(-time.Hour)) {
		t.Fatal("a fresh install refuses an existing session")
	}
}

// The migration happens once, and so does the notice. The other services
// starting afterwards, and every later restart, adopt an already-migrated file:
// they must not claim to have signed anyone out — while health still reports
// the floor that IS in force, which is what explains a logout after the fact.
//
// Mutation that must fail this test: report legacy_cutoff_applied from
// revoked_before (i.e. "a floor exists") instead of from what this process did.
func TestOnlyTheMigratingStartClaimsTheCutoff(t *testing.T) {
	path := use(t)
	notices := captureNotices(t)

	seedLegacy(t, path, `{"version":1,"revoked":{}}`)
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(*notices) != 1 {
		t.Fatalf("migrating start emitted %d notices, want 1", len(*notices))
	}
	first := CurrentHealth().RevokedBefore
	if first == "" {
		t.Fatal("no floor was set, so this test is not exercising the migration")
	}

	// Every later start of every service, against the same file.
	for i := 0; i < 3; i++ {
		SetPath(path)
		if err := Ensure(); err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
		if len(*notices) != 1 {
			t.Fatalf("restart %d announced the cutoff again (%d notices); an operator would read every "+
				"restart as a fresh mass logout", i, len(*notices))
		}
		h := CurrentHealth()
		if h.LegacyCutoffApplied {
			t.Fatalf("restart %d claims it applied the cutoff; it inherited it", i)
		}
		if h.LegacyCutoffAt != "" {
			t.Fatalf("restart %d reports a cutoff instant of its own: %+v", i, h)
		}
		if h.RevokedBefore != first {
			t.Fatalf("restart %d reports the floor as %q, was %q; the reason for the logout must stay legible",
				i, h.RevokedBefore, first)
		}
	}
}

// A revocation can be the operation that migrates the list (a Revoke that
// reaches a pre-contract file before any Provision does). That path signs
// everyone out too, so it owes the same notice.
//
// Mutation that must fail this test: drop the announcement in revokeAt while
// keeping the sanitizeLegacyList call there.
func TestARevocationThatMigratesTheListAnnouncesTheCutoff(t *testing.T) {
	path := use(t)
	notices := captureNotices(t)

	seedLegacy(t, path, `{"version":1,"revoked":{}}`)
	if err := Revoke("deleted"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if len(*notices) != 1 {
		t.Fatalf("a revocation migrated the list and emitted %d notices", len(*notices))
	}
	l, err := readFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if l.before.IsZero() {
		t.Fatal("no floor was applied, so this test is not exercising the migration")
	}
	if (*notices)[0].at.Unix() != l.before.Unix() {
		t.Fatalf("announced %v, the file carries %v", (*notices)[0].at, l.before)
	}
	if h := CurrentHealth(); !h.LegacyCutoffApplied {
		t.Fatalf("health does not report the cutoff this process applied: %+v", h)
	}
}

// A report must never be able to degrade the authority. The notice is written
// to the journal, and the journal can be slow (a spun-down disk, a wedged
// syslog reader); if it were emitted while holding the lock every IsRevoked
// consults, one slow write would stall every request in the process. Asserted
// STRUCTURALLY — the notice takes the lock itself — not by timing anything.
//
// Mutation that must fail this test: move the announceLegacyCutoff call inside
// the global.mu section in Provision.
func TestTheCutoffNoticeIsNotEmittedUnderTheRequestLock(t *testing.T) {
	path := use(t)

	free := false
	called := false
	prev := announceLegacyCutoff
	announceLegacyCutoff = func(string, time.Time, int) {
		called = true
		if global.mu.TryRLock() {
			free = true
			global.mu.RUnlock()
		}
	}
	t.Cleanup(func() { announceLegacyCutoff = prev })

	seedLegacy(t, path, `{"version":1,"revoked":{}}`)
	if err := Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !called {
		t.Fatal("no notice was emitted, so this test is not exercising the announcement")
	}
	if !free {
		t.Fatal("the cutoff notice is written while holding the lock every request consults; " +
			"a slow journal would stop every request in the process")
	}
}

// The notice has to reach the journal from a process whose logger may not exist
// yet — CasaNova-AppManagement provisions the authority before it calls
// LogInit — so it goes to stderr, which systemd captures for every unit. This
// asserts the real notice, not the test seam: the seam is what the other tests
// count, and a seam that pointed at a function writing nowhere would satisfy
// them all.
//
// Mutation that must fail this test: write the notice to a logger that is not
// initialized at that point (or to a discarded writer).
func TestTheCutoffNoticeReachesTheJournal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	prev := os.Stderr
	os.Stderr = w
	at := time.Date(2026, 8, 17, 10, 30, 0, 0, time.UTC)

	announceLegacyCutoff("/var/lib/casanova/token-revocations.json", at, 3)

	os.Stderr = prev
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	r.Close()

	notice := string(out)
	if notice == "" {
		t.Fatal("the box signed everyone out and wrote nothing to the journal")
	}
	// What an operator needs to answer "why was I logged out?": when, which
	// file, and that signing in again is the remedy rather than a symptom.
	for _, want := range []string{
		at.Format(time.RFC3339),
		"/var/lib/casanova/token-revocations.json",
		"sign in again",
	} {
		if !strings.Contains(notice, want) {
			t.Fatalf("the cutoff notice does not mention %q: %s", want, notice)
		}
	}
}
