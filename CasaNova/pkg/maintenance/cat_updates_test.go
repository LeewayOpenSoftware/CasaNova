package maintenance

import (
	"path/filepath"
	"testing"
	"time"
)

// ─── 3.1 casanova-update-packages ───────────────────────────────────────────

func stageUpdatePackages(f *fixture) {
	f.write("var/lib/casanova/updates/casanova-"+installedTestVersion+"-x86_64.tar.gz", "installed", 0)
	f.write("var/lib/casanova/updates/changelog.json", "[]", 0)
	f.write("var/lib/casanova/updates_archive/casanova-5.0.0.dev10.1204260108-x86_64.tar.gz", "old", 0)
	f.write("var/lib/casanova/updates_archive/casanova-5.0.0.dev12.1204260134-x86_64.tar.gz", "old", 0)
	f.write("var/lib/casanova/updates_archive/casanova-5.0.0.dev18.1306261034-x86_64.tar.gz", "n-1", 0)
}

// Guards 1-3 together: only strictly-older packages that are neither the
// installed one, nor the running one, nor the one-step rollback.
func TestUpdatePackagesKeepsInstalledRunningAndN1(t *testing.T) {
	f := newFixture(t)
	stageUpdatePackages(f)

	fi := f.scan(CatUpdatePackages)
	if fi.Blocked {
		t.Fatalf("category blocked unexpectedly: %s", fi.BlockedReason)
	}
	labels := f.itemLabels(fi)
	for _, want := range []string{
		"casanova-5.0.0.dev10.1204260108-x86_64.tar.gz",
		"casanova-5.0.0.dev12.1204260134-x86_64.tar.gz",
	} {
		if !contains(labels, want) {
			t.Errorf("superseded package %s not offered; got %v", want, labels)
		}
	}
	for _, mustKeep := range []string{
		"casanova-" + installedTestVersion + "-x86_64.tar.gz", // installed AND running
		"casanova-5.0.0.dev18.1306261034-x86_64.tar.gz",       // N-1 rollback
		"changelog.json", // never a candidate
	} {
		if contains(labels, mustKeep) {
			t.Errorf("%s was offered for deletion", mustKeep)
		}
	}
	if fi.Preselect {
		t.Error("update packages must never be preselected")
	}
}

// Guard 4: a package newer than the installed version means a staged update
// that has not been applied. The whole category closes.
func TestUpdatePackagesBlockedByStagedPackage(t *testing.T) {
	f := newFixture(t)
	stageUpdatePackages(f)
	f.write("var/lib/casanova/updates/casanova-19.0.0.dev1.0101270000-x86_64.tar.gz", "staged", 0)

	fi := f.scan(CatUpdatePackages)
	if !fi.Blocked || fi.BlockedCode != BlockUpdateStaged {
		t.Fatalf("staged update did not block the category: blocked=%v code=%q",
			fi.Blocked, fi.BlockedCode)
	}
	if len(fi.Items) != 0 {
		t.Errorf("a blocked category still offered %d item(s)", len(fi.Items))
	}
}

// Guard 4, other half: an update being applied right now.
func TestUpdatePackagesBlockedWhileUpdateRunning(t *testing.T) {
	f := newFixture(t)
	stageUpdatePackages(f)
	f.write("run/casanova/update-status.json", `{"stage":"applying"}`, 0)

	fi := f.scan(CatUpdatePackages)
	if !fi.Blocked || fi.BlockedCode != BlockUpdateRunning {
		t.Fatalf("an in-flight update did not block the category: %+v", fi)
	}
}

// Without a manifest we cannot prove ANY package is superseded, so nothing is
// offered — the failure mode is "reclaim nothing", never "guess".
func TestUpdatePackagesBlockedWithoutManifest(t *testing.T) {
	f := newFixture(t)
	stageUpdatePackages(f)
	if err := removeFile(filepath.Join(f.root, "var/lib/casanova/manifest.json")); err != nil {
		t.Fatal(err)
	}

	fi := f.scan(CatUpdatePackages)
	if !fi.Blocked || fi.BlockedCode != BlockInconsistent {
		t.Fatalf("a missing manifest did not block the category: %+v", fi)
	}
}

// The `.part` stubs a cancelled package upload leaves behind: offered once
// they have been untouched for an hour, never while they are being written.
func TestUpdatePackagesOffersOnlyStalePartStubs(t *testing.T) {
	f := newFixture(t)
	stageUpdatePackages(f)
	f.write("var/lib/casanova/updates/.casanova-x.tar.gz.part", "abandoned", 3*time.Hour)
	f.write("var/lib/casanova/updates/.casanova-y.tar.gz.part", "in flight", 0)

	fi := f.scan(CatUpdatePackages)
	labels := f.itemLabels(fi)
	if !contains(labels, ".casanova-x.tar.gz.part") {
		t.Errorf("a stale .part stub was not offered: %v", labels)
	}
	if contains(labels, ".casanova-y.tar.gz.part") {
		t.Error("a .part stub still being written was offered")
	}
}

