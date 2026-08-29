package service

// upload_sessions.go — durable registry for chunked uploads (v2 protocol).
// The in-RAM resume state (FileUploadService.uploadStatus) used to be the ONLY
// record of a partially-uploaded file: a service restart lost the bitmap and
// left the pre-allocated .tmp orphaned on disk forever (the temp sweeper only
// handles v1 .temp directories). This registry mirrors every session to
// uploadSessionsFile so (a) resume survives restarts and (b) stale .tmp files
// are reaped after uploadSessionMaxAge.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"
)

const uploadSessionMaxAge = 48 * time.Hour

// Var so tests can point the registry at a temp dir.
var uploadSessionsFile = "/var/lib/casanova/upload-sessions.json"

type uploadSession struct {
	TmpPath string `json:"tmpPath"`
	// Root is the validated upload root the session was created under. It is
	// the evidence a LATER process needs to re-prove containment before it
	// renames a registry-recorded temp file into place: without it, the only
	// thing rehydration could do was trust a path string written by whatever
	// build produced the file. Entries written before this field existed have
	// it empty and are deliberately never finalized (rehydrateDestination) —
	// that is the one-time migration that makes deploying the path hardening
	// safe on a box whose registry may already be poisoned.
	Root        string      `json:"root,omitempty"`
	TotalChunks int64       `json:"totalChunks"`
	ChunkSize   int64       `json:"chunkSize"`
	TotalSize   int64       `json:"totalSize"`
	Uploaded    chunkBitmap `json:"uploaded"`
	UpdatedAt   string      `json:"updatedAt"`
}

// chunkBitmap is the per-chunk "already stored" flag vector. It stays a []bool
// in RAM (every reader indexes it directly) but is PACKED on the wire.
//
// It used to be persisted as a JSON array of booleans through MarshalIndent,
// which cost ~13 bytes per chunk: a session declaring the permitted maximum of
// 1e6 chunks rewrote ~13 MB of JSON on every single chunk, whatever the chunk
// carried. A caller with PermFileWrite — reachable from the internet through
// the tunnel — could therefore make the daemon write megabytes per byte
// uploaded. Packed one bit per chunk plus base64, the same bitmap is ~167 KB,
// and a realistic session (a few thousand chunks) is under a kilobyte.
type chunkBitmap []bool

// packedBitmap is the compact wire form: n chunks, ceil(n/8) bytes of flags
// (encoding/json renders []byte as base64).
type packedBitmap struct {
	N    int    `json:"n"`
	Bits []byte `json:"bits"`
}

func (b chunkBitmap) MarshalJSON() ([]byte, error) {
	packed := make([]byte, (len(b)+7)/8)
	for i, up := range b {
		if up {
			packed[i/8] |= 1 << (uint(i) % 8)
		}
	}
	return json.Marshal(packedBitmap{N: len(b), Bits: packed})
}

// UnmarshalJSON reads both the packed form and the legacy JSON array. The
// legacy branch is load-bearing on upgrade: a box with uploads in flight has a
// registry full of `"uploaded":[true,false,...]`, and refusing it would make
// readUploadSessions treat the whole registry as unparseable — every resumable
// transfer lost and its pre-allocated .tmp orphaned.
func (b *chunkBitmap) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*b = nil
		return nil
	}
	if trimmed[0] == '[' {
		var legacy []bool
		if err := json.Unmarshal(trimmed, &legacy); err != nil {
			return err
		}
		*b = legacy
		return nil
	}
	var packed packedBitmap
	if err := json.Unmarshal(trimmed, &packed); err != nil {
		return err
	}
	// Bound the allocation: the registry is root-owned, but a corrupt or
	// hand-edited entry must not be able to ask for an arbitrary slice.
	if packed.N < 0 || int64(packed.N) > MaxTotalChunks {
		return fmt.Errorf("upload bitmap length %d out of range [0..%d]", packed.N, MaxTotalChunks)
	}
	if len(packed.Bits) < (packed.N+7)/8 {
		return fmt.Errorf("upload bitmap truncated: %d bytes for %d chunks", len(packed.Bits), packed.N)
	}
	out := make([]bool, packed.N)
	for i := range out {
		out[i] = packed.Bits[i/8]&(1<<(uint(i)%8)) != 0
	}
	*b = out
	return nil
}

