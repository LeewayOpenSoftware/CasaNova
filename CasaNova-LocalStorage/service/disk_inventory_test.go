package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/model"
	"gotest.tools/v3/assert"
)

// InitCheck used to call file.WriteToPath(data, config.AppInfo.DBPath, fileName)
// directly and discard the returned error. A disk-full or permission failure on
// that write left the on-disk disk-added/removed snapshot stale forever with
// zero trace in the logs — the next InitCheck run kept comparing against
// outdated state and could silently re-fire or miss events.
//
// persistDiskInventorySnapshot is the write path pulled out of InitCheck so the
// failure is a value this test can observe. Forced with a blocker FILE sitting
// where a directory component of the destination needs to be, which fails
// os.MkdirAll with ENOTDIR regardless of whether the test runs as root — a
// bare permission-bit test would not reproduce under root.
func TestPersistDiskInventorySnapshotReturnsTheWriteFailure(t *testing.T) {
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "blocker")
	assert.NilError(t, os.WriteFile(blocker, []byte("not a directory"), 0o644))

	dbPath := filepath.Join(blocker, "nested")

	err := persistDiskInventorySnapshot(map[string]model.LSBLKModel{
		"serial-1": {Name: "sda", Path: "/dev/sda"},
	}, dbPath, "local-storage.json")

	assert.Assert(t, err != nil, "a write that cannot succeed must return an error a caller can log, not be swallowed")
}

func TestPersistDiskInventorySnapshotWritesOnSuccess(t *testing.T) {
	tmp := t.TempDir()

	err := persistDiskInventorySnapshot(map[string]model.LSBLKModel{
		"serial-1": {Name: "sda", Path: "/dev/sda"},
	}, tmp, "local-storage.json")

	assert.NilError(t, err)

	written, readErr := os.ReadFile(filepath.Join(tmp, "local-storage.json"))
	assert.NilError(t, readErr)
	assert.Assert(t, len(written) > 0, "expected a non-empty snapshot file")
}
