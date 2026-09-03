package service

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
)

// The bug: a VM created by the wizard (UEFI by default, empty qcow2 + install
// ISO) came up in the internal UEFI shell instead of booting the DVD. The cause
// was in this file — buildDomainXML emitted class-level `<boot dev='hd'/>
// <boot dev='cdrom'/>`, which qemu turns into the legacy `-boot order=` that
// SeaBIOS honours and OVMF ignores outright. OVMF only reorders from the fw_cfg
// `bootorder` file, which qemu populates from per-device `bootindex`, which
// libvirt emits only for `<boot order='N'/>` inside a device.
//
// These tests assert the emitted shape, which is the whole fix: the order has
// to be on the devices and the class form has to be gone (libvirt rejects the
// mix). They parse the XML rather than grepping it, so "the disk carries
// order 1" means the element is really inside that <disk>.

type bootXML struct {
	XMLName xml.Name `xml:"domain"`
	OS      struct {
		Boot []struct {
			Dev string `xml:"dev,attr"`
		} `xml:"boot"`
		BootMenu *struct {
			Enable string `xml:"enable,attr"`
		} `xml:"bootmenu"`
	} `xml:"os"`
	Devices struct {
		Disks []struct {
			Device string `xml:"device,attr"`
			Target struct {
				Dev string `xml:"dev,attr"`
			} `xml:"target"`
			Boot *struct {
				Order int `xml:"order,attr"`
			} `xml:"boot"`
		} `xml:"disk"`
	} `xml:"devices"`
}

func parseBootXML(t *testing.T, cfg *model.VirtualMachineConfiguration) bootXML {
	t.Helper()
	s, err := buildDomainXML(cfg)
	if err != nil {
		t.Fatalf("buildDomainXML: %v", err)
	}
	var got bootXML
	if err := xml.Unmarshal([]byte(s), &got); err != nil {
		t.Fatalf("emitted XML does not parse: %v\n%s", err, s)
	}
	// The class form is mutually exclusive with the per-device one, so its
	// absence is part of the contract, not a nicety.
	if len(got.OS.Boot) != 0 {
		t.Errorf("os-level <boot dev=...> still emitted (%d of them); libvirt "+
			"forbids mixing it with per-device <boot order=>", len(got.OS.Boot))
	}
	// Belt and braces: the parsed check above only looks under <os>, this one
	// catches the class form emitted anywhere at all.
	if strings.Contains(s, "<boot dev=") {
		t.Error("raw XML still contains a class-level <boot dev=>")
	}
	return got
}

func uefiCfg(disks ...model.DomainDisk) *model.VirtualMachineConfiguration {
	return &model.VirtualMachineConfiguration{
		Name:  "vm",
		Os:    model.DomainOs{Firmware: model.FirmwareUEFI},
		Disks: disks,
	}
}

var (
	blankDisk  = model.DomainDisk{Device: model.DiskDeviceDisk, Source: "/var/lib/casanova/vm/vm.qcow2", SizeGiB: 20}
	installISO = model.DomainDisk{Device: model.DiskDeviceCDROM, Source: "/var/lib/casanova/iso/debian.iso"}
	dataDisk   = model.DomainDisk{Device: model.DiskDeviceDisk, Source: "/var/lib/casanova/vm/data.qcow2", SizeGiB: 100}
	seedISO    = model.DomainDisk{Device: model.DiskDeviceCDROM, Source: "/var/lib/casanova/vm/vm-cidata.iso"}
)

