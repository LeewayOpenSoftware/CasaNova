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
	// Boot from disk first, then cdrom, so an installed guest boots itself but
	// a fresh one falls through to the install media.
	b.WriteString(`<boot dev='hd'/><boot dev='cdrom'/><bootmenu enable='yes'/>`)
	b.WriteString(`</os>`)

	b.WriteString(`<features><acpi/><apic/></features>`)
	b.WriteString(`<cpu mode='host-passthrough' check='none' migratable='on'/>`)
	b.WriteString(`<clock offset='utc'/>`)
	b.WriteString(`<on_poweroff>destroy</on_poweroff><on_reboot>restart</on_reboot><on_crash>destroy</on_crash>`)

	b.WriteString(`<devices>`)
	b.WriteString(`<emulator>/usr/bin/qemu-system-x86_64</emulator>`)

	// Disks. Auto-assign target dev letters per bus when not supplied.
	vd, sd := 'a', 'a'
	for _, d := range cfg.Disks {
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

// esc XML-escapes a value for use inside an attribute or element.
func esc(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
