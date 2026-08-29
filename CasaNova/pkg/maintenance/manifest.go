package maintenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// Manifests (design rule 12).
//
// The manifest is written BEFORE anything is touched, in the exact order the
// executor will act — leaves first, container directory last. Three jobs, in
// order of how often they matter:
//
//  1. Support. "What did that button delete?" has an answer on disk, months
//     later, without anyone having to reproduce the run.
//  2. Interruption. A power cut mid-run leaves the plan on disk next to a
//     partially-deleted tree, so the state is explainable instead of a mystery.
//  3. Review. It is the artefact a human can read before trusting the feature.
//
// Small files carry a sha256 so a "that was my config" claim can be checked
// against a backup. Large ones do not: hashing a 326 MB tarball to prove we
// deleted the tarball we said we would buys nothing and costs a minute of I/O.

const manifestHashMaxBytes = 1 << 20

// ManifestEntry is one planned removal.
type ManifestEntry struct {
	Path    string    `json:"path"`
	Kind    string    `json:"kind"`
	Bytes   int64     `json:"bytes"`
	MTime   time.Time `json:"mtime"`
	ItemID  string    `json:"item_id"`
	Reason  string    `json:"reason"`
	SHA256  string    `json:"sha256,omitempty"`
	Removed bool      `json:"removed"`
	Error   string    `json:"error,omitempty"`
}

// Manifest is the whole record of one run.
type Manifest struct {
	ID             string          `json:"id"`
	RunID          string          `json:"run_id"`
	Category       string          `json:"category"`
	StartedAt      time.Time       `json:"started_at"`
	FinishedAt     time.Time       `json:"finished_at"`
	PlannedBytes   int64           `json:"planned_bytes"`
	RemovedBytes   int64           `json:"removed_bytes"`
	DiskBefore     DiskInfo        `json:"disk_before"`
	DiskAfter      DiskInfo        `json:"disk_after"`
	DiskDeltaBytes int64           `json:"disk_delta_bytes"`
	Version        string          `json:"casanova_version"`
	Entries        []ManifestEntry `json:"entries"`
}

// ManifestSummary is the listing row; reading every entry of every past run
// just to render a list would be silly.
type ManifestSummary struct {
	ID             string    `json:"id"`
	Category       string    `json:"category"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
	RemovedBytes   int64     `json:"removed_bytes"`
	DiskDeltaBytes int64     `json:"disk_delta_bytes"`
	Entries        int       `json:"entries"`
}

// newManifest builds the plan record. hashSmall is false in tests that do not
// care about digests.
func newManifest(id, runID, category, version string, items []Item, reasons map[string]string, hashSmall bool) *Manifest {
	m := &Manifest{
		ID:        id,
		RunID:     runID,
		Category:  category,
		StartedAt: time.Now(),
		Version:   version,
	}
	for _, it := range items {
		for _, t := range it.targets {
			e := ManifestEntry{
				Path:   t.Path,
				Kind:   t.Kind.String(),
				Bytes:  t.Bytes,
				MTime:  t.MTime,
				ItemID: it.ID,
				Reason: reasons[it.ID],
			}
			if hashSmall && t.Kind == KindFile && t.Bytes > 0 && t.Bytes <= manifestHashMaxBytes {
				e.SHA256 = hashFileNoFollow(t.Path)
			}
			m.PlannedBytes += t.Bytes
			m.Entries = append(m.Entries, e)
		}
	}
	return m
}

// write persists the manifest atomically and fsyncs it.
//
// The fsync is the point: a manifest that is still in the page cache when the
// box loses power is a manifest that does not exist, and the run it describes
// has already started deleting.
func (m *Manifest) write(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(dir, m.ID+".json")
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|unix.O_NOFOLLOW, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return err
	}
	// Sync the directory too, so the rename itself survives the same crash.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// hashFileNoFollow digests a small regular file. O_NOFOLLOW because the whole
// point is to record what WE are about to delete, not what a link points at.
// Best effort: a hash we could not take is an empty string, never a refusal.
func hashFileNoFollow(path string) string {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, manifestHashMaxBytes)); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// listManifests returns past runs, newest first.
func listManifests(dir string) ([]ManifestSummary, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ManifestSummary
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		m, err := readManifest(dir, e.Name()[:len(e.Name())-len(".json")])
		if err != nil {
			continue
		}
		out = append(out, ManifestSummary{
			ID:             m.ID,
			Category:       m.Category,
			StartedAt:      m.StartedAt,
			FinishedAt:     m.FinishedAt,
			RemovedBytes:   m.RemovedBytes,
			DiskDeltaBytes: m.DiskDeltaBytes,
			Entries:        len(m.Entries),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// readManifest loads one run by ID. The ID is validated as a bare filename so
// a manifest ID that reached us over the network cannot address another
// directory — the same reasoning as the opaque item IDs.
func readManifest(dir, id string) (*Manifest, error) {
	if id == "" || id != filepath.Base(id) || id == "." || id == ".." {
		return nil, fmt.Errorf("invalid manifest id")
	}
	raw, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