// The exact shape the wizard produces, i.e. the human's bug.
func TestBuildDomainXML_UEFIEmptyDiskPlusISOGetsPerDeviceBootIndex(t *testing.T) {
	got := parseBootXML(t, uefiCfg(blankDisk, installISO))

	if len(got.Devices.Disks) != 2 {
		t.Fatalf("want 2 disks, got %d", len(got.Devices.Disks))
	}
	disk, cdrom := got.Devices.Disks[0], got.Devices.Disks[1]
	if disk.Boot == nil || disk.Boot.Order != 1 {
		t.Errorf("disk %s: want <boot order='1'/>, got %+v", disk.Target.Dev, disk.Boot)
	}
	if cdrom.Boot == nil || cdrom.Boot.Order != 2 {
		t.Errorf("cdrom %s: want <boot order='2'/>, got %+v", cdrom.Target.Dev, cdrom.Boot)
	}
	// Keeping the menu was explicitly part of the fix.
	if got.OS.BootMenu == nil || got.OS.BootMenu.Enable != "yes" {
		t.Errorf("want <bootmenu enable='yes'/> kept, got %+v", got.OS.BootMenu)
	}
}

// bootindex is firmware-independent: SeaBIOS reads the same fw_cfg file, so the
// BIOS path must not keep the legacy form "because it used to work there".
func TestBuildDomainXML_BIOSGetsTheSameBootIndex(t *testing.T) {
	cfg := uefiCfg(blankDisk, installISO)
	cfg.Os.Firmware = model.FirmwareBIOS
	got := parseBootXML(t, cfg)

	if got.Devices.Disks[0].Boot == nil || got.Devices.Disks[0].Boot.Order != 1 {
		t.Errorf("bios disk: want order 1, got %+v", got.Devices.Disks[0].Boot)
	}
	if got.Devices.Disks[1].Boot == nil || got.Devices.Disks[1].Boot.Order != 2 {
		t.Errorf("bios cdrom: want order 2, got %+v", got.Devices.Disks[1].Boot)
	}
}

// An installed guest with no media attached still boots itself.
func TestBuildDomainXML_DiskOnlyBootsTheDisk(t *testing.T) {
	got := parseBootXML(t, uefiCfg(blankDisk))
	if got.Devices.Disks[0].Boot == nil || got.Devices.Disks[0].Boot.Order != 1 {
		t.Errorf("want order 1 on the only disk, got %+v", got.Devices.Disks[0].Boot)
	}
}

// A live-media domain with no hard disk: numbering starts at 1 on the cdrom, or
// nothing is bootable at all.
func TestBuildDomainXML_CDROMOnlyStartsAtOne(t *testing.T) {
	got := parseBootXML(t, uefiCfg(installISO))
	if got.Devices.Disks[0].Boot == nil || got.Devices.Disks[0].Boot.Order != 1 {
		t.Errorf("want order 1 on the only cdrom, got %+v", got.Devices.Disks[0].Boot)
	}
}

// Extra data disks stay unnumbered: with no bootindex they are not boot
// candidates, so a blank second disk can never shadow the install media. The
// cloud-init seed cdrom is appended after the installer (domain.go:212), so it
// must sort after it.
func TestBuildDomainXML_ExtraDataDiskUnnumbered_SeedISOLast(t *testing.T) {
	got := parseBootXML(t, uefiCfg(blankDisk, dataDisk, installISO, seedISO))

	want := []*int{intp(1), nil, intp(2), intp(3)}
	if len(got.Devices.Disks) != len(want) {
		t.Fatalf("want %d disks, got %d", len(want), len(got.Devices.Disks))
	}
	for i, w := range want {
		d := got.Devices.Disks[i]
		switch {
		case w == nil && d.Boot != nil:
			t.Errorf("disk[%d] (%s): want no <boot>, got order %d", i, d.Target.Dev, d.Boot.Order)
		case w != nil && d.Boot == nil:
			t.Errorf("disk[%d] (%s): want order %d, got no <boot>", i, d.Target.Dev, *w)
		case w != nil && d.Boot.Order != *w:
			t.Errorf("disk[%d] (%s): want order %d, got %d", i, d.Target.Dev, *w, d.Boot.Order)
		}
	}
}

// Nothing to number must not panic or emit a stray element.
func TestBuildDomainXML_NoDisks(t *testing.T) {
	got := parseBootXML(t, uefiCfg())
	if len(got.Devices.Disks) != 0 {
		t.Fatalf("want no disks, got %d", len(got.Devices.Disks))
	}
}

func intp(n int) *int { return &n }