// UploadSessionVerdict is what the reaper decided about one registry entry.
// It is part of the report so the maintenance screen can explain a session it
// is NOT reclaiming, which for this category is the common case.
type UploadSessionVerdict string

const (
	// UploadSessionActive — inside the retention window and still resumable.
	UploadSessionActive UploadSessionVerdict = "active"
	// UploadSessionPendingFinalize — every chunk arrived and the .tmp is
	// exactly the pre-allocated file the bitmap describes. One rename from
	// landing; never reaped, at any age.
	UploadSessionPendingFinalize UploadSessionVerdict = "pending-finalize"
	// UploadSessionStale — past uploadSessionMaxAge (or carrying an
	// unparseable timestamp) and incomplete: the .tmp goes.
	UploadSessionStale UploadSessionVerdict = "stale"
	// UploadSessionMissingTmp — the .tmp is gone, so only the registry entry
	// is dropped. Nothing is deleted from disk.
	UploadSessionMissingTmp UploadSessionVerdict = "missing-tmp"
	// UploadSessionSuspicious — the recorded path is not one of ours. The
	// entry is dropped and the file is left exactly where it is.
	UploadSessionSuspicious UploadSessionVerdict = "suspicious"
)

// UploadSessionItem is one v2 upload session as the reaper sees it.
type UploadSessionItem struct {
	Identifier string               `json:"identifier"`
	TmpPath    string               `json:"tmpPath"`
	Verdict    UploadSessionVerdict `json:"verdict"`
	Reason     string               `json:"reason"`
	// Bytes is what removing the .tmp actually frees: allocated blocks, not
	// the declared size. The .tmp is pre-allocated with Truncate, so a session
	// that uploaded 20 MB of a 40 GB file reports a 40 GB apparent size and
	// frees 20 MB. Promising the apparent number would be a lie.
	Bytes         int64 `json:"bytes"`
	ApparentBytes int64 `json:"apparentBytes"`
	// UploadedChunks/TotalChunks is the registry's own progress, i.e. the
	// evidence behind the verdict.
	UploadedChunks int64  `json:"uploadedChunks"`
	TotalChunks    int64  `json:"totalChunks"`
	UpdatedAt      string `json:"updatedAt"`
	Removed        bool   `json:"removed"`
	Err            string `json:"err,omitempty"`
}

// UploadSessionReport is the result of a reap pass over the registry.
type UploadSessionReport struct {
	DryRun bool `json:"dryRun"`
	// Live says the pass ran against the RUNNING upload service, under its
	// lock. A report with Live false was read from the registry file and can
	// be one chunk out of date; it is fine to show and never enough to delete.
	Live bool `json:"live"`
	// Sessions is how many entries the registry held when the pass started.
	Sessions int `json:"sessions"`
	// Reaped counts entries removed from the registry — on a dry run, entries
	// that WOULD be removed. Most of them free nothing: a dropped entry whose
	// .tmp is already gone, or one pointing at a file that is not ours.
	Reaped int `json:"reaped"`
	// Bytes is what deleting the stale .tmp files actually frees (allocated
	// blocks), and only for the entries that free anything.
	Bytes int64               `json:"bytes"`
	Items []UploadSessionItem `json:"items"`
}

// ErrUploadServiceUnavailable is returned when something asks to reap v2
// sessions before the upload service registered itself. Reaping from outside
// that service means deleting a .tmp whose session is still live in another
// goroutine's map — so we refuse instead of guessing.
var ErrUploadServiceUnavailable = errors.New("upload service is not available; refusing to reap upload sessions outside its lock")

