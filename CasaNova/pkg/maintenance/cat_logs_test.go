package maintenance

import (
	"path/filepath"
	"testing"
	"time"
)

// ─── 3.3 system-rotated-logs ────────────────────────────────────────────────

func stageRotatedLogs(f *fixture) {
	// A normal rotation: live file, two rotated copies.
	f.write("var/log/syslog", "live", 0)
	f.write("var/log/syslog.1", "rotated yesterday", 5*time.Hour)
	f.write("var/log/syslog.2.gz", "rotated last week", 120*time.Hour)

	// Rotation-shaped, but nothing live next to it: not ours to judge.
	f.write("var/log/orphan.1", "service that no longer exists", 400*24*time.Hour)

	// Rotated minutes ago: the writer may not have reopened yet.
	f.write("var/log/fresh.log", "live", 0)
	f.write("var/log/fresh.log.1", "just rotated", 10*time.Minute)

	// Forensic evidence, excluded by directory.
	f.write("var/log/audit/audit.log", "live", 0)
	f.write("var/log/audit/audit.log.1", "rotated", 200*time.Hour)

	// A per-service subdirectory one level down.
	f.write("var/log/casaos/app-management.log", "live", 0)
	f.write("var/log/casaos/app-management.log.1", "rotated", 200*time.Hour)
}

func TestRotatedLogsOffersOnlyRotationsWithALiveBase(t *testing.T) {
	f := newFixture(t)
	stageRotatedLogs(f)

	fi := f.scan(CatRotatedLogs)
	if fi.Blocked {
		t.Fatalf("category blocked: %s", fi.BlockedReason)
	}
	labels := f.itemLabels(fi)

	for _, want := range []string{"syslog.1", "syslog.2.gz", "app-management.log.1"} {
		if !contains(labels, want) {
			t.Errorf("%s was not offered; got %v", want, labels)
		}
	}
	for _, mustKeep := range []string{
		"syslog",      // the live file
		"orphan.1",    // no live base: we cannot prove anything about it
		"fresh.log.1", // rotated minutes ago
		"audit.log.1", // /var/log/audit is excluded outright
		"audit.log",   // ditto
	} {
		if contains(labels, mustKeep) {
			t.Errorf("%s was offered for deletion", mustKeep)
		}
	}
}

// The writer has not written to the live file since the rotation, so it may
// still hold the rotated one open. Skip until it has clearly moved on.
func TestRotatedLogsWaitsForTheWriterToReopen(t *testing.T) {
	f := newFixture(t)
	// Live file OLDER than its rotated copy: the reopen has not happened.
	f.write("var/log/lagging.log", "live", 300*time.Hour)
	f.write("var/log/lagging.log.1", "rotated", 200*time.Hour)

	fi := f.scan(CatRotatedLogs)
	if contains(f.itemLabels(fi), "lagging.log.1") {
		t.Fatal("a rotation whose writer has not reopened was offered")
	}
}

// Rule 6 at scan time: a file some process still holds open is withheld with
// an explanation instead of being promised and then skipped.
func TestRotatedLogsSkipsFilesWithAnOpenDescriptor(t *testing.T) {
	f := newFixture(t)
	stageRotatedLogs(f)

	st, err := lstatNoFollow(filepath.Join(f.root, "var/log/syslog.1"))
	if err != nil {
		t.Fatal(err)
	}
	f.env.FD = fakeFD{held: map[devIno]string{{st.Dev, st.Ino}: "pid 900 (rsyslogd)"}}

	fi := f.scan(CatRotatedLogs)
	if contains(f.itemLabels(fi), "syslog.1") {
		t.Fatal("a rotated log with an open descriptor was offered")
	}
	if !contains(f.itemLabels(fi), "syslog.2.gz") {
		t.Fatal("the open descriptor on one file suppressed the others too")
	}
}

// Without a readable /proc, "nobody has it open" and "we cannot tell" look the
// same. The category refuses rather than assume.
func TestRotatedLogsBlockedWithoutProc(t *testing.T) {
	f := newFixture(t)
	stageRotatedLogs(f)
	f.env.ProcRoot = filepath.Join(f.root, "no-such-proc")

	fi := f.scan(CatRotatedLogs)
	if !fi.Blocked || fi.BlockedCode != BlockNoProcAccess {
		t.Fatalf("an unreadable /proc did not block the category: %+v", fi)
	}
}

// ─── 3.4 orphan-journal ─────────────────────────────────────────────────────

const (
	currentMachine = "f4acef217a5844b8b31fbd7071f13e7b"
	orphanMachine  = "faf1d59ba7c84452a4b9f44f4d6f9ff0"
	recentMachine  = "0123456789abcdef0123456789abcdef"
)

