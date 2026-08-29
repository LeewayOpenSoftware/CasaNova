package maintenance

import (
	"path/filepath"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova/common"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils"
	"github.com/LeewayOpenSoftware/CasaNova/service"
)

// Env holds every path and every outside signal the catalogue depends on.
//
// Everything is a field rather than a constant for one reason: this package
// deletes files as root, and a test that cannot point it at a fixture tree is
// a test nobody writes. Production values come from DefaultEnv(); the handler
// layer may override the two hooks at the bottom, which are the only pieces of
// state that live in route/v1 and would otherwise force an import cycle.
type Env struct {
	// Our own artefacts.
	UpdatesDir        string // staged packages (also the .part upload scratch)
	UpdatesArchiveDir string // superseded packages kept by hand
	BackupsDir        string // upgrade-<ts>/ trees + the .last-upgrade-backup pointer
	ManifestFile      string // /var/lib/casanova/manifest.json — the installed version
	UpdateStatusFile  string // written by casanova-update.sh while it runs

	// The OS's.
	LogDir         string // /var/log
	JournalDir     string // /var/log/journal
	MachineIDFile  string // /etc/machine-id
	AptArchivesDir string // /var/cache/apt/archives
	DpkgLockFile   string // /var/lib/dpkg/lock-frontend
	AptLockFile    string // /var/cache/apt/archives/lock

	// Phase-2 scratch. Held behind ScratchEnabled: the scratch category is the
	// only one that walks the user's data area (/DATA, /mnt, /media), and
	// walking 5+ TB and deleting as root is not safe until design rule 1 — the
	// untouchable set built from every container mount and every app compose
	// volume — is implemented and validated against a live box. Until then the
	// category is defined but never wired into a scan or a run, and its roots
	// are kept OUT of the guard's whitelist so nothing can reach them even by
	// mistake. Flipping this to true without rule 1 in place is a data-loss
	// hazard, not a feature toggle.
	ScratchEnabled     bool
	ScratchRoots       []string // where our own upload/copy leftovers can appear
	UploadSessionsFile string   // registry of live v2 upload sessions

	// Where run manifests are written (rule 12) and the run lock lives.
	StateDir string

	// RunningVersion is the build that is executing right now. Distinct from
	// the manifest's version on a dev box where a binary was hand-deployed,
	// and both are excluded from deletion.
	RunningVersion string

	// ProcRoot is /proc in production; a fixture in tests.
	ProcRoot string

	Now func() time.Time

	// FD answers "is this inode open" (rule 6). nil ⇒ a live /proc scan.
	FD fdLookup

	// RemoteMounts is rule 18's set of mount points we refuse to walk into.
	// nil ⇒ service.UnwalkableMounts(), the ONE parser this product has for
	// that question.
	RemoteMounts map[string]bool

	// UpdateStage lets the handler hand us route/v1's in-memory update state.
	// nil ⇒ we read UpdateStatusFile, which the update script writes and which
	// survives the restart the update itself causes.
	UpdateStage func() string

	// AptSelfManaged lets the handler say "the apt lock is held because WE are
	// applying updates", so the blocked reason can name the real cause instead
	// of a generic lock. nil ⇒ generic reason.
	AptSelfManaged func() (bool, string)

	// SystemBusy contributes the rest of rule 11's "system is quiet" test —
	// a copy/move job in flight, a trash purge running — which is state held
	// in service/ and route/ and cannot be read from here without a cycle.
	// nil ⇒ only the signals this package can prove for itself are checked.
	SystemBusy func() (bool, string)
}

// DefaultEnv is the production wiring. Every path here is a literal because
// each one is a decision: see the corresponding §3.x card in
// docs/design/maintenance-module.md.
func DefaultEnv() *Env {
	return &Env{
		UpdatesDir:        "/var/lib/casanova/updates",
		UpdatesArchiveDir: "/var/lib/casanova/updates_archive",
		BackupsDir:        "/var/lib/casanova/backups",
		ManifestFile:      "/var/lib/casanova/manifest.json",
		UpdateStatusFile:  "/var/run/casanova/update-status.json",

		LogDir:         "/var/log",
		JournalDir:     "/var/log/journal",
		MachineIDFile:  "/etc/machine-id",
		AptArchivesDir: "/var/cache/apt/archives",
		DpkgLockFile:   "/var/lib/dpkg/lock-frontend",
		AptLockFile:    "/var/cache/apt/archives/lock",

		// The scratch roots are the canonical storage roots, reused rather
		// than re-listed so an install with the /var/lib/casaos/files fallback
		// layout is covered by the same code.
		ScratchRoots:       append([]string{}, utils.DataRoots...),
		UploadSessionsFile: "",

		StateDir:       "/var/lib/casanova/maintenance",
		RunningVersion: common.BUILD_VERSION,
		ProcRoot:       "/proc",
	}
}

// now is the clock, injectable for tests.
func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// allowedRoots is the whitelist handed to the Guard: the ONLY directories any
// category may reach into.
//
// Note what is NOT here. /var/lib/casanova itself is absent so nothing can
// touch the databases, the manifest or the config next to the two directories
// we do clean. /var/lib/apt/lists is absent (design §2: it would leave a box
// unable to install anything until the next apt update). /var/lib/docker is
// absent and stays that way — dockerd is the only legitimate owner of that
// tree.
func (e *Env) allowedRoots() []string {
	roots := []string{
		e.UpdatesDir,
		e.UpdatesArchiveDir,
		e.BackupsDir,
		e.LogDir,
		e.AptArchivesDir,
	}
	// The journal lives under /var/log, but list it explicitly: if LogDir is
	// ever narrowed, the journal category must fail loudly rather than quietly
	// start matching against the wrong root.
	if e.JournalDir != "" {
		roots = append(roots, e.JournalDir)
	}
	// The scratch roots (the user's data area) enter the whitelist ONLY when
	// scratch is enabled. A root the guard accepts is a root a bad plan could
	// delete inside; there is no reason to trust /DATA to the chokepoint while
	// no enabled category scans it.
	if e.ScratchEnabled {
		roots = append(roots, e.ScratchRoots...)
	}
	return roots
}

func (e *Env) remoteMounts() map[string]bool {
	if e.RemoteMounts != nil {
		return e.RemoteMounts
	}
	// service.UnwalkableMounts is the existing parser (temp_sweeper.go), not a
	// copy of it with different thresholds — see rule 18.
	return service.UnwalkableMounts()
}

func (e *Env) manifestDir() string { return filepath.Join(e.StateDir, "manifests") }

func (e *Env) lockFile() string { return filepath.Join(e.StateDir, "run.lock") }
