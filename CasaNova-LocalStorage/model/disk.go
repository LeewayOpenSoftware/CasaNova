/*@Author: LinkLeong dev@lucentopen.org
 *@Date: 2022-07-13 10:43:45
 *@LastEditors: LinkLeong
 *@LastEditTime: 2022-08-03 14:45:35
 *@FilePath: /CasaOS/model/disk.go
 *@Description:
 *@Website: https://casanova.dev
 *Copyright (c) 2022-2026 Leeway Open Software.
 */
package model

import "encoding/json"

type LSBLKModel struct {
	Name        string       `json:"name"`
	FsType      string       `json:"fstype"`
	Size        uint64       `json:"size"`
	FSSize      json.Number  `json:"fssize"`
	Path        string       `json:"path"`
	Model       string       `json:"model"` // device identifier
	RM          bool         `json:"rm"`    // removable device flag
	RO          bool         `json:"ro"`    // read-only device flag
	State       string       `json:"state"`
	PhySec      int          `json:"phy-sec"` // physical sector size
	Type        string       `json:"type"`
	Vendor      string       `json:"vendor"`  // vendor
	Rev         string       `json:"rev"`     // revision
	FSAvail     json.Number  `json:"fsavail"` // available space
	FSUse       string       `json:"fsuse%"`  // used percentage
	MountPoint  string       `json:"mountpoint"`
	Format      string       `json:"format"`
	Health      string       `json:"health"`
	HotPlug     bool         `json:"hotplug"`
	UUID        string       `json:"uuid"`
	PTUUID      string       `json:"ptuuid"`
	PartUUID    string       `json:"partuuid"`
	FSUsed      json.Number  `json:"fsused"`
	Temperature int          `json:"temperature"`
	Tran        string       `json:"tran"`
	MinIO       uint64       `json:"min-io"`
	UsedPercent float64      `json:"used_percent"`
	Serial      string       `json:"serial"`
	Children    []LSBLKModel `json:"children"`
	SubSystems  string       `json:"subsystems"`
	Label       string       `json:"label"`

	StartSector uint64 `json:"start_sector,omitempty"`
	Rota        bool   `json:"rota"` // true(HDD) false(SSD)
	DiskType    string `json:"disk_type"`
	EndSector   uint64 `json:"end_sector,omitempty"`

	// CasaNova v0.5.0 additions
	IsSSD          bool   `json:"is_ssd"`                 // Derived from Rota + sysfs check
	SmartHealth    string `json:"smart_health,omitempty"` // "GOOD", "WARNING", "CRITICAL", "UNKNOWN"
	SmartSupported bool   `json:"smart_supported"`
	PowerOnHours   int    `json:"power_on_hours,omitempty"`
	IsUSB          bool   `json:"is_usb"` // Multi-check USB detection
}

type Drive struct {
	Name               string         `json:"name"`
	Size               uint64         `json:"size"`
	Model              string         `json:"model"`
	Health             string         `json:"health"`
	Temperature        int            `json:"temperature"`
	DiskType           string         `json:"disk_type"`
	NeedFormat         bool           `json:"need_format"`
	Serial             string         `json:"serial"`
	Path               string         `json:"path"`
	UUID               string         `json:"uuid"`        // disk-level UUID (PTUUID when present)
	UsedBytes          uint64         `json:"used_bytes"`  // sum of mounted children — 0 when nothing mounted
	AvailBytes         uint64         `json:"avail_bytes"` // sum of mounted children — 0 when nothing mounted
	ChildrenNumber     int            `json:"children_number"`
	Children           []DiskChildren `json:"children"`
	Supported          bool           `json:"supported"`
	PowerOnHours       int            `json:"power_on_hours"`
	ReallocatedSectors int            `json:"reallocated_sectors"`
}
type DiskChildren struct {
	Name       string `json:"name"`
	Size       uint64 `json:"size"`
	Format     string `json:"format"`
	UUID       string `json:"uuid"`        // filesystem UUID — stable across reboots
	MountPoint string `json:"mount_point"` // empty if not mounted
	UsedBytes  uint64 `json:"used_bytes"`  // statfs-derived; 0 when not mounted
	AvailBytes uint64 `json:"avail_bytes"` // statfs-derived; 0 when not mounted
	// UsageUnknown says the byte counts above mean nothing: the filesystem did
	// not answer statfs within its budget (a dead network share, a dying disk).
	// Without this flag the UI cannot tell an unresponsive mount from an empty
	// one, and renders "0 B used" for a filesystem that may well be full.
	UsageUnknown bool `json:"usage_unknown"`
	// UsageStale says the byte counts are the last successful measurement,
	// served because the filesystem stopped answering. Real numbers, but not
	// current ones.
	UsageStale bool `json:"usage_stale"`
	Supported  bool `json:"supported"`
}

type USBDriveStatus struct {
	Name     string        `json:"name"`
	Size     uint64        `json:"size"`
	Model    string        `json:"model"`
	Avail    uint64        `json:"avail"`
	Children []USBChildren `json:"children"`
}
type USBChildren struct {
	Name       string `json:"name"`
	Size       uint64 `json:"size"`
	Avail      uint64 `json:"avail"`
	MountPoint string `json:"mount_point"`
}

type Storage struct {
	UUID        string `json:"uuid"`
	MountPoint  string `json:"mount_point"`
	Size        string `json:"size"`
	Avail       string `json:"avail"`
	Used        string `json:"used"`
	Type        string `json:"type"`
	Path        string `json:"path"`
	DriveName   string `json:"drive_name"`
	Label       string `json:"label"`
	PersistedIn string `json:"persisted_in"` // none, fstab, casaos
}
type Storages struct {
	DiskName string    `json:"disk_name"`
	Size     uint64    `json:"size"`
	Path     string    `json:"path"`
	Children []Storage `json:"children"`
	Type     string    `json:"type"`
}

type DiskStatus struct {
	Size   uint64 `json:"size"`
	Avail  uint64 `json:"avail"` // available space
	Health bool   `json:"health"`
	Used   uint64 `json:"used"`
}