func stageJournals(f *fixture) {
	f.write("etc/machine-id", currentMachine+"\n", 0)
	// This machine's own journal is deliberately as OLD as the orphan's. With
	// Storage=volatile that is exactly what a healthy box looks like — the
	// persistent directory stopped being written to long ago — so the only
	// thing standing between it and deletion is the machine-id identity check.
	f.write("var/log/journal/"+currentMachine+"/system.journal", "this machine's journal", 400*24*time.Hour)
	f.age(filepath.Join(f.root, "var/log/journal/"+currentMachine), 400*24*time.Hour)
	f.write("var/log/journal/"+orphanMachine+"/system@aaaa.journal", "dead journal", 400*24*time.Hour)
	f.write("var/log/journal/"+orphanMachine+"/system@bbbb.journal", "dead journal", 400*24*time.Hour)
	f.age(filepath.Join(f.root, "var/log/journal/"+orphanMachine), 400*24*time.Hour)
}

func TestOrphanJournalDeletesByMachineIDIdentity(t *testing.T) {
	f := newFixture(t)
	stageJournals(f)

	fi := f.scan(CatOrphanJournal)
	if fi.Blocked {
		t.Fatalf("category blocked: %s", fi.BlockedReason)
	}
	if len(fi.Items) != 1 || fi.Items[0].Label != orphanMachine {
		t.Fatalf("expected exactly the orphan journal, got %+v", f.itemLabels(fi))
	}
	if contains(f.itemLabels(fi), currentMachine) {
		t.Fatal("this machine's own journal was offered")
	}
}

// A journal that is not this machine's but is still recent stays: it could be
// inside somebody's retention window.
func TestOrphanJournalRespectsTheRetentionFloor(t *testing.T) {
	f := newFixture(t)
	stageJournals(f)
	f.write("var/log/journal/"+recentMachine+"/system@cccc.journal", "recent", time.Hour)

	fi := f.scan(CatOrphanJournal)
	if contains(f.itemLabels(fi), recentMachine) {
		t.Fatal("a recent journal was offered despite the retention floor")
	}
}

// Without a machine-id there is no identity to compare against, so nothing is
// an orphan.
func TestOrphanJournalBlockedWithoutMachineID(t *testing.T) {
	f := newFixture(t)
	stageJournals(f)
	if err := removeFile(filepath.Join(f.root, "etc/machine-id")); err != nil {
		t.Fatal(err)
	}

	fi := f.scan(CatOrphanJournal)
	if !fi.Blocked || fi.BlockedCode != BlockInconsistent {
		t.Fatalf("a missing machine-id did not block the category: %+v", fi)
	}
	if len(fi.Items) != 0 {
		t.Fatal("journals were offered with no way to identify the live one")
	}
}

// The run-time re-check: if the machine-id now matches a planned directory,
// that directory is the LIVE journal.
func TestOrphanJournalPrecheckRefusesTheLiveJournal(t *testing.T) {
	f := newFixture(t)
	stageJournals(f)
	fi := f.scan(CatOrphanJournal)
	items, err := defaultPlan(f.engine(), fi, f.itemIDs(fi))
	if err != nil {
		t.Fatal(err)
	}

	f.write("etc/machine-id", orphanMachine+"\n", 0)
	if perr := precheckOrphanJournal(f.engine(), items); perr == nil {
		t.Fatal("the precheck accepted deleting this machine's own journal")
	}
}

// A journal directory whose files are still open by journald is withheld.
func TestOrphanJournalSkipsOpenJournals(t *testing.T) {
	f := newFixture(t)
	stageJournals(f)
	st, err := lstatNoFollow(filepath.Join(f.root, "var/log/journal/"+orphanMachine+"/system@aaaa.journal"))
	if err != nil {
		t.Fatal(err)
	}
	f.env.FD = fakeFD{held: map[devIno]string{{st.Dev, st.Ino}: "pid 1 (systemd-journald)"}}

	fi := f.scan(CatOrphanJournal)
	if len(fi.Items) != 0 {
		t.Fatalf("a journal with an open file was offered: %+v", f.itemLabels(fi))
	}
}

// wtmp/btmp are the login and failed-login history — a rotated wtmp.1 passes
// the same "live base, old, no writer" test every other rotation does, so
// without an explicit exclusion it was not just offered but PRE-TICKED, and the
// default Analyse→Clean-logs flow would have destroyed the login audit trail.
// This pins that they are never offered at all.
func TestRotatedLogsExcludesLoginRecords(t *testing.T) {
	f := newFixture(t)
	// A live base plus an old rotated copy for each security record.
	for _, base := range []string{"wtmp", "btmp", "faillog", "lastlog"} {
		f.write("var/log/"+base, "live", 0)
		f.write("var/log/"+base+".1", "rotated", 200*time.Hour)
	}
	// A normal service log alongside, to prove the category still works.
	f.write("var/log/syslog", "live", 0)
	f.write("var/log/syslog.1", "rotated", 5*time.Hour)

	fi := f.scan(CatRotatedLogs)
	if fi.Blocked {
		t.Fatalf("category blocked: %s", fi.BlockedReason)
	}
	labels := f.itemLabels(fi)
	for _, forbidden := range []string{"wtmp.1", "btmp.1", "faillog.1", "lastlog.1"} {
		if contains(labels, forbidden) {
			t.Errorf("login record %s was offered for deletion; got %v", forbidden, labels)
		}
	}
	if !contains(labels, "syslog.1") {
		t.Errorf("the ordinary rotation was lost; got %v", labels)
	}
}
