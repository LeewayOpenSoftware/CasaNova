package model

// The JSON API model. Field names and the type hierarchy mirror the
// `codegen.*` types recovered from the ZimaOS-VirtManagement binary
// (Domain, DomainDisk, DomainInterface, DomainGraphic, DomainTpm, Firmware,
// DomainOs, DomainVcpu, DomainMemory, HostInfo, StoragePool, ...). See
// ANALYSIS.md for the provenance of each type.

// BaseResponse is the envelope used for simple message replies.
type BaseResponse struct {
	Message string `json:"message,omitempty"`
}

// ---- Domain (VM) ----------------------------------------------------------

// DomainStatusValue is the libvirt domain state, rendered as a string.
type DomainStatusValue string

const (
	StatusNoState     DomainStatusValue = "nostate"
	StatusRunning     DomainStatusValue = "running"
	StatusBlocked     DomainStatusValue = "blocked"
	StatusPaused      DomainStatusValue = "paused"
	StatusShutdown    DomainStatusValue = "shutdown"
	StatusShutoff     DomainStatusValue = "shutoff"
	StatusCrashed     DomainStatusValue = "crashed"
	StatusPMSuspended DomainStatusValue = "pmsuspended"
	StatusUnknown     DomainStatusValue = "unknown"
)

// DomainAction is the verb accepted by PUT /domain/:name/status.
// Values match the lifecycle transitions the oracle exposed.
type DomainAction string

const (
	ActionStart    DomainAction = "start"    // DomainCreate
	ActionShutdown DomainAction = "shutdown" // graceful ACPI DomainShutdown
	ActionReboot   DomainAction = "reboot"   // DomainReboot
	ActionStop     DomainAction = "stop"     // forceful DomainDestroy
	ActionSuspend  DomainAction = "suspend"  // DomainSuspend (pause)
	ActionResume   DomainAction = "resume"   // DomainResume
)

// DomainMemory in libvirt uses a unit + value. We keep MiB as the canonical
// external unit.
type DomainMemory struct {
	ValueMiB uint64 `json:"value"` // memory in MiB
}

// DomainVcpu is the virtual CPU count.
type DomainVcpu struct {
	Count uint `json:"count"`
}

// Firmware selects BIOS (SeaBIOS) or UEFI (OVMF). Matches codegen.Firmware.
type Firmware string

const (
	FirmwareBIOS Firmware = "bios"
	FirmwareUEFI Firmware = "uefi"
)

// DomainOs describes the guest OS/boot settings (codegen.DomainOs).
type DomainOs struct {
	Type     string   `json:"type,omitempty"`     // "hvm"
	Arch     string   `json:"arch,omitempty"`     // "x86_64"
	Machine  string   `json:"machine,omitempty"`  // "q35" / "pc-i440fx"
	Firmware Firmware `json:"firmware,omitempty"` // bios | uefi
}

// DomainDiskDevice — disk | cdrom (codegen.DomainDiskDevice).
type DomainDiskDevice string

const (
	DiskDeviceDisk  DomainDiskDevice = "disk"
	DiskDeviceCDROM DomainDiskDevice = "cdrom"
)

// DomainDiskBus — the guest-facing bus (codegen.DomainDiskTargetBus).
type DomainDiskBus string

const (
	DiskBusVirtio DomainDiskBus = "virtio"
	DiskBusSATA   DomainDiskBus = "sata"
	DiskBusIDE    DomainDiskBus = "ide"
	DiskBusSCSI   DomainDiskBus = "scsi"
	DiskBusUSB    DomainDiskBus = "usb"
)