// liveUploadService is the FileUploadService the v2 upload routes are using.
// The on-demand reaper needs THAT instance, not a second one: the registry
// file is only half the state — the other half is the in-RAM bitmap and the
// lock that keeps the two consistent.
var liveUploadService atomic.Pointer[FileUploadService]

// RegisterUploadService publishes the running upload service for the
// maintenance module. Call it once, right where the service is constructed.
func RegisterUploadService(s *FileUploadService) {
	liveUploadService.Store(s)
}

// loadUploadSessions restores the registry, reaps stale sessions (deleting
// their .tmp), and returns the survivors keyed by identifier. Called once from
// NewFileUploadService, before the service is reachable, which is why it takes
// no lock.
func loadUploadSessions() map[string]*uploadSession {
	sessions := readUploadSessions()
	// Corruption recovery lives here, on the service-start path only, and never
	// on a read. readUploadSessions is reached by the maintenance busy-check on
	// EVERY clean; if a parse failure deleted the registry there, a live
	// upload's .tmp would lose its record and 48h later be offered as an
	// orphan. Rename the bad file aside instead of destroying it: keeping the
	// evidence is worth a few KB, and the reaper never sees a half-parsed map.
	if len(sessions) == 0 {
		if raw, err := os.ReadFile(uploadSessionsFile); err == nil && len(raw) > 0 {
			var probe map[string]*uploadSession
			if json.Unmarshal(raw, &probe) != nil {
				aside := uploadSessionsFile + ".corrupt"
				if err := os.Rename(uploadSessionsFile, aside); err != nil {
					logger.Error("failed to set aside corrupt upload registry", zap.Error(err))
				} else {
					logger.Error("upload session registry was corrupt; moved aside", zap.String("path", aside))
				}
			}
		}
	}
	if rep := reapUploadSessions(sessions, false); rep.Reaped > 0 {
		persistUploadSessions(sessions)
	}
	return sessions
}

// readUploadSessions loads the registry from disk as a pure read. A file it
// cannot parse yields an EMPTY map and touches nothing: a half-parsed registry
// would point the reaper at paths nobody wrote, and this function is on the
// maintenance busy-check path, so it must never mutate. Corruption is recovered
// (by moving the file aside) only in loadUploadSessions, at service start.
func readUploadSessions() map[string]*uploadSession {
	sessions := map[string]*uploadSession{}
	raw, err := os.ReadFile(uploadSessionsFile)
	if err != nil {
		return sessions
	}
	if err := json.Unmarshal(raw, &sessions); err != nil {
		// Do NOT delete here. Return empty so no reaper acts on a guess; the
		// file is preserved for recovery at next service start.
		logger.Error("upload session registry unreadable on this read; treating as empty", zap.Error(err))
		return map[string]*uploadSession{}
	}
	return sessions
}

// ScanUploadSessions reports what an on-demand reap would do, without touching
// anything. It prefers the running upload service (so the answer reflects the
// bitmaps as they are right now) and falls back to reading the registry file,
// flagging the report as not-live.
func ScanUploadSessions() UploadSessionReport {
	if s := liveUploadService.Load(); s != nil {
		return s.SweepSessions(true)
	}
	sessions := readUploadSessions()
	return reapUploadSessions(sessions, true)
}

// ReapUploadSessions runs the reaper on demand against the running upload
// service. It is the ONLY exported way to delete a v2 upload .tmp: everything
// else can look, not touch.
func ReapUploadSessions() (UploadSessionReport, error) {
	s := liveUploadService.Load()
	if s == nil {
		return UploadSessionReport{}, ErrUploadServiceUnavailable
	}
	return s.SweepSessions(false), nil
}

