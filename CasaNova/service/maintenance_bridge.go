package service

// Bridge for pkg/maintenance.
//
// The maintenance module has to reuse three things that already exist in this
// package and are unexported: the /proc/self/mountinfo parser that decides
// which mounts are unsafe to walk (design rule 18), the chunk-group detector
// the boot sweeper uses (rule 8 — "never rewrite a sweep that already
// exists"), and the durable upload-session registry (rule 7 — "in progress" is
// decided by the registry, not by age).
//
// Cloning any of them with slightly different thresholds is precisely the
// failure mode those rules exist to prevent, and pkg/maintenance cannot import
// route/v2 (where the FileUploadService instance lives) without an import
// cycle. So this file exports thin wrappers and NOTHING else: no logic, no
// second copy of a constant, no reachable way to delete anything. Every
// function here is a one-line call into the code that already ships.

import (
	"encoding/json"
	"os"
	"time"
)

// UnwalkableMounts returns the mount points a walker must not descend into —
// dead NFS/CIFS shares and unknown FUSE backends. Wraps unwalkableMounts()
// verbatim (temp_sweeper.go); the parser, the fstype tables and the octal
// unescaping are the sweeper's, not a second implementation.
func UnwalkableMounts() map[string]bool { return unwalkableMounts() }

// IsOrphanChunkGroup reports whether dir is an abandoned v1 upload chunk group
// that is safe to reclaim: our `<md5hex><nChunks>` name shape, holding nothing
// but numeric chunk files (and the plain subdirectories a folder upload
// mirrors), with EVERY entry older than cutoff. Wraps isChunkGroup, whose 22
// table-driven test cases each pin one guard.
func IsOrphanChunkGroup(dir string, cutoff time.Time) bool { return isChunkGroup(dir, cutoff) }

// IsChunkGroupName is the name half of the same test, exposed for callers that
// have already decided the entry is a directory.
func IsChunkGroupName(name string) bool { return isChunkGroupName(name) }

// IsUploadTmpPath recognises a v2 upload scratch file: absolute, ending in
// `.upload-<8 hex>.tmp`. Wraps isUploadTmpPath (upload_sessions.go).
func IsUploadTmpPath(p string) bool { return isUploadTmpPath(p) }

// UploadSessionMaxAge is how long a v2 upload session stays resumable. Exposed
// so the maintenance scanner uses the SAME cutoff as the reaper instead of
// inventing a more aggressive one.
func UploadSessionMaxAge() time.Duration { return uploadSessionMaxAge }

// UploadSessionsPath is where the durable registry lives.
func UploadSessionsPath() string { return uploadSessionsFile }

// RegisteredUploadTmpPaths returns the scratch files the registry still knows
// about, keyed by absolute path.
//
// Read-only by design: loadUploadSessions() REAPS as a side effect (it deletes
// the .tmp of a stale session), which is the right behaviour at service start
// and the wrong behaviour inside a scan the user has not confirmed yet. So
// this decodes the same file into the same type and touches nothing. A path
// that appears here is never offered for deletion, whatever its age — a
// session whose bitmap is complete is one rename away from landing, and the
// client may simply be a laptop that is closed.
// registryPath is the file to read; "" means the production registry. The
// parameter exists so the maintenance scanner can be pointed at a fixture
// without this package exporting a mutable global.
func RegisteredUploadTmpPaths(registryPath string) map[string]bool {
	if registryPath == "" {
		registryPath = uploadSessionsFile
	}
	out := map[string]bool{}
	raw, err := os.ReadFile(registryPath)
	if err != nil {
		return out
	}
	sessions := map[string]*uploadSession{}
	if err := json.Unmarshal(raw, &sessions); err != nil {
		// A corrupt registry means we cannot prove anything is finished. Say
		// so with an empty map plus the error, and let the caller decide; it
		// disables the affected shape rather than guessing.
		return out
	}
	for _, s := range sessions {
		if s == nil || s.TmpPath == "" {
			continue
		}
		out[s.TmpPath] = true
	}
	return out
}

// UploadSessionsReadable reports whether the registry could be read and
// parsed. A missing file is fine (no uploads ever happened); a corrupt one is
// not, and the scratch category refuses to offer v2 leftovers in that case.
func UploadSessionsReadable(registryPath string) bool {
	if registryPath == "" {
		registryPath = uploadSessionsFile
	}
	raw, err := os.ReadFile(registryPath)
	if err != nil {
		return os.IsNotExist(err)
	}
	sessions := map[string]*uploadSession{}
	return json.Unmarshal(raw, &sessions) == nil
}
