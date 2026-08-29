package fstab

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	PassDoNotCheck      = 0
	PassCheckDuringBoot = 1
	PassCheckAfterBoot  = 2

	DefaultPath = "/etc/fstab"

	// casaosMarker is the trailing comment Add() writes on every entry it
	// creates. RemoveByMountPoint refuses to delete any line that lacks
	// this marker — that's the only thing keeping a malicious or buggy
	// caller from deleting the system root from /etc/fstab and bricking
	// the next boot.
	//
	// "CasaOS" is preserved (not "CasaNova") because pre-rebrand installs
	// have years of fstab entries with the old marker; rejecting them
	// would orphan every legacy mount.
	casaosMarker = "# Added by the CasaOS"
)

var (
	_fstab *FStab

	ErrInvalidFSTabEntry                     = errors.New("invalid fstab entry")
	ErrDifferentFSTabEntryWithSameMountPoint = errors.New("a different fstab entry with the same mount point already exists")
	// ErrEntryNotManaged is raised when Add() or RemoveByMountPoint() finds an
	// existing entry at the requested mount point that we did not create.
	// Refusing to stomp it is safer than appending a duplicate or pretending to
	// have replaced it — and reporting the refusal is what stops the caller
	// from claiming success over an entry that is still there.
	ErrEntryNotManaged = errors.New("fstab entry exists but is not managed by CasaNova")
)

type (
	Entry struct {
		// The device name, label, UUID, or other means of specifying the partition or data source this entry refers to.
		Source string

		// Where the contents of the device may be accessed after mounting
		MountPoint string

		// The type of file system to be mounted.
		FSType string

		// Options describing various other aspects of the file system, such as whether it is automatically mounted at boot, which users may mount or access it, whether it may be written to or only read from, its size, and so forth; the special option defaults refers to a pre-determined set of options depending on the file system type.
		Options string

		// A number indicating whether and how often the file system should be backed up by the dump program; a zero indicates the file system will never be automatically backed up.
		Dump int

		// A number indicating the order in which the fsck program will check the devices for errors at boot time
		Pass int
	}

	FStab struct {
		path string
	}
)

func (e *Entry) String() string {
	return e.Source + "\t" + e.MountPoint + "\t" + e.FSType + "\t" + e.Options + "\t" + strconv.Itoa(e.Dump) + "\t" + strconv.Itoa(e.Pass)
}

func (f *FStab) Add(e Entry, replace bool) error {
	entry, err := f.GetEntryByMountPoint(e.MountPoint)
	if err != nil {
		return err
	}

	if entry != nil {
		if !replace &&
			(entry.Source != e.Source ||
				entry.FSType != e.FSType ||
				entry.Options != e.Options ||
				entry.Dump != e.Dump ||
				entry.Pass != e.Pass) {
			return ErrDifferentFSTabEntryWithSameMountPoint
		}

		// Wave 26: only remove an existing entry if WE put it there.
		// Otherwise the marker check inside RemoveByMountPoint silently
		// no-ops and we'd append a duplicate line below.
		managed, mErr := f.entryHasMarker(e.MountPoint)
		if mErr != nil {
			return mErr
		}
		if !managed {
			return ErrEntryNotManaged
		}

		if err := f.RemoveByMountPoint(e.MountPoint, false); err != nil {
			return err
		}
	}

	if err := copy(f.path, f.path+".casaos.bak"); err != nil {
		return err
	}

	fstabFile, err := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer fstabFile.Close()

	_, err = fstabFile.WriteString(e.String() + "\t" + casaosMarker + "\n")
	if err != nil {
		return err
	}

	return err
}

// entryHasMarker scans /etc/fstab and returns true if the line whose
// mount point matches mountpoint also carries the casaosMarker comment.
// Used by Add() to decide whether it owns the existing entry.
func (f *FStab) entryHasMarker(mountpoint string) (bool, error) {
	found := false

	if err := foreachLine(f.path, func(line string) error {
		entry, _ := parseEntry(line)
		if entry != nil && entry.MountPoint == mountpoint && strings.Contains(line, casaosMarker) {
			found = true
		}
		return nil
	}); err != nil {
		return false, err
	}
	return found, nil
}

