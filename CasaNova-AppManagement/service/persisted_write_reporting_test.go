package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-AppManagement/pkg/config"
)

// A WRITE THAT NOBODY CAN LEARN FAILED IS A WRITE THAT REPORTS SUCCESS.
//
// Two settings paths in this package changed state in memory, handed the
// durable write to somewhere that could only log, and returned nil to an HTTP
// handler that had already answered 200. The value was live in memory, so the
// UI re-read it and showed it applied; the next restart loaded the file that
// was never written and the setting was gone. That is the "my setting reverts
// by itself" report, and nothing in the product could see it — the API said
// success, the screen agreed, and only the daemon log knew.
//
// These tests drive the real functions against a path that cannot be written,
// and assert the two things that were missing: the caller is TOLD, and memory
// does not stay ahead of disk.

// unwritableEnvFile points the global-settings file at a path whose parent
// directory does not exist, so the create fails for a mundane, realistic
// reason (EACCES on /etc, a read-only rootfs, a full disk all land here too).
// Nothing real is touched: the path is under t.TempDir().
func unwritableEnvFile(t *testing.T) {
	t.Helper()
	prev := config.AppManagementGlobalEnvFilePath
	config.AppManagementGlobalEnvFilePath = filepath.Join(t.TempDir(), "no-such-dir", "env")
	t.Cleanup(func() { config.AppManagementGlobalEnvFilePath = prev })
}

func writableEnvFile(t *testing.T) string {
	t.Helper()
	prev := config.AppManagementGlobalEnvFilePath
	p := filepath.Join(t.TempDir(), "env")
	config.AppManagementGlobalEnvFilePath = p
	t.Cleanup(func() { config.AppManagementGlobalEnvFilePath = prev })
	return p
}

// resetGlobals clears the package-level settings map for one test and puts the
// previous contents back afterwards.
func resetGlobals(t *testing.T) {
	t.Helper()
	config.GlobalMu.Lock()
	prev := make(map[string]string, len(config.Global))
	for k, v := range config.Global {
		prev[k] = v
	}
	config.Global = map[string]string{}
	config.GlobalMu.Unlock()
	t.Cleanup(func() {
		config.GlobalMu.Lock()
		config.Global = prev
		config.GlobalMu.Unlock()
	})
}

func globalValue(key string) (string, bool) {
	config.GlobalMu.RLock()
	defer config.GlobalMu.RUnlock()
	v, ok := config.Global[key]
	return v, ok
}

// Mutation that must fail this test: hand config.SaveGlobal() back to a
// detached goroutine and return nil.
func TestAGlobalSettingThatCannotBeSavedIsReportedAsFailed(t *testing.T) {
	resetGlobals(t)
	unwritableEnvFile(t)

	a := &AppStoreManagement{}
	err := a.ChangeGlobal("OPENAI_API_KEY", "sk-not-a-real-key")

	if err == nil {
		t.Fatal("ChangeGlobal returned nil for a setting it could not write; the handler answers 200 and the user believes it was saved")
	}
	if !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("the error does not name the setting that was lost: %v", err)
	}
	// The value must NOT be in the error — it can be an API key or a password,
	// and this error is on its way to a log and an HTTP response.
	if strings.Contains(err.Error(), "sk-not-a-real-key") {
		t.Errorf("the error carries the setting's VALUE, which may be a secret: %v", err)
	}

	// And memory must not be left ahead of disk: that is the same lie, just
	// with an error message on top of it.
	if v, ok := globalValue("OPENAI_API_KEY"); ok {
		t.Errorf("the setting is live in memory (%q) but was never written; it will vanish at the next restart", v)
	}
}

// A write that succeeds really does reach the file, and really does return
// nil — otherwise the test above would pass on a function that refuses
// everything.
func TestAGlobalSettingThatSavesIsOnDisk(t *testing.T) {
	resetGlobals(t)
	p := writableEnvFile(t)

	a := &AppStoreManagement{}
	if err := a.ChangeGlobal("TZ", "Europe/Madrid"); err != nil {
		t.Fatalf("ChangeGlobal on a writable path: %v", err)
	}
	if v, ok := globalValue("TZ"); !ok || v != "Europe/Madrid" {
		t.Fatalf("in-memory value = (%q, %v), want Europe/Madrid", v, ok)
	}

	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("the settings file was not written at all: %v", err)
	}
	if !strings.Contains(string(body), "TZ=Europe/Madrid") {
		t.Errorf("the file does not contain the setting: %q", string(body))
	}
}

