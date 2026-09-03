package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
)

// buildDomainXML renders a libvirt domain definition from the API config.
//
// The shape mirrors what the ZimaOS oracle produced via
// pkg/configurator.{addQcow2DiskConfig,AddBootAndFirmwareConfig} and the
// libvirtxml.Domain* types seen in the binary: a KVM/q35 guest with
// host-passthrough CPU, virtio disk, virtio NIC, a VNC/SPICE graphics device
// and an optional emulated TPM. UEFI is expressed with the modern
// `<os firmware='efi'>` attribute so libvirt auto-selects the OVMF firmware.
func buildDomainXML(cfg *model.VirtualMachineConfiguration) (string, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return "", fmt.Errorf("domain name is required")
	}

	mem := cfg.Memory.ValueMiB
	if mem == 0 {
		mem = 2048
	}
	vcpu := cfg.Vcpu.Count
	if vcpu == 0 {
		vcpu = 2
	}

	os := cfg.Os
	if os.Type == "" {
		os.Type = "hvm"
	}
	if os.Arch == "" {
		os.Arch = "x86_64"
	}
	if os.Machine == "" {
		os.Machine = "q35"
	}

	var b strings.Builder
	b.WriteString(`<domain type='kvm'>`)
	fmt.Fprintf(&b, `<name>%s</name>`, esc(cfg.Name))
	if cfg.Description != "" {
		fmt.Fprintf(&b, `<description>%s</description>`, esc(cfg.Description))
	}
	fmt.Fprintf(&b, `<memory unit='MiB'>%d</memory>`, mem)
	fmt.Fprintf(&b, `<currentMemory unit='MiB'>%d</currentMemory>`, mem)
	fmt.Fprintf(&b, `<vcpu placement='static'>%d</vcpu>`, vcpu)

	// OS / firmware / boot order.
	if os.Firmware == model.FirmwareUEFI {
		fmt.Fprintf(&b, `<os firmware='efi'>`)
	} else {
		b.WriteString(`<os>`)
	}
	fmt.Fprintf(&b, `<type arch='%s' machine='%s'>%s</type>`, esc(os.Arch), esc(os.Machine), esc(os.Type))
	// Boot order lives on the devices themselves (see bootOrder), not here: the
	// class-level `<boot dev='hd'/>` form is what OVMF ignores, and libvirt
	// rejects mixing the two. Only the boot menu stays.
	b.WriteString(`<bootmenu enable='yes'/>`)
	b.WriteString(`</os>`)

	b.WriteString(`<features><acpi/><apic/></features>`)
	b.WriteString(`<cpu mode='host-passthrough' check='none' migratable='on'/>`)
	b.WriteString(`<clock offset='utc'/>`)
	b.WriteString(`<on_poweroff>destroy</on_poweroff><on_reboot>restart</on_reboot><on_crash>destroy</on_crash>`)

	b.WriteString(`<devices>`)
	b.WriteString(`<emulator>/usr/bin/qemu-system-x86_64</emulator>`)

	// Disks. Auto-assign target dev letters per bus when not supplied.
	boot := bootOrder(cfg.Disks)
	vd, sd := 'a', 'a'
	for i, d := range cfg.Disks {
		driver := d.Driver
		if driver == "" {
			if d.Device == model.DiskDeviceCDROM {
				driver = "raw"
			} else {
				driver = "qcow2"
			}
		}
		bus := d.Bus
		if bus == "" {
			if d.Device == model.DiskDeviceCDROM {
				bus = model.DiskBusSATA
			} else {
				bus = model.DiskBusVirtio
			}
		}
		target := d.Target
		if target == "" {
			if bus == model.DiskBusVirtio {
				target = fmt.Sprintf("vd%c", vd)
				vd++
			} else {
				target = fmt.Sprintf("sd%c", sd)
				sd++
			}
		}
		fmt.Fprintf(&b, `<disk type='file' device='%s'>`, esc(string(d.Device)))
		fmt.Fprintf(&b, `<driver name='qemu' type='%s'/>`, esc(driver))
		fmt.Fprintf(&b, `<source file='%s'/>`, esc(d.Source))
		fmt.Fprintf(&b, `<target dev='%s' bus='%s'/>`, esc(target), esc(string(bus)))
		if d.ReadOnly || d.Device == model.DiskDeviceCDROM {
			b.WriteString(`<readonly/>`)
		}
		if n, ok := boot[i]; ok {
			fmt.Fprintf(&b, `<boot order='%d'/>`, n)
		}
		b.WriteString(`</disk>`)
	}

	// Network interfaces.
	for _, n := range cfg.Interfaces {
		typ := n.Type
		if typ == "" {
			typ = model.IfaceTypeNetwork
		}
		modelType := n.Model
		if modelType == "" {
			modelType = "virtio"
		}
		fmt.Fprintf(&b, `<interface type='%s'>`, esc(string(typ)))
		switch typ {
		case model.IfaceTypeBridge:
			src := n.Source
			if src == "" {
				src = "br0"
			}
			fmt.Fprintf(&b, `<source bridge='%s'/>`, esc(src))
		default: // network / nat -> libvirt virtual network
			src := n.Source
			if src == "" {
				src = "default"
			}
			fmt.Fprintf(&b, `<source network='%s'/>`, esc(src))
		}
		if n.Mac != "" {
			fmt.Fprintf(&b, `<mac address='%s'/>`, esc(n.Mac))
		}
		fmt.Fprintf(&b, `<model type='%s'/>`, esc(modelType))
		b.WriteString(`</interface>`)
	}

	// Graphics / console. Default to a listen-all VNC if none specified so the
	// UI always has a console to attach to.
	graphics := cfg.Graphics
	if len(graphics) == 0 {
		graphics = []model.DomainGraphic{{Type: model.GraphicVNC, Port: -1, Autoport: true, Listen: "0.0.0.0"}}
	}
	for _, g := range graphics {
		listen := g.Listen
		if listen == "" {
			listen = "0.0.0.0"
		}
		autoport := "yes"
		portAttr := ""
		if !g.Autoport && g.Port > 0 {
			autoport = "no"
			portAttr = fmt.Sprintf(` port='%d'`, g.Port)
		}
		passAttr := ""
		if g.Password != "" {
			passAttr = fmt.Sprintf(` passwd='%s'`, esc(g.Password))
		}
		fmt.Fprintf(&b, `<graphics type='%s'%s autoport='%s' listen='%s'%s>`,
			esc(string(g.Type)), portAttr, autoport, esc(listen), passAttr)
		fmt.Fprintf(&b, `<listen type='address' address='%s'/>`, esc(listen))
		b.WriteString(`</graphics>`)
	}
	b.WriteString(`<video><model type='virtio'/></video>`)
	b.WriteString(`<console type='pty'/>`)
	b.WriteString(`<channel type='unix'><target type='virtio' name='org.qemu.guest_agent.0'/></channel>`)

	// Optional emulated TPM (swtpm).
	if cfg.Tpm != nil {
		m := cfg.Tpm.Model
		if m == "" {
			m = "tpm-tis"
		}
		v := cfg.Tpm.Version
		if v == "" {
			v = "2.0"
		}
		fmt.Fprintf(&b, `<tpm model='%s'><backend type='emulator' version='%s'/></tpm>`, esc(m), esc(v))
	}

	b.WriteString(`</devices>`)
	b.WriteString(`</domain>`)
	return b.String(), nil
}

