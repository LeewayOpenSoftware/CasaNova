package service

import (
	"fmt"

	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/pkg/libvirtconn"
	"github.com/digitalocean/go-libvirt"
)

// HostService reports hypervisor host facts (codegen.HostInfo in the oracle).
type HostService struct {
	conn *libvirtconn.Manager
}

func NewHostService(conn *libvirtconn.Manager) *HostService {
	return &HostService{conn: conn}
}

// Info gathers a host summary for the virtualization dashboard.
func (s *HostService) Info() (*model.HostInfo, error) {
	l, err := s.conn.Conn()
	if err != nil {
		return nil, err
	}

	info := &model.HostInfo{Arch: "x86_64", KVMEnabled: true}

	if hn, err := l.ConnectGetHostname(); err == nil {
		info.Hostname = hn
	}
	if typ, err := l.ConnectGetType(); err == nil {
		info.Hypervisor = typ
	}
	if ver, err := l.ConnectGetLibVersion(); err == nil {
		info.LibvirtVersion = formatVersion(ver)
	}

	model32, mem, cpus, _, _, _, _, _, err := l.NodeGetInfo()
	if err == nil {
		info.CPUModel = cStr(model32[:])
		info.CPUCount = int(cpus)
		info.MemoryTotalMiB = mem / 1024 // NodeGetInfo memory is KiB
	}

	// Domain counts.
	if active, _, err := l.ConnectListAllDomains(1, libvirt.ConnectListDomainsActive); err == nil {
		info.ActiveDomains = len(active)
	}
	if all, _, err := l.ConnectListAllDomains(1, libvirt.ConnectListDomainsActive|libvirt.ConnectListDomainsInactive); err == nil {
		info.TotalDomains = len(all)
	}

	return info, nil
}

// formatVersion decodes libvirt's packed version (major*1e6 + minor*1e3 + rel).
func formatVersion(v uint64) string {
	major := v / 1000000
	minor := (v % 1000000) / 1000
	rel := v % 1000
	return fmt.Sprintf("%d.%d.%d", major, minor, rel)
}

// cStr turns a NUL-terminated int8 C string into a Go string.
func cStr(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}
