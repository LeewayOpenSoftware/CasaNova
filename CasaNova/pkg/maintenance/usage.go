package maintenance

import (
	"golang.org/x/sys/unix"
)

// Space accounting (design rule 14).
//
// We count st_blocks*512 per UNIQUE inode, not st_size, and we say so in the
// UI. Two measurements from this box are why:
//
//   - the orphan journal reports 375 MB apparent and 180 MB real, because
//     journald writes sparse files. Promising 375 MB and freeing 180 would
//     look like a bug in the delete path.
//   - `du` over the box sums ~95 GB on a filesystem `df` calls 78 GB used,
//     because hardlinked and multiply-mounted trees get counted more than once.
//
// The run still reports the statfs delta afterwards, which is the only number
// that is not an estimate.

// allocatedBytes is what the entry actually costs the filesystem.
func allocatedBytes(st *unix.Stat_t) int64 {
	// st_blocks is defined in 512-byte units regardless of the fs block size.
	return st.Blocks * 512
}

// sizer sums allocated bytes while charging each inode exactly once.
//
// Scoped to one finding: an inode counted for one category must not be counted
// again for another, and a hardlink inside the same plan must not inflate the
// promise. Links whose other names live OUTSIDE the plan are a different
// problem — unlinking one name frees nothing — but the statfs delta at the end
// is what tells the user the truth about those.
type sizer struct {
	seen map[devIno]bool
}

func newSizer() *sizer { return &sizer{seen: map[devIno]bool{}} }

func (s *sizer) add(st *unix.Stat_t) int64 {
	key := devIno{st.Dev, st.Ino}
	if s.seen[key] {
		return 0
	}
	s.seen[key] = true
	return allocatedBytes(st)
}

// diskUsage is a statfs snapshot of the filesystem holding path. Bavail (not
// Bfree) is what a non-root user could actually use, but this service runs as
// root and the reserved blocks are genuinely available to it, so Free reports
// Bavail anyway: it is the number `df` shows the user.
func diskUsage(path string) (DiskInfo, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return DiskInfo{Path: path}, err
	}
	bsize := int64(st.Bsize)
	total := int64(st.Blocks) * bsize
	free := int64(st.Bavail) * bsize
	return DiskInfo{
		Path:  path,
		Total: total,
		Used:  total - int64(st.Bfree)*bsize,
		Free:  free,
	}, nil
}