// bootOrder assigns the per-device `<boot order='N'/>` values, keyed by index
// into disks. An absent key means the device gets no <boot> element at all.
//
// Why per-device and not `<boot dev='hd'/>` at the <os> level: the class form
// only reaches qemu as the legacy `-boot order=`, which SeaBIOS honours and
// OVMF does not read. EDK2 reorders its boot attempts from the fw_cfg
// `bootorder` file, qemu writes that file solely from per-device `bootindex`
// properties, and libvirt only emits bootindex for `<boot order='N'/>` inside a
// device. So a UEFI guest defined from {empty disk + install ISO} never saw our
// order: OVMF enumerated on its own, found no ESP on the blank disk and dropped
// into the internal UEFI shell instead of the DVD — the reported "new VM does
// not boot the installer". libvirt treats the two forms as mutually exclusive,
// which is why the os-level pair is gone rather than kept alongside.
//
// Order: the first non-cdrom disk, then the cdroms in declaration order. An
// installed guest still boots itself, and a fresh one falls through to the
// install media because BDS tries each entry in turn and moves to the next when
// one fails. A cdrom-only domain starts its numbering at 1. Extra data disks
// are deliberately left unnumbered: without a bootindex they are not boot
// candidates, so a blank second disk can never shadow the install media.
// SeaBIOS reads bootindex too, so both firmwares end up with one semantics.
func bootOrder(disks []model.DomainDisk) map[int]int {
	order := make(map[int]int, len(disks))
	n := 1
	for i, d := range disks {
		if d.Device != model.DiskDeviceCDROM {
			order[i] = n
			n++
			break
		}
	}
	for i, d := range disks {
		if d.Device == model.DiskDeviceCDROM {
			order[i] = n
			n++
		}
	}
	return order
}

// esc XML-escapes a value for use inside an attribute or element.
func esc(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
