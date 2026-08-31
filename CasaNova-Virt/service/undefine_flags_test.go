package service

// Deleting a VM returned 500 and left the domain defined:
//
//   Requested operation is not valid: cannot undefine domain with nvram
//
// A UEFI guest gets its NVRAM file the first time it STARTS, not when it is
// defined — so a VM created through this API deleted fine until it had been
// booted once, and was undeletable from then on. Reproduced end to end on the
// test box before this changed: create + delete a UEFI domain succeeded;
// create, start, stop, delete failed and the domain survived.
//
// This cannot exercise libvirt, so it pins the one thing that made the call
// fail: which leftovers the flag set tells libvirt it may remove. A revert to
// plain DomainUndefine, or a flag set that drops NVRAM, fails here.

import (
	"testing"

	"github.com/digitalocean/go-libvirt"
)

func TestUndefineFlags_CoverTheLeftoversLibvirtRefusesToOrphan(t *testing.T) {
	cases := []struct {
		flag libvirt.DomainUndefineFlagsValues
		why  string
	}{
		{libvirt.DomainUndefineNvram, "a booted UEFI guest owns an NVRAM file; without this, deleting it is refused"},
		{libvirt.DomainUndefineManagedSave, "a suspended-to-disk guest owns a save image"},
		{libvirt.DomainUndefineSnapshotsMetadata, "a guest with snapshots owns their metadata"},
	}
	for _, c := range cases {
		if undefineFlags&c.flag == 0 {
			t.Errorf("flag %d missing: %s", c.flag, c.why)
		}
	}
}

func TestUndefineFlags_DoNotKeepNvram(t *testing.T) {
	// KEEP_NVRAM is the opposite request and libvirt rejects it together with
	// NVRAM. Setting both would fail every delete rather than fix any.
	if undefineFlags&libvirt.DomainUndefineKeepNvram != 0 {
		t.Error("KeepNvram contradicts Nvram; libvirt refuses the pair")
	}
}

func TestUndefineFlags_AreNotZero(t *testing.T) {
	// Guards the guard: a zero flag set makes DomainUndefineFlags behave
	// exactly like the plain DomainUndefine this replaced, and the assertions
	// above would still need to fail — but only if they are looking at
	// something. This says they are.
	if undefineFlags == 0 {
		t.Fatal("no flags set: this is the unfixed behaviour under a new name")
	}
}
