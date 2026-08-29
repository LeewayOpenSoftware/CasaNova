package service

import (
	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/pkg/libvirtconn"
	"github.com/digitalocean/go-libvirt"
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
