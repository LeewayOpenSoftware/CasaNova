package service

import (
	"fmt"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/pkg/libvirtconn"
	"github.com/digitalocean/go-libvirt"
	"path/filepath"
	"strings"
)

// StorageService exposes libvirt storage pools and their volumes.
type StorageService struct {
	conn *libvirtconn.Manager
}

func NewStorageService(conn *libvirtconn.Manager) *StorageService {
	return &StorageService{conn: conn}
}

// ListPools returns all defined storage pools with usage figures.
func (s *StorageService) ListPools() ([]model.StoragePool, error) {
	l, err := s.conn.Conn()
	if err != nil {
		return nil, err
	}
	pools, _, err := l.ConnectListAllStoragePools(1, 0)
	if err != nil {
		return nil, err
	}
	out := make([]model.StoragePool, 0, len(pools))
	for _, p := range pools {
		mp := model.StoragePool{
			Name: p.Name,
			UUID: uuidString(p.UUID),
		}
		if state, cap, alloc, avail, err := l.StoragePoolGetInfo(p); err == nil {
			mp.State = poolStateString(state)
			mp.CapacityB = cap
			mp.AllocationB = alloc
			mp.AvailableB = avail
		}
		out = append(out, mp)
	}
	return out, nil
}

// ListVolumes returns the volumes contained in the named pool.
func (s *StorageService) ListVolumes(poolName string) ([]model.StorageVolume, error) {
	l, err := s.conn.Conn()
	if err != nil {
		return nil, err
	}
	pool, err := l.StoragePoolLookupByName(poolName)
	if err != nil {
		return nil, err
	}
	vols, _, err := l.StoragePoolListAllVolumes(pool, 1, 0)
	if err != nil {
		return nil, err
	}
	out := make([]model.StorageVolume, 0, len(vols))
	for _, v := range vols {
		mv := model.StorageVolume{
			Name: v.Name,
			Key:  v.Key,
		}
		if path, err := l.StorageVolGetPath(v); err == nil {
			mv.Path = path
		}
		if _, cap, alloc, err := l.StorageVolGetInfo(v); err == nil {
			mv.CapacityB = cap
			mv.AllocationB = alloc
		}
		out = append(out, mv)
	}
	return out, nil
}

func poolStateString(state uint8) string {
	switch libvirt.StoragePoolState(state) {
	case libvirt.StoragePoolInactive:
		return "inactive"
	case libvirt.StoragePoolBuilding:
		return "building"
	case libvirt.StoragePoolRunning:
		return "running"
	case libvirt.StoragePoolDegraded:
		return "degraded"
	case libvirt.StoragePoolInaccessible:
		return "inaccessible"
	default:
		return "unknown"
	}
}

// CreateVolume makes a new disk image in a pool.
//
// WHY THIS EXISTS. The storage side of this service was read-only: ListPools
// and ListVolumes reported what was there and nothing created anything. So a
// VM could be defined but had no path from "I want a VM" to "it has a disk to
// install onto" that did not go through a shell. That gap is the reason the
// design note for this module called storage the blocker rather than the
// domain API.
//
// qcow2 is the default because it allocates lazily: a 40 GiB disk costs what
// the guest has actually written, which is what makes several VMs on one NAS
// disk practical. raw is offered for the case where that indirection is not
// wanted.
func (s *StorageService) CreateVolume(poolName, name string, capacityB uint64, format string) (*model.StorageVolume, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("volume name is required")
	}
	// The name becomes a filename inside the pool's directory, and it arrives
	// from an HTTP body. Anything that is not a plain name is refused rather
	// than sanitised, so there is no question of what was written where.
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("invalid volume name")
	}
	if capacityB == 0 {
		return nil, fmt.Errorf("capacity is required")
	}
	if format != "qcow2" && format != "raw" {
		format = "qcow2"
	}

	l, err := s.conn.Conn()
	if err != nil {
		return nil, err
	}
	pool, err := l.StoragePoolLookupByName(poolName)
	if err != nil {
		return nil, err
	}

	// Built with a template rather than an XML library because this is the
	// whole document and every field is either a validated name, a number, or
	// one of two constants — there is no user-controlled text left to escape.
	volXML := fmt.Sprintf(`<volume>
  <name>%s</name>
  <capacity unit="bytes">%d</capacity>
  <allocation unit="bytes">0</allocation>
  <target>
    <format type="%s"/>
  </target>
</volume>`, name, capacityB, format)

	v, err := l.StorageVolCreateXML(pool, volXML, 0)
	if err != nil {
		return nil, err
	}

	out := &model.StorageVolume{Name: v.Name, Key: v.Key, CapacityB: capacityB}
	if path, err := l.StorageVolGetPath(v); err == nil {
		out.Path = path
	}
	return out, nil
}

// DeleteVolume removes a disk image from a pool.
//
// It refuses a volume any domain still references. Deleting a disk out from
// under a defined VM turns a working guest into one that fails to start with a
// message about a missing file, and by then the connection to this action is
// gone.
func (s *StorageService) DeleteVolume(poolName, name string) error {
	l, err := s.conn.Conn()
	if err != nil {
		return err
	}
	pool, err := l.StoragePoolLookupByName(poolName)
	if err != nil {
		return err
	}
	v, err := l.StorageVolLookupByName(pool, name)
	if err != nil {
		return err
	}

	if path, err := l.StorageVolGetPath(v); err == nil {
		if users := domainsReferencing(l, path); len(users) > 0 {
			return fmt.Errorf("in use by %s", strings.Join(users, ", "))
		}
	}
	return l.StorageVolDelete(v, 0)
}

// domainsReferencing names the domains whose XML mentions a path.
func domainsReferencing(l *libvirt.Libvirt, path string) []string {
	doms, _, err := l.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil
	}
	var users []string
	for _, d := range doms {
		xml, err := l.DomainGetXMLDesc(d, 0)
		if err != nil {
			continue
		}
		if strings.Contains(xml, path) {
			users = append(users, d.Name)
		}
	}
	return users
}