// SweepSessions runs a reap pass under the service's own lock, which is what
// makes the decision safe: the registry, the in-RAM bitmap and the .tmp on
// disk are read as one consistent picture, and no chunk can land in between.
//
// A dry run only reads, so it takes the read lock and leaves concurrent
// uploads alone.
func (s *FileUploadService) SweepSessions(dryRun bool) UploadSessionReport {
	if dryRun {
		s.lock.RLock()
		defer s.lock.RUnlock()
		// Classify against a shallow copy: reapUploadSessions must not be able
		// to touch the live map even by accident on this path.
		snapshot := make(map[string]*uploadSession, len(s.sessions))
		for id, sess := range s.sessions {
			snapshot[id] = sess
		}
		rep := reapUploadSessions(snapshot, true)
		rep.Live = true
		return rep
	}

	s.lock.Lock()
	defer s.lock.Unlock()
	return s.sweepLocked()
}

// sweepLocked is the real pass, for callers that ALREADY hold s.lock: the
// hourly reaper takes the lock through SweepSessions, while the admission check
// in UploadFile is mid-transaction and cannot re-take it. One body so the two
// can never drift.
func (s *FileUploadService) sweepLocked() UploadSessionReport {
	rep := reapUploadSessions(s.sessions, false)
	rep.Live = true
	if rep.Reaped > 0 {
		// Drop the in-RAM resume state too, or TestChunk keeps answering for a
		// session whose .tmp we just deleted.
		for _, it := range rep.Items {
			if it.Verdict != UploadSessionActive && it.Verdict != UploadSessionPendingFinalize {
				s.uploadStatus.Delete(it.Identifier)
			}
		}
		persistUploadSessions(s.sessions)
	}
	return rep
}

