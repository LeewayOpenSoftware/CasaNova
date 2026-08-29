package service

import (
	"os"
	"path/filepath"
	"testing"
)

// The residual the round-2 verifier demonstrated: the deletion gate was shape
// plus blocklist, which is not containment. BlockedSystemPrefixes does not list
// /home, /tmp or /, so a planted registry entry naming a correctly-shaped temp
// file in one of those trees was deleted by the "fixed" build.
//
// Sessions this build writes record their Root, so the answer for them is real
// containment. Mutation that must fail this test: have sessionTmpAcceptableIn
// ignore root and fall back to the shape check for every entry.
func TestSessionTmpOutsideItsRootIsRefused(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	inside := filepath.Join(root, "sub", "file.upload-deadbeef.tmp")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if !sessionTmpAcceptableIn(root, inside) {
		t.Fatal("a temp file inside its own upload root was refused; real uploads could never be cleaned up")
	}

	planted := filepath.Join(outside, "victim.upload-deadbeef.tmp")
	if err := os.WriteFile(planted, []byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sessionTmpAcceptableIn(root, planted) {
		t.Fatal("a correctly-shaped path OUTSIDE the session's root was accepted for deletion")
	}

	// And the gate must actually stop the unlink, not merely report false.
	removeSessionTmp("planted", root, planted)
	if _, err := os.Stat(planted); err != nil {
		t.Fatalf("the file outside the upload root was deleted anyway: %v", err)
	}
}

// Traversal dressed up as containment must not pass either.
func TestSessionTmpTraversalOutOfRootIsRefused(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "uploads")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(root, "..", "sibling.upload-deadbeef.tmp")
	if sessionTmpAcceptableIn(root, escape) {
		t.Fatal("a path traversing out of the upload root was accepted for deletion")
	}
}

// Entries written before Root existed must still be cleanable, or real temp
// files strand on disk forever with nothing able to reap them.
func TestLegacySessionWithoutRootStillFallsBackToShapeCheck(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "old.upload-deadbeef.tmp")
	if err := os.WriteFile(legacy, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !sessionTmpAcceptableIn("", legacy) {
		t.Fatal("a legacy entry became uncleanable; its temp file would strand forever")
	}
	if sessionTmpAcceptableIn("", filepath.Join(dir, "not-ours.txt")) {
		t.Fatal("the legacy fallback accepted a path that is not shaped like ours at all")
	}
}
