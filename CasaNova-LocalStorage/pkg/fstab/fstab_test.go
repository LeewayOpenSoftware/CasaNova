package fstab

import (
	"os"
	"testing"

	"gotest.tools/v3/assert"
)

const fstabContent = `
	# UNCONFIGURED FSTAB FOR BASE SYSTEM
	LABEL=UEFI      /boot/efi       vfat    umask=0077      0 1
	/mnt/sdb:/mnt/sdc       /media  mergerfs        defaults,allow_other,category.create=mfs,moveonenospc=true,minfreespace=1M 0 0
	LABEL=desktop-rootfs    /               ext4    defaults        0 1
`

func TestFSTab(t *testing.T) {
	fstab := &FStab{path: t.TempDir() + "/fstab"}

	err := os.WriteFile(fstab.path, []byte(fstabContent), 0o600)
	assert.NilError(t, err)

	entries, err := fstab.GetEntries()
	assert.NilError(t, err)

	assert.Equal(t, len(entries), 3)

	entry, err := fstab.GetEntryByMountPoint("/media")
	assert.NilError(t, err)

	assert.Equal(t, entry.Source, "/mnt/sdb:/mnt/sdc")
	assert.Equal(t, entry.MountPoint, "/media")
	assert.Equal(t, entry.FSType, "mergerfs")
	assert.Equal(t, entry.Options, "defaults,allow_other,category.create=mfs,moveonenospc=true,minfreespace=1M")
	assert.Equal(t, entry.Dump, 0)
	assert.Equal(t, entry.Pass, PassDoNotCheck)

	// The fixture entry carries no casaosMarker, so it belongs to the operator:
	// removal must be REFUSED, and refused audibly. Silently returning nil here
	// is what made an unmount look successful while the entry survived and the
	// mount reappeared on the next boot.
	// The fixture entry carries no casaosMarker, so it belongs to the operator.
	// Both mutating paths must refuse it, and refuse AUDIBLY: returning nil
	// while the entry survives is what made an unmount look successful and let
	// the mount reappear on the next boot.
	err = fstab.RemoveByMountPoint(entry.MountPoint, false)
	assert.ErrorIs(t, err, ErrEntryNotManaged)

	stillThere, err := fstab.GetEntryByMountPoint(entry.MountPoint)
	assert.NilError(t, err)
	assert.Assert(t, stillThere != nil, "a foreign entry must survive the refusal")

	err = fstab.Add(*entry, true)
	assert.ErrorIs(t, err, ErrEntryNotManaged)
}

// An entry this package added carries the marker, so it owns it: adding and
// removing must both work. This is the other half of the contract — without it
// a "refuse everything" implementation would pass the suite above.
func TestFSTabManagesItsOwnEntries(t *testing.T) {
	path := t.TempDir() + "/fstab"
	assert.NilError(t, os.WriteFile(path, []byte(fstabContent), 0o600))
	fstab := &FStab{path: path}

	ours := Entry{
		Source:     "/mnt/sdd",
		MountPoint: "/media/ours",
		FSType:     "ext4",
		Options:    "defaults",
		Dump:       0,
		Pass:       PassDoNotCheck,
	}
	assert.NilError(t, fstab.Add(ours, false))

	got, err := fstab.GetEntryByMountPoint(ours.MountPoint)
	assert.NilError(t, err)
	assert.Assert(t, got != nil, "our own entry should be readable back")

	assert.NilError(t, fstab.RemoveByMountPoint(ours.MountPoint, false))

	gone, err := fstab.GetEntryByMountPoint(ours.MountPoint)
	assert.NilError(t, err)
	assert.Equal(t, gone, (*Entry)(nil))

	// The operator's untouched lines must still be there.
	efi, err := fstab.GetEntryByMountPoint("/boot/efi")
	assert.NilError(t, err)
	assert.Assert(t, efi != nil, "removing our entry must not disturb the operator's")
}