// The run-time re-check: a staged update that appeared after the scan stops
// the run even though the plan was valid when it was built.
func TestUpdatePackagesPrecheckCatchesLateStaging(t *testing.T) {
	f := newFixture(t)
	stageUpdatePackages(f)
	fi := f.scan(CatUpdatePackages)
	items, err := defaultPlan(f.engine(), fi, f.itemIDs(fi))
	if err != nil {
		t.Fatal(err)
	}

	f.write("run/casanova/update-status.json", `{"stage":"applying"}`, 0)
	if perr := precheckUpdatePackages(f.engine(), items); perr == nil {
		t.Fatal("the precheck accepted a run while an update was being applied")
	}
}

// ─── 3.2 casanova-upgrade-backups ───────────────────────────────────────────

func stageUpgradeBackups(f *fixture) (current, superseded string) {
	current = filepath.Join(f.root, "var/lib/casanova/backups/upgrade-20260808-163700")
	superseded = filepath.Join(f.root, "var/lib/casanova/backups/upgrade-20260101-000000")

	f.write("var/lib/casanova/backups/upgrade-20260101-000000/bin/casanova", "old binary", 48*time.Hour)
	f.write("var/lib/casanova/backups/upgrade-20260101-000000/update-report.json",
		`{"backup_location":"`+superseded+`"}`, 48*time.Hour)

	f.write("var/lib/casanova/backups/upgrade-20260808-163700/bin/casanova", "current binary", 0)
	f.write("var/lib/casanova/backups/upgrade-20260808-163700/update-report.json",
		`{"backup_location":"`+current+`"}`, 0)

	f.write("var/lib/casanova/backups/.last-upgrade-backup", current+"\n", 0)
	return current, superseded
}

func TestUpgradeBackupsNeverOffersTheCurrentRollback(t *testing.T) {
	f := newFixture(t)
	current, superseded := stageUpgradeBackups(f)

	fi := f.scan(CatUpgradeBackups)
	if fi.Blocked {
		t.Fatalf("category blocked: %s", fi.BlockedReason)
	}
	if len(fi.Items) != 1 || fi.Items[0].Path != superseded {
		t.Fatalf("expected exactly the superseded backup, got %+v", fi.Items)
	}
	for _, it := range fi.Items {
		if it.Path == current {
			t.Fatal("the current rollback point was offered")
		}
	}
	if fi.Preselect {
		t.Error("rollback points must not be preselected")
	}
}

// Two sources of truth that disagree is a broken invariant, not a cleanup
// opportunity.
func TestUpgradeBackupsBlockedWhenPointerAndReportDisagree(t *testing.T) {
	f := newFixture(t)
	_, superseded := stageUpgradeBackups(f)
	f.write("var/lib/casanova/backups/.last-upgrade-backup", superseded+"\n", 0)

	fi := f.scan(CatUpgradeBackups)
	if !fi.Blocked || fi.BlockedCode != BlockInconsistent {
		t.Fatalf("disagreeing rollback pointers did not block the category: %+v", fi)
	}
}

// "Keep the last one", never "keep N": with no way to identify the current
// backup we refuse rather than delete the only one there is.
func TestUpgradeBackupsBlockedWithoutAnyPointer(t *testing.T) {
	f := newFixture(t)
	f.write("var/lib/casanova/backups/upgrade-20260101-000000/bin/casanova", "only backup", 48*time.Hour)

	fi := f.scan(CatUpgradeBackups)
	if !fi.Blocked {
		t.Fatalf("with no pointer and no report the category must block, got %+v", fi)
	}
	if len(fi.Items) != 0 {
		t.Fatal("the only existing backup was offered for deletion")
	}
}

// The run-time re-check: the pointer moved onto a planned directory between
// the scan and the click.
func TestUpgradeBackupsPrecheckRefusesTheCurrentRollback(t *testing.T) {
	f := newFixture(t)
	_, superseded := stageUpgradeBackups(f)
	fi := f.scan(CatUpgradeBackups)
	items, err := defaultPlan(f.engine(), fi, f.itemIDs(fi))
	if err != nil {
		t.Fatal(err)
	}

	// A rollback happened: the superseded backup is now the live one.
	f.write("var/lib/casanova/backups/.last-upgrade-backup", superseded+"\n", 0)
	f.age(filepath.Join(superseded, "update-report.json"), 0)

	if perr := precheckUpgradeBackups(f.engine(), items); perr == nil {
		t.Fatal("the precheck accepted deleting the current rollback point")
	}
}