// DomainDisk mirrors codegen.DomainDisk / DomainDiskSource / DomainDiskTarget /
// DomainDiskDriver.
type DomainDisk struct {
	Device DomainDiskDevice `json:"device"`           // disk | cdrom
	Driver string           `json:"driver,omitempty"` // qcow2 | raw
	Source string           `json:"source"`           // host file path
	Target string           `json:"target,omitempty"` // vda / sda ...
	Bus    DomainDiskBus    `json:"bus,omitempty"`
	// SizeGiB, when >0 on a create request, causes a qcow2 to be provisioned
	// via qemu-img at Source before the domain is defined.
	SizeGiB  uint64 `json:"size_gib,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// DomainInterfaceType — network | bridge | nat (codegen.DomainInterfaceType).
type DomainInterfaceType string

const (
	IfaceTypeNetwork DomainInterfaceType = "network"
	IfaceTypeBridge  DomainInterfaceType = "bridge"
	IfaceTypeNAT     DomainInterfaceType = "nat"
)

// DomainInterface mirrors codegen.DomainInterface + Source/Model/Mac.
type DomainInterface struct {
	Type   DomainInterfaceType `json:"type"`
	Source string              `json:"source,omitempty"` // network name / bridge name
	Model  string              `json:"model,omitempty"`  // virtio | e1000 | rtl8139
	Mac    string              `json:"mac,omitempty"`
}

// GraphicType — vnc | spice (codegen.DomainGraphic).
type GraphicType string

const (
	GraphicVNC   GraphicType = "vnc"
	GraphicSPICE GraphicType = "spice"
)

// DomainGraphic is the remote-console configuration (codegen.DomainGraphic).
type DomainGraphic struct {
	Type     GraphicType `json:"type"`
	Port     int         `json:"port,omitempty"`   // -1 => autoport
	Listen   string      `json:"listen,omitempty"` // 0.0.0.0 / 127.0.0.1
	Autoport bool        `json:"autoport,omitempty"`
	Password string      `json:"password,omitempty"`
}

// DomainTpm mirrors codegen.DomainTpm (swtpm emulator backend).
type DomainTpm struct {
	Model   string `json:"model,omitempty"`   // tpm-tis | tpm-crb
	Backend string `json:"backend,omitempty"` // emulator
	Version string `json:"version,omitempty"` // 2.0
}

// VirtualMachineConfiguration is the create/define request body. It maps to
// the oracle's top-level codegen.VirtualMachineConfiguration.
type VirtualMachineConfiguration struct {
	Name        string            `json:"name"`
	Memory      DomainMemory      `json:"memory"`
	Vcpu        DomainVcpu        `json:"vcpu"`
	Os          DomainOs          `json:"os"`
	Disks       []DomainDisk      `json:"disks"`
	Interfaces  []DomainInterface `json:"interfaces"`
	Graphics    []DomainGraphic   `json:"graphics"`
	Tpm         *DomainTpm        `json:"tpm,omitempty"`
	CloudInit   *CloudInitConfig  `json:"cloud_init,omitempty"`
	Description string            `json:"description,omitempty"`
}

// CloudInitConfig, when present on a create request, makes Define generate a
// NoCloud "cidata" seed ISO (user-data + meta-data) and attach it as a cdrom,
// so cloud-init inside the guest self-configures on first boot (user, SSH keys,
// packages, etc.). Requires genisoimage on the host.
type CloudInitConfig struct {
	UserData string `json:"user_data"`          // #cloud-config YAML
	Hostname string `json:"hostname,omitempty"` // meta-data local-hostname (defaults to VM name)
}

// ISOEntry is one installer/boot image available to attach to a VM.
type ISOEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

// Domain is the summary representation returned by list/get.
type Domain struct {
	Name       string            `json:"name"`
	UUID       string            `json:"uuid"`
	ID         int32             `json:"id"`
	Status     DomainStatusValue `json:"status"`
	MemoryMiB  uint64            `json:"memory"`
	Vcpu       uint16            `json:"vcpu"`
	Autostart  bool              `json:"autostart"`
	Os         *DomainOs         `json:"os,omitempty"`
	Disks      []DomainDisk      `json:"disks,omitempty"`
	Interfaces []DomainInterface `json:"interfaces,omitempty"`
	Graphics   []DomainGraphic   `json:"graphics,omitempty"`
}

// DomainStatus is the payload of GET /domain/:name/status.
type DomainStatus struct {
	Status DomainStatusValue `json:"status"`
	Reason int32             `json:"reason"`
}

// ConsoleInfo is returned by GET /domain/:name/vnc — the console address the
// UI's noVNC/SPICE client connects to.
type ConsoleInfo struct {
	Type     GraphicType `json:"type"`
	Host     string      `json:"host"`
	Port     int         `json:"port"`
	Password string      `json:"password,omitempty"`
}

// ---- Storage --------------------------------------------------------------

// StoragePool mirrors the pool half of the storage API.
type StoragePool struct {
	Name        string `json:"name"`
	UUID        string `json:"uuid"`
	State       string `json:"state"`
	CapacityB   uint64 `json:"capacity"`
	AllocationB uint64 `json:"allocation"`
	AvailableB  uint64 `json:"available"`
}

// StorageVolume mirrors codegen volume shapes.
type StorageVolume struct {
	Name        string `json:"name"`
	Key         string `json:"key"`
	Path        string `json:"path"`
	CapacityB   uint64 `json:"capacity"`
	AllocationB uint64 `json:"allocation"`
}

// ---- Host -----------------------------------------------------------------

// HostInfo mirrors codegen.HostInfo — the hypervisor host summary shown on the
// virtualization dashboard.
type HostInfo struct {
	Hostname       string `json:"hostname"`
	LibvirtVersion string `json:"libvirt_version"`
	Hypervisor     string `json:"hypervisor"`
	CPUModel       string `json:"cpu_model,omitempty"`
	CPUCount       int    `json:"cpu_count"`
	MemoryTotalMiB uint64 `json:"memory_total"`
	Arch           string `json:"arch"`
	KVMEnabled     bool   `json:"kvm_enabled"`
	ActiveDomains  int    `json:"active_domains"`
	TotalDomains   int    `json:"total_domains"`
}
