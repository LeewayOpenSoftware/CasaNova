package service

import (
	"os"
	"path/filepath"
	"testing"
)

// mtimeOf reports the file's modification time in nanoseconds, or 0.
func sizeAndMtime(t *testing.T, p string) (int64, int64) {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		return 0, 0
	}
	return fi.Size(), fi.ModTime().UnixNano()
}

// THE DEFECT: the route watchdog re-registers all 51 prefixes every 30 s, and
// each registration rewrote routes.json with byte-identical content — a temp
// file, an fsync and a rename, measured at 2.63 ms, about 146,880 times a day.
//
// The assertion is a COUNT of writes (via mtime changes), not a duration.
func TestIdenticalRoutesAreNotRewritten(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "routes.json")
	targets := map[string]string{"/v1/apps": "http://127.0.0.1:1", "/v2/x": "http://127.0.0.1:2"}
	cidrs := map[string][]string{}

	if err := savePathTargetMapTo(p, targets, cidrs); err != nil {
		t.Fatal(err)
	}
	_, first := sizeAndMtime(t, p)
	if first == 0 {
		t.Fatal("the first write did not produce a file")
	}

	for i := 0; i < 20; i++ {
		if err := savePathTargetMapTo(p, targets, cidrs); err != nil {
			t.Fatal(err)
		}
	}
	_, after := sizeAndMtime(t, p)
	if after != first {
		t.Fatal("20 identical re-registrations rewrote routes.json. Each rewrite " +
			"is a temp file, an fsync and a rename (2.63 ms measured); the " +
			"watchdog does this ~146,880 times a day for content that never " +
			"changed.")
	}
}

// A REAL CHANGE MUST STILL BE PERSISTED. An optimisation that also skips real
// updates is not an optimisation, it is data loss.
func TestAChangedRouteIsWritten(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "routes.json")
	cidrs := map[string][]string{}

	if err := savePathTargetMapTo(p, map[string]string{"/v1/a": "http://127.0.0.1:1"}, cidrs); err != nil {
		t.Fatal(err)
	}
	sizeBefore, _ := sizeAndMtime(t, p)

	if err := savePathTargetMapTo(p, map[string]string{
		"/v1/a": "http://127.0.0.1:1", "/v1/b": "http://127.0.0.1:2",
	}, cidrs); err != nil {
		t.Fatal(err)
	}
	sizeAfter, _ := sizeAndMtime(t, p)
	if sizeAfter == sizeBefore {
		t.Fatal("adding a route did not change routes.json: the skip-if-unchanged " +
			"guard is swallowing real updates")
	}

	// And a changed TARGET for the same path, which keeps the same key set.
	if err := savePathTargetMapTo(p, map[string]string{
		"/v1/a": "http://127.0.0.1:9999", "/v1/b": "http://127.0.0.1:2",
	}, cidrs); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(body, "9999") {
		t.Fatal("a re-pointed target was not persisted — after a service restarts " +
			"on a new ephemeral port, routes.json would still name the old one")
	}
}

// THE FILE DISAPPEARING MUST BE NOTICED. A cache that trusts only its own
// memory would leave routes.json missing after a cleanup or a bad rollback.
func TestADeletedFileIsRecreated(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "routes.json")
	targets := map[string]string{"/v1/a": "http://127.0.0.1:1"}
	cidrs := map[string][]string{}

	if err := savePathTargetMapTo(p, targets, cidrs); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := savePathTargetMapTo(p, targets, cidrs); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("routes.json was not recreated after being deleted: %v — the "+
			"guard is comparing content without checking the file still exists", err)
	}
}

func contains(b []byte, s string) bool {
	return len(b) >= len(s) && (string(b) == s || indexOf(string(b), s) >= 0)
}
func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