// reapUploadSessions is the single decision table for v2 sessions, shared by
// the boot reaper and the on-demand one. On a real run it deletes the stale
// .tmp files and removes their entries from `sessions` in place; on a dry run
// it only classifies. Persisting the map is the caller's job, because only the
// caller knows which lock it is holding.
//
// The order of the rules is the whole point:
//
//  1. A path that is not ours is dropped from the registry and never deleted.
//  2. A .tmp that is already gone leaves nothing to delete.
//  3. A session whose bitmap is COMPLETE and whose .tmp is intact is kept no
//     matter how old it is. Age is not the criterion here — the registry is.
//     That file is one rename away from landing (NewFileUploadService
//     finalizes it on the next start), and the user has already watched their
//     client report 100%. Deleting it destroys a file they believe they have.
//  4. Only then does age decide.
func reapUploadSessions(sessions map[string]*uploadSession, dryRun bool) UploadSessionReport {
	rep := UploadSessionReport{DryRun: dryRun, Sessions: len(sessions)}

	for id, s := range sessions {
		item := UploadSessionItem{
			Identifier:  id,
			TmpPath:     s.TmpPath,
			TotalChunks: s.TotalChunks,
			UpdatedAt:   s.UpdatedAt,
		}
		for _, u := range s.Uploaded {
			if u {
				item.UploadedChunks++
			}
		}

		// (1) Defense in depth: only ever os.Remove paths that LOOK like our
		// per-session tmps (absolute, ".upload-XXXXXXXX.tmp" shape). The
		// registry is root-owned, but a corrupted/hand-edited entry must not
		// turn the reaper into an arbitrary-file deleter.
		if !sessionTmpAcceptableIn(s.Root, s.TmpPath) {
			item.Verdict = UploadSessionSuspicious
			item.Reason = "recorded path is not one of our upload temp files; entry dropped, file untouched"
			if !dryRun {
				logger.Error("upload session with suspicious tmp path dropped (not reaped)",
					zap.String("identifier", id), zap.String("tmp", s.TmpPath))
				delete(sessions, id)
			}
			rep.Items = append(rep.Items, item)
			rep.Reaped++
			continue
		}

		// Lstat, never Stat: a planted symlink must not be followed, and a
		// non-regular file is not something our upload code created.
		st, statErr := os.Lstat(s.TmpPath)
		if statErr == nil && st.Mode().IsRegular() {
			item.ApparentBytes = st.Size()
			item.Bytes = allocatedBytes(st)
		}

		// (2) A session whose .tmp vanished is unresumable either way.
		if os.IsNotExist(statErr) {
			item.Verdict = UploadSessionMissingTmp
			item.Reason = "temp file no longer exists; nothing to delete"
			if !dryRun {
				delete(sessions, id)
			}
			rep.Items = append(rep.Items, item)
			rep.Reaped++
			continue
		}

		// (3) Complete bitmap + intact tmp ⇒ hands off, regardless of age.
		if item.UploadedChunks == s.TotalChunks && s.TotalChunks > 0 && sessionTmpValid(s) {
			item.Verdict = UploadSessionPendingFinalize
			item.Reason = "all chunks uploaded; the file is one rename from landing and is finalized on the next service start"
			rep.Items = append(rep.Items, item)
			continue
		}

		// (4) Age. An unparseable timestamp counts as stale: we wrote it in
		// RFC3339 and cannot resume a session we cannot date.
		stale := true
		if t, err := time.Parse(time.RFC3339, s.UpdatedAt); err == nil {
			stale = time.Since(t) > uploadSessionMaxAge
		}
		if !stale {
			item.Verdict = UploadSessionActive
			item.Reason = "within the resume window"
			rep.Items = append(rep.Items, item)
			continue
		}

		item.Verdict = UploadSessionStale
		item.Reason = "abandoned for more than the resume window"
		if dryRun {
			rep.Items = append(rep.Items, item)
			rep.Reaped++
			rep.Bytes += item.Bytes
			continue
		}

		if statErr == nil && st.Mode().IsRegular() {
			// Revalidate immediately before the unlink. The stat behind the
			// decision is already in the past, and /DATA is writable over SMB:
			// os.SameFile compares (st_dev, st_ino), so a file swapped for a
			// symlink or replaced by a newer upload in that window fails here
			// and is left exactly where it is.
			if cur, cerr := os.Lstat(s.TmpPath); cerr != nil || !cur.Mode().IsRegular() || !os.SameFile(cur, st) {
				logger.Error("upload .tmp changed between the check and the removal; left alone",
					zap.String("identifier", id), zap.String("tmp", s.TmpPath))
				item.Err = "temp file changed on disk between the check and the removal"
				item.Bytes = 0
				rep.Items = append(rep.Items, item)
				continue
			}
			if err := os.Remove(s.TmpPath); err != nil {
				// Keep the registry entry so the next pass tries again rather
				// than orphaning a file nothing owns any more.
				logger.Error("reap stale upload .tmp", zap.String("path", s.TmpPath), zap.Error(err))
				item.Err = err.Error()
				item.Bytes = 0
				rep.Items = append(rep.Items, item)
				continue
			}
			item.Removed = true
			logger.Info("reaped stale upload session", zap.String("identifier", id), zap.String("tmp", s.TmpPath))
		} else {
			// Irregular or unreadable: drop the entry, leave the node.
			item.Bytes = 0
			item.Reason = "abandoned, but the recorded path is not a regular file; entry dropped, file untouched"
		}
		delete(sessions, id)
		rep.Items = append(rep.Items, item)
		rep.Reaped++
		rep.Bytes += item.Bytes
	}
	return rep
}

// allocatedBytes is what removing a file actually frees. The v2 upload .tmp is
// created at its final size with Truncate, so it is sparse until the last chunk
// lands and its apparent size is not what the filesystem is holding.
func allocatedBytes(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return info.Size()
}

