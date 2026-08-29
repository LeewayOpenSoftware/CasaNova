package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func scanAll(t *testing.T, f *fixture) *ScanResult {
	t.Helper()
	res, err := f.engine().Scan(context.Background(), nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return res
}

func TestScanCoversTheWholeCatalogue(t *testing.T) {
	f := newFixture(t)
	stageUpdatePackages(f)
	stageUpgradeBackups(f)
	stageRotatedLogs(f)
	stageJournals(f)
	stageAptCache(f)
	stageScratch(f)

	res := scanAll(t, f)
	if len(res.Findings) != len(catalogOrder) {
		t.Fatalf("scan returned %d findings, want %d", len(res.Findings), len(catalogOrder))
	}
	if res.Token == "" {
		t.Fatal("scan produced no token")
	}
	if res.ReclaimableBytes <= 0 {
		t.Fatal("a fixture full of reclaimable artefacts reported nothing")
	}
	if len(res.Notices) == 0 {
		t.Fatal("the read-only panel came back empty")
	}
	for _, key := range catalogOrder {
		if res.FindingFor(key) == nil {
			t.Errorf("no finding for %s", key)
		}
	}
}

// Scratch is the one category that walks the user's data area, and until its
// untouchable set (rule 1) exists it must be unreachable in a production build.
// This pins both halves of that: it never appears in a scan, and a client that
// asks to clean it is refused as if it did not exist — so a stale UI or a
// probe cannot trigger a root-level walk of /DATA.
func TestScratchDisabledByDefaultInProduction(t *testing.T) {
	f := newFixture(t)
	f.env.ScratchEnabled = false // what DefaultEnv ships
	stageUpdatePackages(f)
	stageScratch(f) // even with real leftovers staged, they must not surface

	res := scanAll(t, f)
	for _, fd := range res.Findings {
		if fd.Category == CatScratch {
			t.Fatal("scratch appeared in a production scan")
		}
	}
	if got := len(res.Findings); got != len(catalogOrder)-1 {
		t.Fatalf("scan returned %d findings, want %d (catalogue minus scratch)", got, len(catalogOrder)-1)
	}

	// Clean must refuse the disabled category outright, before planning.
	_, err := f.engine().Clean(context.Background(), res.Token, CatScratch, nil)
	if err != ErrUnknownCategory {
		t.Fatalf("Clean(scratch) = %v, want ErrUnknownCategory", err)
	}

	// And the guard must not even accept /DATA as a root when scratch is off.
	for _, r := range f.env.allowedRoots() {
		if r == filepath.Clean(f.env.ScratchRoots[0]) {
			t.Fatal("a data root is in the guard whitelist while scratch is disabled")
		}
	}
}

// The end-to-end path: scan, clean one category, and check that the files are
// gone, the manifest is on disk and the statfs delta was measured.
func TestCleanRemovesOnlyTheTickedItems(t *testing.T) {
	f := newFixture(t)
	stageAptCache(f)

	res := scanAll(t, f)
	fi := res.FindingFor(CatAptCache)
	if len(fi.Items) != 2 {
		t.Fatalf("expected 2 cached debs, got %d", len(fi.Items))
	}
	keep := fi.Items[1].Path

	run, err := f.engine().Clean(context.Background(), res.Token, CatAptCache, []string{fi.Items[0].ID})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if run.RemovedItems != 1 || run.RemovedEntries != 1 {
		t.Fatalf("unexpected run result: %+v", run)
	}
	if _, err := os.Lstat(fi.Items[0].Path); !os.IsNotExist(err) {
		t.Fatal("the ticked item was not removed")
	}
	if _, err := os.Lstat(keep); err != nil {
		t.Fatal("an item the user did NOT tick was removed")
	}
	if run.ManifestID == "" {
		t.Fatal("no manifest was recorded")
	}
	m, err := f.engine().Manifest(run.ManifestID)
	if err != nil {
		t.Fatalf("manifest unreadable: %v", err)
	}
	if len(m.Entries) != 1 || !m.Entries[0].Removed {
		t.Fatalf("manifest does not describe the run: %+v", m.Entries)
	}
	if m.Entries[0].SHA256 == "" {
		t.Error("no digest recorded for a small file")
	}
}

// Rule 12: the manifest is written BEFORE anything is touched. With the
// manifest directory unwritable, the run must abort having deleted nothing.
func TestCleanWritesTheManifestBeforeDeletingAnything(t *testing.T) {
	f := newFixture(t)
	stageAptCache(f)
	// Occupy the manifests path with a regular file so MkdirAll fails.
	writeFile(t, f.env.manifestDir(), "not a directory")

	res := scanAll(t, f)
	fi := res.FindingFor(CatAptCache)
	if _, err := f.engine().Clean(context.Background(), res.Token, CatAptCache, f.itemIDs(fi)); err == nil {
		t.Fatal("the run proceeded without a manifest")
	}
	for _, it := range fi.Items {
		if _, err := os.Lstat(it.Path); err != nil {
			t.Fatalf("%s was deleted even though the manifest could not be written", it.Path)
		}
	}
}

// A whole tree comes out leaves-first, and the container directory goes last.
func TestCleanRemovesATreeLeavesFirst(t *testing.T) {
	f := newFixture(t)
	_, superseded := stageUpgradeBackups(f)

	res := scanAll(t, f)
	fi := res.FindingFor(CatUpgradeBackups)
	run, err := f.engine().Clean(context.Background(), res.Token, CatUpgradeBackups, f.itemIDs(fi))
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if len(run.Failed) != 0 || len(run.Skipped) != 0 {
		t.Fatalf("tree removal reported problems: %+v %+v", run.Failed, run.Skipped)
	}
	if _, err := os.Lstat(superseded); !os.IsNotExist(err) {
		t.Fatal("the superseded backup directory survived")
	}

	m, err := f.engine().Manifest(run.ManifestID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Entries[len(m.Entries)-1].Path != superseded {
		t.Fatalf("the container directory is not last in the manifest: %s",
			m.Entries[len(m.Entries)-1].Path)
	}
}

// The scan token ties a plan to the scan that produced it.
func TestCleanRejectsAStaleOrUnknownToken(t *testing.T) {
	f := newFixture(t)
	stageAptCache(f)
	res := scanAll(t, f)
	fi := res.FindingFor(CatAptCache)

	if _, err := f.engine().Clean(context.Background(), "deadbeef", CatAptCache, f.itemIDs(fi)); !errors.Is(err, ErrStaleScan) {
		t.Fatalf("a wrong token was accepted: %v", err)
	}

	// And an expired one.
	f.env.Now = func() time.Time { return time.Now().Add(scanTTL + time.Minute) }
	if _, err := f.engine().Clean(context.Background(), res.Token, CatAptCache, f.itemIDs(fi)); !errors.Is(err, ErrStaleScan) {
		t.Fatalf("an expired token was accepted: %v", err)
	}
}

func TestCleanRejectsUnknownCategoryAndItems(t *testing.T) {
	f := newFixture(t)
	stageAptCache(f)
	res := scanAll(t, f)

	if _, err := f.engine().Clean(context.Background(), res.Token, "docker-system-prune", nil); !errors.Is(err, ErrUnknownCategory) {
		t.Fatalf("an unknown category was accepted: %v", err)
	}
	if _, err := f.engine().Clean(context.Background(), res.Token, CatAptCache, []string{"0000000000000000"}); !errors.Is(err, ErrUnknownItem) {
		t.Fatalf("an unknown item id was accepted: %v", err)
	}
}

// A blocked category cannot be executed even if the client asks nicely.
func TestCleanRefusesABlockedCategory(t *testing.T) {
	f := newFixture(t)
	stageUpdatePackages(f)
	f.write("var/lib/casanova/updates/casanova-19.0.0.dev1.0101270000-x86_64.tar.gz", "staged", 0)

	res := scanAll(t, f)
	_, err := f.engine().Clean(context.Background(), res.Token, CatUpdatePackages, []string{"whatever"})
	var perr *PreflightError
	if !errors.As(err, &perr) || perr.Code != BlockUpdateStaged {
		t.Fatalf("a blocked category was executed: %v", err)
	}
}

// Rule 11: one run at a time, enforced by a lock file so a second PROCESS is
// refused too, not just a second handler.
func TestRunLockIsExclusive(t *testing.T) {
	f := newFixture(t)
	first, err := f.engine().acquireRunLock()
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer first()

	if _, err := f.engine().acquireRunLock(); err == nil {
		t.Fatal("a second run acquired the lock while the first held it")
	}
}

// Rule 11 again: the run refuses to start while an update is being applied,
// whatever category was picked.
func TestCleanRefusesWhileTheSystemIsBusy(t *testing.T) {
	f := newFixture(t)
	stageAptCache(f)
	res := scanAll(t, f)
	fi := res.FindingFor(CatAptCache)

	f.write("run/casanova/update-status.json", `{"stage":"applying"}`, 0)
	_, err := f.engine().Clean(context.Background(), res.Token, CatAptCache, f.itemIDs(fi))
	var perr *PreflightError
	if !errors.As(err, &perr) || perr.Code != BlockUpdateRunning {
		t.Fatalf("a run started while an update was applying: %v", err)
	}
	for _, it := range fi.Items {
		if _, err := os.Lstat(it.Path); err != nil {
			t.Fatalf("%s was deleted despite the refusal", it.Path)
		}
	}
}

// The copy that leaves the process carries no removal plan and is capped.
func TestForAPITruncatesAndDropsTheInternalPlan(t *testing.T) {
	f := newFixture(t)
	stageAptCache(f)
	res := scanAll(t, f)

	api := res.ForAPI(1)
	fi := api.FindingFor(CatAptCache)
	if len(fi.Items) != 1 || !fi.Truncated {
		t.Fatalf("items were not truncated: %d items, truncated=%v", len(fi.Items), fi.Truncated)
	}
	if fi.Items[0].targets != nil {
		t.Fatal("the internal removal plan was exposed on the API copy")
	}
	// The stored plan is untouched.
	if len(res.FindingFor(CatAptCache).Items[0].targets) == 0 {
		t.Fatal("ForAPI mutated the stored scan")
	}
}

func TestCategoriesMetadataMatchesTheCatalogue(t *testing.T) {
	infos := Categories()
	if len(infos) != len(catalogOrder) {
		t.Fatalf("Categories() returned %d rows, want %d", len(infos), len(catalogOrder))
	}
	for _, info := range infos {
		if Lookup(info.Key) == nil {
			t.Errorf("Lookup(%q) = nil", info.Key)
		}
		if !info.RequiresStrictJWT {
			t.Errorf("%s does not require a strict JWT, but it deletes files", info.Key)
		}
		if info.Warn == "" {
			t.Errorf("%s has no consequence sentence for its confirmation dialog", info.Key)
		}
	}
	if Lookup("docker-system-prune") != nil {
		t.Fatal("a category that must never exist resolved")
	}
}

func TestManifestListingIsNewestFirst(t *testing.T) {
	f := newFixture(t)
	stageAptCache(f)
	res := scanAll(t, f)
	fi := res.FindingFor(CatAptCache)
	if _, err := f.engine().Clean(context.Background(), res.Token, CatAptCache, []string{fi.Items[0].ID}); err != nil {
		t.Fatal(err)
	}
	list, err := f.engine().Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected one manifest, got %d", len(list))
	}
	if list[0].Category != CatAptCache {
		t.Fatalf("manifest summary lost its category: %+v", list[0])
	}
}

// A manifest id that came over the network cannot address another directory.
func TestManifestIDCannotTraverse(t *testing.T) {
	f := newFixture(t)
	for _, bad := range []string{"../../etc/passwd", "..", "", "sub/dir"} {
		if _, err := f.engine().Manifest(bad); err == nil {
			t.Errorf("Manifest(%q) was accepted", bad)
		}
	}
}

func TestNoticesCoverTheDeferredCategories(t *testing.T) {
	keys := map[string]bool{}
	for _, n := range ReportOnlyNotices() {
		keys[n.Key] = true
		if n.Why == "" {
			t.Errorf("notice %q has no explanation", n.Key)
		}
	}
	for _, want := range []string{"docker-dangling-images", "container-log-rotation", "trash"} {
		if !keys[want] {
			t.Errorf("the read-only panel does not mention %q", want)
		}
	}
}

func TestProbePathFallsBackToRoot(t *testing.T) {
	if got := probePath(nil); got != "/" {
		t.Fatalf("probePath(nil) = %q, want /", got)
	}
	items := []Item{{targets: []target{{Path: filepath.Join("/var/log", "syslog.1")}}}}
	if got := probePath(items); got != "/var/log" {
		t.Fatalf("probePath = %q, want /var/log", got)
	}
}
