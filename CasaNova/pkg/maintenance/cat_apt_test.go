package maintenance

import (
	"path/filepath"
	"testing"
)

// ─── 3.5 apt-package-cache ──────────────────────────────────────────────────

func stageAptCache(f *fixture) {
	f.write("var/cache/apt/archives/avahi-daemon_0.8-13ubuntu6.2_amd64.deb", "deb one", 0)
	f.write("var/cache/apt/archives/apport_2.28.3-0ubuntu0.1_all.deb", "deb two", 0)
	// A download in flight lives in partial/ and is never listed.
	f.write("var/cache/apt/archives/partial/half-downloaded_1.0_amd64.deb", "partial", 0)
	// The lock files apt uses.
	f.write("var/lib/dpkg/lock-frontend", "", 0)
	f.write("var/cache/apt/archives/lock", "", 0)
}

func TestAptCacheOffersInstalledDebsAndIgnoresPartial(t *testing.T) {
	f := newFixture(t)
	stageAptCache(f)

	fi := f.scan(CatAptCache)
	if fi.Blocked {
		t.Fatalf("category blocked with apt idle: %s", fi.BlockedReason)
	}
	labels := f.itemLabels(fi)
	if len(labels) != 2 {
		t.Fatalf("expected the two cached .deb files, got %v", labels)
	}
	if contains(labels, "half-downloaded_1.0_amd64.deb") {
		t.Fatal("a download in flight under partial/ was offered")
	}
}

// THE safeguard of this category. apt and dpkg lock with fcntl, so this test
// runs a real second process holding a real fcntl write lock: deleting a .deb
// mid-transaction can leave dpkg demanding `dpkg --configure -a` over SSH.
func TestAptCacheBlockedWhileDpkgHoldsTheLock(t *testing.T) {
	f := newFixture(t)
	stageAptCache(f)

	stop := holdLock(t, filepath.Join(f.root, "var/lib/dpkg/lock-frontend"))
	defer stop()

	fi := f.scan(CatAptCache)
	if !fi.Blocked || fi.BlockedCode != BlockDpkgLocked {
		t.Fatalf("a held dpkg lock did not block the category: %+v", fi)
	}
	// The size stays visible even when blocked, so the UI can say what is
	// being withheld and why.
	if fi.ReclaimableBytes <= 0 {
		t.Error("a blocked category reported no size at all")
	}
}

// The archives lock is checked too, not just the frontend one.
func TestAptCacheBlockedWhileArchivesLockHeld(t *testing.T) {
	f := newFixture(t)
	stageAptCache(f)

	stop := holdLock(t, filepath.Join(f.root, "var/cache/apt/archives/lock"))
	defer stop()

	fi := f.scan(CatAptCache)
	if !fi.Blocked {
		t.Fatal("a held archives lock did not block the category")
	}
}

// Re-checked at run time: unattended-upgrades can start between the scan and
// the click, and it is the apt user nobody remembers.
func TestAptCachePrecheckRunsAgainAtExecutionTime(t *testing.T) {
	f := newFixture(t)
	stageAptCache(f)
	fi := f.scan(CatAptCache)
	if fi.Blocked {
		t.Fatalf("category blocked before the lock was taken: %s", fi.BlockedReason)
	}

	stop := holdLock(t, filepath.Join(f.root, "var/lib/dpkg/lock-frontend"))
	defer stop()

	if perr := precheckAptCache(f.engine(), nil); perr == nil {
		t.Fatal("the precheck accepted a run while apt held its lock")
	}
}

// Our own apt runs get named specifically instead of showing a generic lock.
func TestAptCacheUsesOurOwnAptSignal(t *testing.T) {
	f := newFixture(t)
	stageAptCache(f)
	f.env.AptSelfManaged = func() (bool, string) {
		return true, "CasaNova is applying system updates"
	}

	fi := f.scan(CatAptCache)
	if !fi.Blocked || fi.BlockedReason != "CasaNova is applying system updates" {
		t.Fatalf("the host-admin apt signal was ignored: %+v", fi)
	}
}