// isUploadTmpPath recognizes the exact shape uploadTmpPath produces:
// absolute path ending in ".upload-<8 hex>.tmp".
//
// Deliberately narrower than the temp sweeper's isSingleShotUploadTmpName,
// which matches the v1 single-shot leftover `<dest>.upload-<UnixNano>.tmp`.
// The two shapes cannot overlap (8 characters vs. 16-20 digits) and that is on
// purpose: a v2 tmp belongs to this registry and is never decided by age, and
// the sweeper must not reach one behind the registry's back.
func isUploadTmpPath(p string) bool {
	if p == "" || !filepath.IsAbs(p) || !strings.HasSuffix(p, ".tmp") {
		return false
	}
	base := strings.TrimSuffix(p, ".tmp")
	i := strings.LastIndex(base, ".upload-")
	if i < 0 {
		return false
	}
	hex := base[i+len(".upload-"):]
	if len(hex) != 8 {
		return false
	}
	for _, r := range hex {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// sessionTmpAcceptable is the SINGLE predicate every code path must satisfy
// before it acts on a path that came out of the registry file. It is the shape
// check plus the same sanitiser a live request goes through.
//
// isUploadTmpPath on its own was never enough: "/etc/cron.d/x.upload-deadbeef
// .tmp" is a perfectly well-formed name, so the shape test happily passed a
// path that the vulnerable build could have recorded and that no live request
// would be allowed to produce today. Running sanitizeUploadPath here means the
// registry cannot re-introduce a destination the request path already rejects.
func sessionTmpAcceptable(p string) bool {
	return sessionTmpAcceptableIn("", p)
}

// sessionTmpAcceptableIn decides whether a registry-recorded temp path may be
// unlinked, given the upload root that session recorded.
//
// The shape check plus the blocklist is NOT containment, and the distinction
// matters: BlockedSystemPrefixes does not list /home, /tmp or /, so a planted
// entry naming /tmp/x/y.upload-deadbeef.tmp passes both and gets deleted. That
// residual was demonstrated, not theorised.
//
// Sessions this build creates record their Root, so for them the answer is
// real containment: the path must resolve inside the root the upload actually
// used. Entries with no Root come from a build that predates the field; they
// fall back to the shape check, which is what the reaper has always applied,
// because refusing them outright would strand real temp files on disk with
// nothing left able to clean them up.
func sessionTmpAcceptableIn(root, p string) bool {
	if !isUploadTmpPath(p) {
		return false
	}
	clean, err := sanitizeUploadPath(p)
	if err != nil {
		return false
	}
	if strings.TrimSpace(root) == "" {
		return true // legacy entry: shape and blocklist are all we have
	}
	cleanRoot, err := sanitizeUploadPath(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(cleanRoot, clean)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// removeSessionTmp deletes a temp file named by the REGISTRY, and only when
// that path is one of ours: inside the session's recorded upload root when it
// has one, and — for entries written before that field existed — shaped like
// ours and outside every blocked prefix.
//
// Three places unlinked sess.TmpPath. Exactly one of them (the reaper) checked
// the path first; the rehydrate path and the resume-mismatch path in UploadFile
// deleted whatever string the JSON contained. That made a registry entry — which
// a caller holding PermFileWrite could plant through the pre-hardening build,
// and this box answers from the internet — into an arbitrary-file delete. One
// gated helper at all three sites so the check cannot be forgotten again.
func removeSessionTmp(identifier, root, p string) {
	if !sessionTmpAcceptableIn(root, p) {
		logger.Error("refusing to delete a registry path that is not one of our upload temp files",
			zap.String("identifier", identifier), zap.String("tmp", p))
		return
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		logger.Error("remove upload temp file", zap.String("identifier", identifier),
			zap.String("tmp", p), zap.Error(err))
	}
}

// persistUploadSessions writes the registry atomically (tmp+rename). Callers
// hold FileUploadService.lock, so writes never interleave.
func persistUploadSessions(sessions map[string]*uploadSession) {
	// Marshal, never MarshalIndent: this runs on every chunk of every upload,
	// and the indentation alone doubled a bitmap that is already the bulk of
	// the document (see chunkBitmap). Nothing reads this file by eye.
	raw, err := json.Marshal(sessions)
	if err != nil {
		return
	}
	os.MkdirAll("/var/lib/casanova", 0o755)
	tmp := uploadSessionsFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		logger.Error("persist upload sessions", zap.Error(err))
		return
	}
	if err := os.Rename(tmp, uploadSessionsFile); err != nil {
		logger.Error("persist upload sessions", zap.Error(err))
	}
}
