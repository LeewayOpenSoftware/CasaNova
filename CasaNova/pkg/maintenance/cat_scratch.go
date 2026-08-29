package maintenance

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova/service"
)

// ─── 3.6 casanova-scratch ───────────────────────────────────────────────────
//
// Four shapes that OUR code and only our code produces:
//
//  1. <target>/.temp/<md5hex><nChunks>/…   v1 chunked-upload scratch
//  2. <destination>.casanova-tmp           interrupted atomic copy stub
//  3. <destination>.upload-<8 hex>.tmp     v2 upload session (pre-allocated to
//     the final size, so possibly tens of GB)
//  4. <destination>.upload-<UnixNano>.tmp  v1 single-shot upload
//
// Shape 4 is the reason this category exists at all: nothing cleans it up
// today. The boot sweeper only knows .temp and .casanova-tmp, and
// isUploadTmpPath requires exactly 8 hex characters, so a 19-digit timestamp
// never matches. It is an unbounded leak.
//
// Detection REUSES the existing code rather than cloning it (rule 8): shape 1
// goes through service.IsOrphanChunkGroup — the sweeper's isChunkGroup, whose
// 22 table-driven cases each pin one guard — and shape 3 is decided by the
// upload-session REGISTRY, not by age (rule 7). A .tmp whose bitmap is
// complete is one rename away from landing; deleting it would destroy a file
// the user already watched reach 100%.

const (
	// scratchStubMinAge matches SweepOrphanTempDirs' own safety margin. Same
	// cutoff, deliberately: a maintenance screen that is more aggressive than
	// the janitor would be a second, competing policy.
	scratchStubMinAge = time.Hour

	// legacyUploadMinAge applies to shape 4, which has no registry to consult.
	// A day of silence on a single-shot upload means the request is long gone.
	legacyUploadMinAge = 24 * time.Hour

	// scratchMaxDepth mirrors the boot sweeper's maxSweepDepth. Our leftovers
	// sit next to the directory the user was uploading into, which is a handful
	// of levels below a storage root, never buried inside a photo library.
	scratchMaxDepth = 6

	// casanovaTmpSuffix is pkg/utils/file's tmpSuffix, which is unexported.
	// Keep this literal identical to it and to temp_sweeper.go's check.
	casanovaTmpSuffix = ".casanova-tmp"
)

// legacyUploadTmpRe is shape 4: `<destination>.upload-<16..20 digits>.tmp`.
// Digits only, and long enough to be a UnixNano timestamp — the 8-hex v2 form
// can never match it, so the two shapes stay disjoint.
var legacyUploadTmpRe = regexp.MustCompile(`^.+\.upload-[0-9]{16,20}\.tmp$`)

func scanScratch(e *Engine) *Finding {
	f := newFinding(CatScratch)

	registryOK := service.UploadSessionsReadable(e.env.UploadSessionsFile)
	registered := service.RegisteredUploadTmpPaths(e.env.UploadSessionsFile)
	if !registryOK {
		// Without a readable registry we cannot tell a live v2 session from a
		// leftover, so that shape is withheld. The other three still work.
		f.Notes = append(f.Notes, "the upload session registry could not be read: v2 upload leftovers are not offered")
	}

	now := e.env.now()
	stubCutoff := now.Add(-scratchStubMinAge)
	legacyCutoff := now.Add(-legacyUploadMinAge)
	sessionCutoff := now.Add(-service.UploadSessionMaxAge())

	sz := newSizer()
	lim := newPlanLimits()
	remote := e.guard.mounts()

	for _, root := range e.env.ScratchRoots {
		root = filepath.Clean(root)
		if root == "" {
			continue
		}
		rootDepth := strings.Count(root, string(os.PathSeparator))

		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable node: skip it, never abort the scan
			}
			if d.IsDir() {
				// Rule 18: a dead NFS/CIFS share answers nothing, and /mnt and
				// /media are themselves mount parents, so we cannot simply
				// refuse to leave the root's filesystem the way `find -xdev`
				// would — that would stop the walk from ever reaching an
				// external disk.
				if p != root && remote[p] {
					return filepath.SkipDir
				}
				if denyComponentNames[d.Name()] {
					return filepath.SkipDir
				}
				if d.Name() == ".temp" {
					e.scanChunkGroups(f, p, stubCutoff, sz, lim)
					// Everything under here has been inspected; descending
					// would only re-report entries we already judged.
					return filepath.SkipDir
				}
				if strings.Count(p, string(os.PathSeparator))-rootDepth >= scratchMaxDepth {
					return filepath.SkipDir
				}
				return nil
			}

			name := d.Name()
			switch {
			case strings.HasSuffix(name, casanovaTmpSuffix):
				e.addScratchFile(f, p, name, "interrupted atomic copy", stubCutoff, sz, lim)

			case service.IsUploadTmpPath(p):
				if !registryOK {
					return nil
				}
				if registered[p] {
					// Registered means resumable, whatever its age. The client
					// may simply be a laptop that is closed.
					return nil
				}
				e.addScratchFile(f, p, name, "upload session no longer in the registry", sessionCutoff, sz, lim)

			case legacyUploadTmpRe.MatchString(name):
				e.addScratchFile(f, p, name, "interrupted single-shot upload", legacyCutoff, sz, lim)
			}
			return nil
		})
	}

	f.Preselect = f.ItemCount > 0
	if f.ItemCount == 0 {
		f.Summary = "No upload or copy leftovers. Those flows are cleaning up after themselves."
	} else {
		f.Summary = fmt.Sprintf("%d leftover(s) from interrupted uploads or copies.", f.ItemCount)
	}
	return f
}