func (f *FStab) RemoveByMountPoint(mountpoint string, comment bool) error {
	FStabPathNew := f.path + ".casaos.new"
	FStabFileNew, err := os.OpenFile(FStabPathNew, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// Belt-and-braces close: previously this fd leaked on every
	// invocation because nothing called Close(). Each remount cycle
	// would push the descriptor count up by one until the daemon hit
	// EMFILE on storage operations. We also added O_TRUNC above so a
	// stale .casaos.new file from a crashed prior run can't bleed
	// extra lines into the rebuilt fstab.
	defer FStabFileNew.Close()

	// Track what we saw so the caller can tell "nothing there" (fine) from
	// "there is one but it is not ours" (must surface).
	sawForeign := false
	removed := false

	if err := foreachLine(f.path, func(line string) error {
		entry, _ := parseEntry(line)
		if entry != nil && entry.MountPoint == mountpoint && !strings.Contains(line, casaosMarker) {
			sawForeign = true
		}
		// Wave 26: only ever touch entries that *we* added. The casaosMarker
		// trailing comment is the only authoritative signal — without it
		// we copy the line through verbatim. This blocks the entire class
		// of "umount /boot/efi → fstab loses the system root → next boot
		// drops to a recovery shell" failure modes, whether triggered
		// maliciously or by an off-by-one bug elsewhere.
		if entry != nil && entry.MountPoint == mountpoint && strings.Contains(line, casaosMarker) {
			removed = true
			if comment {
				_, err := FStabFileNew.WriteString("#" + line + "\n")
				return err
			}
			return nil
		}

		_, err := FStabFileNew.WriteString(line + "\n")
		return err
	}); err != nil {
		return err
	}

	// Flush + close BEFORE the rename so the new fstab is fully on
	// disk by the time the kernel atomically replaces /etc/fstab. The
	// deferred Close above is a safety net for the early-return error
	// paths; the explicit one here is what guarantees durability.
	if err := FStabFileNew.Sync(); err != nil {
		return err
	}
	if err := FStabFileNew.Close(); err != nil {
		return err
	}

	if err := copy(f.path, f.path+".casaos.bak"); err != nil {
		return err
	}

	if err := os.Rename(FStabPathNew, f.path); err != nil {
		return err
	}

	// The rewrite succeeded; now report whether it actually removed anything.
	// Reported last so the file is already consistent when the caller sees the
	// refusal, and so a foreign entry never blocks removing our own.
	// Report the refusal with the sentinel this package already defines for
	// exactly this condition in Add(). Removing kept returning nil whether or
	// not it removed anything, so RemoveFromFStab reported success while the
	// entry survived and the mount reappeared on the next boot — a guard that
	// refuses without saying so is indistinguishable from one that did the work.
	if !removed && sawForeign {
		return ErrEntryNotManaged
	}
	return nil
}

func (f *FStab) GetEntries() ([]*Entry, error) {
	entries := []*Entry{}

	if err := foreachLine(f.path, func(line string) error {
		entry, err := parseEntry(line)
		if err != nil {
			return err
		}
		if entry != nil {
			entries = append(entries, entry)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return entries, nil
}

func (f *FStab) GetEntryByMountPoint(mountpoint string) (*Entry, error) {
	entries, err := f.GetEntries()
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.MountPoint == mountpoint {
			return entry, nil
		}
	}

	return nil, nil
}

func (f *FStab) GetEntryBySource(source string) (*Entry, error) {
	entries, err := f.GetEntries()
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.Source == source {
			return entry, nil
		}
	}

	return nil, nil
}

func Get() *FStab {
	if _fstab == nil {
		_fstab = &FStab{
			path: DefaultPath,
		}
	}

	return _fstab
}

func parseEntry(line string) (*Entry, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil, nil
	}

	fields := strings.Fields(line)
	if len(fields) < 4 {
		return nil, nil
	}

	entry := Entry{
		Dump: 0,
		Pass: PassDoNotCheck,
	}

	entry.Source = fields[0]
	entry.MountPoint = fields[1]
	entry.FSType = fields[2]
	entry.Options = fields[3]

	if len(fields) > 4 {
		dump, err := strconv.Atoi(fields[4])
		if err != nil {
			return nil, ErrInvalidFSTabEntry
		}
		entry.Dump = dump
	}

	if len(fields) > 5 {
		pass, err := strconv.Atoi(fields[5])
		if err != nil {
			return nil, ErrInvalidFSTabEntry
		}
		entry.Pass = pass
	}

	return &entry, nil
}

func foreachLine(path string, handle func(line string) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Split(bufio.ScanLines)

	for scanner.Scan() {
		line := scanner.Text()
		if err := handle(line); err != nil {
			return err
		}
	}

	return nil
}

func copy(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}
