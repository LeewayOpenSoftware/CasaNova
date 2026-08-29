package maintenance

import (
	"path/filepath"
	"testing"
	"time"
)

// ─── 3.6 casanova-scratch ───────────────────────────────────────────────────

const chunkGroupName = "0123456789abcdef0123456789abcdef12"

func stageScratch(f *fixture) {
	// (1) v1 chunked-upload scratch: a well-formed, fully stale chunk group.
	f.write("DATA/share/.temp/"+chunkGroupName+"/0", "chunk", 5*time.Hour)
	f.write("DATA/share/.temp/"+chunkGroupName+"/1", "chunk", 5*time.Hour)
	f.age(filepath.Join(f.root, "DATA/share/.temp/"+chunkGroupName), 5*time.Hour)

	// A loose file directly under .temp is not something we wrote.
	f.write("DATA/share/.temp/somebody-elses-file", "not ours", 5*time.Hour)

	// (2) interrupted atomic copy.
	f.write("DATA/share/photo.jpg.casanova-tmp", "half copied", 5*time.Hour)
	f.write("DATA/share/fresh.jpg.casanova-tmp", "being copied right now", 0)

	// (3) v2 upload sessions: one still registered, one forgotten.
	f.write("DATA/share/movie.mkv.upload-0123abcd.tmp", "resumable", 100*time.Hour)
	f.write("DATA/share/other.mkv.upload-89abcdef.tmp", "forgotten", 100*time.Hour)

	// (4) v1 single-shot upload — the leak nothing cleans up today.
	f.write("DATA/share/legacy.mkv.upload-1712345678901234567.tmp", "abandoned", 48*time.Hour)

	f.write("var/lib/casanova/upload-sessions.json",
		`{"sess-1":{"tmpPath":"`+filepath.Join(f.root, "DATA/share/movie.mkv.upload-0123abcd.tmp")+
			`","totalChunks":2,"chunkSize":1,"totalSize":2,"uploaded":[true,false],"updatedAt":"2020-01-01T00:00:00Z"}}`, 0)
}

func TestScratchRecognisesAllFourShapes(t *testing.T) {
	f := newFixture(t)
	stageScratch(f)

	fi := f.scan(CatScratch)
	if fi.Blocked {
		t.Fatalf("category blocked: %s", fi.BlockedReason)
	}
	labels := f.itemLabels(fi)

	for _, want := range []string{
		chunkGroupName,                              // (1)
		"photo.jpg.casanova-tmp",                    // (2)
		"other.mkv.upload-89abcdef.tmp",             // (3), no longer registered
		"legacy.mkv.upload-1712345678901234567.tmp", // (4), the unclaimed leak
	} {
		if !contains(labels, want) {
			t.Errorf("%s was not offered; got %v", want, labels)
		}
	}
}

// Rule 7: "in progress" is decided by the REGISTRY, not by age. A session with
// a complete bitmap is one rename away from landing — deleting its .tmp
// destroys a file the user already watched reach 100%.
func TestScratchNeverTouchesARegisteredUploadSession(t *testing.T) {
	f := newFixture(t)
	stageScratch(f)

	fi := f.scan(CatScratch)
	if contains(f.itemLabels(fi), "movie.mkv.upload-0123abcd.tmp") {
		t.Fatal("a registered upload session's scratch file was offered")
	}
}

// The four house guards: name shape, content shape, per-entry age, and
// non-regular entries.
func TestScratchLeavesForeignAndFreshEntriesAlone(t *testing.T) {
	f := newFixture(t)
	stageScratch(f)

	fi := f.scan(CatScratch)
	labels := f.itemLabels(fi)
	for _, mustKeep := range []string{
		"somebody-elses-file",    // a loose file under .temp is not ours
		"fresh.jpg.casanova-tmp", // still being written
		".temp",                  // the shell itself is left to the boot sweeper
	} {
		if contains(labels, mustKeep) {
			t.Errorf("%s was offered for deletion", mustKeep)
		}
	}
}

// A chunk group holding anything that is not a numeric chunk file is somebody
// else's data: the WHOLE group is left alone.
func TestScratchRefusesAChunkGroupWithForeignContent(t *testing.T) {
	f := newFixture(t)
	stageScratch(f)
	f.write("DATA/share/.temp/"+chunkGroupName+"/notes.txt", "the user's file", 5*time.Hour)
	f.age(filepath.Join(f.root, "DATA/share/.temp/"+chunkGroupName), 5*time.Hour)

	fi := f.scan(CatScratch)
	if contains(f.itemLabels(fi), chunkGroupName) {
		t.Fatal("a chunk group containing a non-chunk file was offered")
	}
}

// An unreadable registry means we cannot tell a live v2 session from a
// leftover, so that shape is withheld while the other three keep working.
func TestScratchWithholdsV2LeftoversWhenTheRegistryIsCorrupt(t *testing.T) {
	f := newFixture(t)
	stageScratch(f)
	f.write("var/lib/casanova/upload-sessions.json", "{not json", 0)

	fi := f.scan(CatScratch)
	labels := f.itemLabels(fi)
	if contains(labels, "other.mkv.upload-89abcdef.tmp") {
		t.Fatal("a v2 leftover was offered with an unreadable registry")
	}
	if !contains(labels, "photo.jpg.casanova-tmp") {
		t.Fatal("a corrupt registry suppressed the shapes that do not depend on it")
	}
}

// The run-time re-check: a session registered after the scan protects its file.
func TestScratchPrecheckRefusesNewlyRegisteredSessions(t *testing.T) {
	f := newFixture(t)
	stageScratch(f)
	fi := f.scan(CatScratch)
	items, err := defaultPlan(f.engine(), fi, f.itemIDs(fi))
	if err != nil {
		t.Fatal(err)
	}

	f.write("var/lib/casanova/upload-sessions.json",
		`{"sess-2":{"tmpPath":"`+filepath.Join(f.root, "DATA/share/other.mkv.upload-89abcdef.tmp")+
			`","totalChunks":1,"chunkSize":1,"totalSize":1,"uploaded":[true],"updatedAt":"2020-01-01T00:00:00Z"}}`, 0)

	if perr := precheckScratch(f.engine(), items); perr == nil {
		t.Fatal("the precheck accepted deleting a newly registered session's file")
	}
}

// Depth is a real guard here: a leftover sitting directly in a storage root is
// a second-level component, which rule 3 forbids removing.
func TestScratchSkipsLeftoversTooCloseToTheRoot(t *testing.T) {
	// A guard on the production root, where the depth rule actually bites.
	g, err := NewGuard([]string{"/DATA"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Check("/DATA/orphan.casanova-tmp"); err == nil {
		t.Fatal("a second-level leftover passed the depth guard")
	}
}
