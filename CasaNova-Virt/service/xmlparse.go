package service

import (
	"encoding/xml"

	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
)

// domainXMLView is a partial unmarshal of a libvirt domain document — just the
// fields the API surfaces back to the UI. We deliberately do not model the full
// libvirt schema (the oracle used libvirtxml.Domain, a ~10k-line generated
// type); we read only what GET /domain/:name and the console endpoint need.
type domainXMLView struct {
	XMLName xml.Name `xml:"domain"`
	Name    string   `xml:"name"`
	UUID    string   `xml:"uuid"`
	Memory  struct {
		Unit  string `xml:"unit,attr"`
		Value uint64 `xml:",chardata"`
	} `xml:"memory"`
	Vcpu uint16 `xml:"vcpu"`
	OS   struct {
		Type struct {
			Arch    string `xml:"arch,attr"`
			Machine string `xml:"machine,attr"`
			Value   string `xml:",chardata"`
		} `xml:"type"`
		Firmware string `xml:"firmware,attr"`
	} `xml:"os"`
	Devices struct {
		Disks []struct {
			Device string `xml:"device,attr"`
			Driver struct {
				Type string `xml:"type,attr"`
			} `xml:"driver"`
			Source struct {
				File string `xml:"file,attr"`
			} `xml:"source"`
			Target struct {
				Dev string `xml:"dev,attr"`
				Bus string `xml:"bus,attr"`
			} `xml:"target"`
			ReadOnly *struct{} `xml:"readonly"`
		} `xml:"disk"`
		Interfaces []struct {
			Type   string `xml:"type,attr"`
			Source struct {
				Network string `xml:"network,attr"`
				Bridge  string `xml:"bridge,attr"`
			} `xml:"source"`
			Mac struct {
				Address string `xml:"address,attr"`
			} `xml:"mac"`
			Model struct {
				Type string `xml:"type,attr"`
			} `xml:"model"`
		} `xml:"interface"`
		Graphics []struct {
			Type     string `xml:"type,attr"`
			Port     int    `xml:"port,attr"`
			Listen   string `xml:"listen,attr"`
			Autoport string `xml:"autoport,attr"`
			Passwd   string `xml:"passwd,attr"`
		} `xml:"graphics"`
	} `xml:"devices"`
}

func parseDomainXML(raw string) (*domainXMLView, error) {
	var v domainXMLView
	if err := xml.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// disks converts the parsed disks to the API model.
func (v *domainXMLView) disks() []model.DomainDisk {
	out := make([]model.DomainDisk, 0, len(v.Devices.Disks))
	for _, d := range v.Devices.Disks {
		out = append(out, model.DomainDisk{
			Device:   model.DomainDiskDevice(d.Device),
			Driver:   d.Driver.Type,
			Source:   d.Source.File,
			Target:   d.Target.Dev,
			Bus:      model.DomainDiskBus(d.Target.Bus),
			ReadOnly: d.ReadOnly != nil,
		})
	}
	return out
}

func (v *domainXMLView) interfaces() []model.DomainInterface {
	out := make([]model.DomainInterface, 0, len(v.Devices.Interfaces))
	for _, n := range v.Devices.Interfaces {
		src := n.Source.Network
		if src == "" {
			src = n.Source.Bridge
		}
		out = append(out, model.DomainInterface{
			Type:   model.DomainInterfaceType(n.Type),
			Source: src,
			Model:  n.Model.Type,
			Mac:    n.Mac.Address,
		})
	}
	return out
}

func (v *domainXMLView) graphics() []model.DomainGraphic {
	out := make([]model.DomainGraphic, 0, len(v.Devices.Graphics))
	for _, g := range v.Devices.Graphics {
		out = append(out, model.DomainGraphic{
			Type:     model.GraphicType(g.Type),
			Port:     g.Port,
			Listen:   g.Listen,
			Autoport: g.Autoport == "yes",
			Password: g.Passwd,
		})
	}
	return out
}

func (v *domainXMLView) os() *model.DomainOs {
	fw := model.FirmwareBIOS
	if v.OS.Firmware == "efi" {
		fw = model.FirmwareUEFI
	}
	return &model.DomainOs{
		Type:     v.OS.Type.Value,
		Arch:     v.OS.Type.Arch,
		Machine:  v.OS.Type.Machine,
		Firmware: fw,
	}
}
