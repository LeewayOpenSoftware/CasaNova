package service

import (
	"os"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/storageauth"
)

// TestMain provisions the storage authority for this package's tests.
//
// sanitizeUploadPath now consults the shared ceiling, so an unprovisioned
// authority refuses every upload — correct fail-closed behaviour, and what the
// upload suite started reporting the moment the service was wired. The tests
// exercise chunk assembly, resume and cleanup, not authorization, and their
// fixtures live under the OS temporary directory.
//
// Declaring that tree as storage is a fixture, not a bypass: every rule still
// evaluates, and the escape tests keep passing because the paths they try to
// reach are still outside it.
func TestMain(m *testing.M) {
	stateDir, err := os.MkdirTemp("", "casanova-storageauth-state-*")
	if err != nil {
		panic("test setup: " + err.Error())
	}
	storageauth.SetStateDir(stateDir)
	if err := storageauth.Ensure(storageauth.Deps{
		DataRoots: []string{os.TempDir()},
	}); err != nil {
		panic("test setup: provision storage authority: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(stateDir)
	os.Exit(code)
}