// Deleting has the same two halves, pointing the other way: a setting that is
// gone from memory and still on disk comes back at the next restart.
func TestAGlobalSettingThatCannotBeDeletedKeepsItsValue(t *testing.T) {
	resetGlobals(t)
	p := writableEnvFile(t)

	a := &AppStoreManagement{}
	if err := a.ChangeGlobal("TZ", "Europe/Madrid"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := os.ReadFile(p); err != nil {
		t.Fatalf("seed did not reach disk: %v", err)
	}

	// Now make the write fail and try to delete.
	unwritableEnvFile(t)
	err := a.DeleteGlobal("TZ")

	if err == nil {
		t.Fatal("DeleteGlobal returned nil for a deletion it could not write; the setting returns at the next restart with no warning")
	}
	if v, ok := globalValue("TZ"); !ok || v != "Europe/Madrid" {
		t.Errorf("the setting was removed from memory (%q, %v) although the deletion was never written", v, ok)
	}
}

// ---------------------------------------------------------------------------
// The store registry: save() had no error to return at all
// ---------------------------------------------------------------------------

// RegisterStore, UnregisterStore and UpdateStore each return an error to an
// HTTP handler, and each called a save() that returned nothing. A read-only or
// full disk produced a registered store that existed only in this process.
//
// Mutation that must fail this test: give save() back its `func (r
// *StoreRegistry) save()` signature and drop the propagation.
func TestRegisteringAStoreThatCannotBeWrittenFails(t *testing.T) {
	// Fail the REGISTRY WRITE, and nothing else.
	//
	// This test took two wrong turns worth recording, because both of them
	// passed for a reason that had nothing to do with the fix:
	//
	//  1. Pointing AppStorePath at a directory that does not exist. That fails
	//     nothing: file.WriteFileAtomic calls MkdirAll itself.
	//  2. Pointing it under a regular file so MkdirAll fails. That DID make
	//     RegisterStore return an error — from its own `MkdirAll(absPath)`
	//     three lines earlier, never reaching save() at all. The test passed
	//     with the fix reverted, which is how it was caught.
	//
	// So the failure is aimed exactly at the atomic write: the store directory
	// is creatable, and the temp file the write needs is blocked by a
	// DIRECTORY sitting on its name (EISDIR on open). Same class of refusal a
	// read-only filesystem or a full disk produces at that step.
	dir := t.TempDir()
	blockedTmp := filepath.Join(dir, "stores.json.casanova-tmp")
	if err := os.MkdirAll(blockedTmp, 0o755); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blockedTmp, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker content: %v", err)
	}
	prev := config.AppInfo.AppStorePath
	config.AppInfo.AppStorePath = dir
	t.Cleanup(func() { config.AppInfo.AppStorePath = prev })

	r := &StoreRegistry{Stores: defaultStores()}
	countBefore := len(r.Stores)

	err := r.RegisterStore(StoreEntry{
		ID:   "example",
		Name: "Example",
		Type: "local",
		Path: "example-store",
	})

	if err == nil {
		t.Fatal("RegisterStore returned nil for a registry it could not write; the store disappears at the next restart")
	}
	// Prove the error came from the registry write and not from the store
	// directory creation three lines above it — that is the wrong reason this
	// test passed for once already.
	if strings.Contains(err.Error(), "failed to create store directory") {
		t.Fatalf("the error is the directory creation, not the registry write: %v", err)
	}
	if !strings.Contains(err.Error(), "stores.json") {
		t.Errorf("err = %v; want the registry write failure", err)
	}
	if len(r.Stores) != countBefore {
		t.Errorf("the store is in memory (%d entries, was %d) but was never written", len(r.Stores), countBefore)
	}
}

// And the same call on a writable path really does register and really does
// persist — so the test above is not passing because RegisterStore refuses
// everything.
func TestRegisteringAStoreOnAWritablePathPersists(t *testing.T) {
	dir := t.TempDir()
	prev := config.AppInfo.AppStorePath
	config.AppInfo.AppStorePath = dir
	t.Cleanup(func() { config.AppInfo.AppStorePath = prev })

	r := &StoreRegistry{Stores: defaultStores()}
	countBefore := len(r.Stores)

	if err := r.RegisterStore(StoreEntry{
		ID:   "example",
		Name: "Example",
		Type: "local",
		Path: "example-store",
	}); err != nil {
		t.Fatalf("RegisterStore on a writable path: %v", err)
	}
	if len(r.Stores) != countBefore+1 {
		t.Fatalf("store count = %d, want %d", len(r.Stores), countBefore+1)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read store dir: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			body, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			if strings.Contains(string(body), `"example"`) {
				found = true
			}
		}
	}
	if !found {
		t.Error("the registry file does not mention the store that was just registered")
	}
}