// scanChunkGroups inspects one `.temp` directory.
//
// The shape test is applied one level DOWN, because PostFileUpload writes
// `<target>/.temp/<hash><chunks>/…`. A loose file directly under `.temp` is not
// something we wrote: it is left alone, and leaving it also leaves the `.temp`
// itself.
func (e *Engine) scanChunkGroups(f *Finding, tempDir string, cutoff time.Time, sz *sizer, lim *planLimits) {
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		group := filepath.Join(tempDir, entry.Name())
		// The whole decision — name shape, contents, per-entry age — is the
		// boot sweeper's, called rather than reimplemented.
		if !service.IsOrphanChunkGroup(group, cutoff) {
			continue
		}
		it, err := e.item(CatScratch, group, entry.Name(), "abandoned upload chunks", cutoff, sz, lim)
		if err != nil {
			f.Notes = append(f.Notes, entry.Name()+" skipped: "+err.Error())
			continue
		}
		if holder, held := e.anyTargetHeldOpen(it); held {
			f.Notes = append(f.Notes, entry.Name()+" skipped: still open by "+holder)
			continue
		}
		it.Preselected = true
		f.addItem(it)
	}
}

// addScratchFile validates and appends one leftover file.
func (e *Engine) addScratchFile(f *Finding, path, name, detail string, cutoff time.Time, sz *sizer, lim *planLimits) {
	st, err := lstatNoFollow(path)
	if err != nil || !isRegular(&st) {
		// Not a regular file: a symlink wearing our suffix is somebody
		// probing, and a directory with that name is not ours either.
		return
	}
	if !mtimeOf(&st).Before(cutoff) {
		return // too fresh to be sure nobody is still writing
	}
	it, err := e.item(CatScratch, path, name, detail, cutoff, sz, lim)
	if err != nil {
		f.Notes = append(f.Notes, name+" skipped: "+err.Error())
		return
	}
	if holder, held := e.anyTargetHeldOpen(it); held {
		f.Notes = append(f.Notes, name+" skipped: still open by "+holder)
		return
	}
	it.Preselected = true
	f.addItem(it)
}

func precheckScratch(e *Engine, items []Item) *PreflightError {
	// The registry is re-read right before deleting: a v2 session that started
	// during the confirmation dialog must not lose its pre-allocated file.
	if !service.UploadSessionsReadable(e.env.UploadSessionsFile) {
		return &PreflightError{Code: BlockUploadRegistry, Reason: "the upload session registry could not be read"}
	}
	registered := service.RegisteredUploadTmpPaths(e.env.UploadSessionsFile)
	for _, it := range items {
		if registered[it.Path] {
			return &PreflightError{
				Code:   BlockUploadRegistry,
				Reason: "an upload session was registered for " + it.Path + " after the scan",
			}
		}
	}
	return nil
}
