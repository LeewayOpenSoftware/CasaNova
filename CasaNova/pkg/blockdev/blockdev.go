// Package blockdev is the authoritative inventory of this host's block
// devices, and the authority that decides which of them a destructive
// operation may ever touch.
//
// # WHY THIS EXISTS AS ONE AUTHORITY
//
// Free-space sanitization and the external formatter are both destructive.
// If each discovered devices for itself by parsing lsblk output, they would
// drift — and the two would disagree about which disk is the system disk on
// precisely the day that matters. There is one inventory and one protection
// classifier, and both features consult them.
//
// # IDENTITY IS NOT A DEVICE NAME
//
// /dev/sdb is not an identity. It is a slot: unplug a drive, plug another in,
// and the name is reused. A destructive operation prepared against "sdb" and
// executed a minute later can land on a different physical device. So an
// operation binds to a DeviceIdentity — built from the facts that travel with
// the hardware (WWN, serial, model, size, sysfs path, major:minor) — and that
// identity is re-derived and compared immediately before anything destructive
// happens. If it changed, the operation aborts.
//
// # PROTECTION PROPAGATES UPWARD
//
// Protection is decided from topology, never from name prefixes. If /dev/sda2
// carries the root filesystem, then sda2 is protected AND so is /dev/sda —
// which means /dev/sda1 is not offered as a format target merely because it is
// not itself the root partition. Wiping the partition table of the parent
// destroys the root filesystem just as effectively as wiping the partition.
//
// # FALSE NEGATIVES BEAT WRONG DISKS
//
// Every failure to determine something — an unreadable sysfs attribute, a
// mount table that will not parse, a holder chain that cannot be walked — makes
// a device PROTECTED, not eligible. Refusing to format a perfectly good USB
// stick is an inconvenience. Formatting the disk holding 5.4 TB of user data is
// not recoverable.
package blockdev

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Transport is how a device is attached. It matters because "removable" is not
// a reliable answer: most USB-to-SATA and USB-to-NVMe bridges present the disk
// with removable=0, so a formatter that trusted that flag would refuse every
// external SSD enclosure people actually own.
type Transport string

const (
	TransportUSB     Transport = "usb"
	TransportSATA    Transport = "sata"
	TransportNVMe    Transport = "nvme"
	TransportMMC     Transport = "mmc"
	TransportSCSI    Transport = "scsi"
	TransportVirtual Transport = "virtual"
	TransportLoop    Transport = "loop"
	TransportUnknown Transport = "unknown"
)

// DeviceIdentity is what an operation binds to. It deliberately excludes the
// kernel name: two identities are equal when they describe the same physical
// device, whatever it is currently called.
type DeviceIdentity struct {
	// WWN and Serial are the strongest facts, when the device supplies them.
	WWN    string `json:"wwn,omitempty"`
	Serial string `json:"serial,omitempty"`
	// Model and Vendor are weaker but present on almost everything.
	Model  string `json:"model,omitempty"`
	Vendor string `json:"vendor,omitempty"`
	// SizeBytes catches the "same model, different disk" case cheaply.
	SizeBytes uint64 `json:"size_bytes"`
	// SysfsPath is the kernel's own topology path, which survives a rename
	// within one boot but not across a replug.
	SysfsPath string `json:"sysfs_path"`
	// Major/Minor identify the device node at discovery time.
	Major uint32 `json:"major"`
	Minor uint32 `json:"minor"`
	// PartitionUUIDs pins a partitioned disk to its exact table.
	PartitionUUIDs []string `json:"partition_uuids,omitempty"`
}

// Strength reports how confidently this identity distinguishes one physical
// device from another. An operation refuses on Weak: if CasaNova cannot say
// WHICH device this is, it must not destroy it.
type Strength string

const (
	// StrengthStrong: a WWN or serial is present. A replug is detectable.
	StrengthStrong Strength = "strong"
	// StrengthModerate: no serial, but model+size+topology agree. A replug
	// with an identical model and size would not be distinguished, which is
	// unlikely but not impossible.
	StrengthModerate Strength = "moderate"
	// StrengthWeak: too little to bind an operation to. Refused.
	StrengthWeak Strength = "weak"
)

// Strength classifies the identity.
func (id DeviceIdentity) Strength() Strength {
	switch {
	case strings.TrimSpace(id.WWN) != "" || strings.TrimSpace(id.Serial) != "":
		return StrengthStrong
	case strings.TrimSpace(id.Model) != "" && id.SizeBytes > 0 && id.SysfsPath != "" &&
		len(id.PartitionUUIDs) > 0:
		// Model, size and topology alone do NOT distinguish two identical
		// devices in the same port, which is exactly the swap this identity
		// exists to detect: cheap USB sticks and card readers routinely report
		// no serial and no WWN. The partition-table UUIDs are the only thing
		// that travels with the DATA, so without them there is nothing to
		// compare and the honest answer is "I cannot tell".
		return StrengthModerate
	default:
		return StrengthWeak
	}
}

// Equal reports whether two identities describe the same physical device.
//
// A strong identity compares on WWN/serial and size: the kernel name and even
// the sysfs path may legitimately change (a replug into another port) without
// the device changing. A moderate identity has to fall back on model, size and
// topology together, and says so through Strength so callers can require more.
func (id DeviceIdentity) Equal(other DeviceIdentity) bool {
	if id.Strength() == StrengthWeak || other.Strength() == StrengthWeak {
		return false // never claim two unidentifiable devices are the same one
	}
	if id.SizeBytes != other.SizeBytes {
		return false
	}
	if w := strings.TrimSpace(id.WWN); w != "" || strings.TrimSpace(other.WWN) != "" {
		return strings.EqualFold(w, strings.TrimSpace(other.WWN))
	}
	if s := strings.TrimSpace(id.Serial); s != "" || strings.TrimSpace(other.Serial) != "" {
		return strings.EqualFold(s, strings.TrimSpace(other.Serial))
	}
	return strings.EqualFold(id.Model, other.Model) &&
		strings.EqualFold(id.Vendor, other.Vendor) &&
		id.SysfsPath == other.SysfsPath &&
		equalStrings(id.PartitionUUIDs, other.PartitionUUIDs)
}

// Fingerprint is a stable string form for logs, audit records and lock keys.
func (id DeviceIdentity) Fingerprint() string {
	switch {
	case strings.TrimSpace(id.WWN) != "":
		return "wwn:" + strings.ToLower(strings.TrimSpace(id.WWN))
	case strings.TrimSpace(id.Serial) != "":
		return "serial:" + strings.ToLower(strings.TrimSpace(id.Serial))
	default:
		return fmt.Sprintf("topo:%s:%d", id.SysfsPath, id.SizeBytes)
	}
}

// ProtectionReason names why a device may not be destroyed. It is
// machine-readable so a UI can explain the refusal rather than showing a
// disabled control with no cause.
type ProtectionReason string

const (
	ReasonRootFilesystem  ProtectionReason = "ROOT_FILESYSTEM"
	ReasonBootFilesystem  ProtectionReason = "BOOT_FILESYSTEM"
	ReasonCasaNovaState   ProtectionReason = "CASANOVA_STATE"
	ReasonMountedSystem   ProtectionReason = "MOUNTED_SYSTEM_FILESYSTEM"
	ReasonActiveSwap      ProtectionReason = "ACTIVE_SWAP"
	ReasonHasHolders      ProtectionReason = "IN_USE_BY_LVM_RAID_OR_CRYPT"
	ReasonParentProtected ProtectionReason = "PARENT_DISK_PROTECTED"
	ReasonChildProtected  ProtectionReason = "CONTAINS_A_PROTECTED_PARTITION"
	ReasonReadOnly        ProtectionReason = "READ_ONLY"
	ReasonUnidentifiable  ProtectionReason = "CANNOT_BE_IDENTIFIED"
	ReasonDiscoveryFailed ProtectionReason = "DISCOVERY_INCOMPLETE"
	ReasonManagedStorage  ProtectionReason = "CASANOVA_MANAGED_STORAGE"
)

// Device is one block device: a whole disk or a partition.
type Device struct {
	Name      string         `json:"name"` // kernel name, for display only
	Path      string         `json:"path"` // /dev/<name>, resolved at use time
	Identity  DeviceIdentity `json:"identity"`
	Transport Transport      `json:"transport"`

	IsPartition bool     `json:"is_partition"`
	Parent      string   `json:"parent,omitempty"`
	Children    []string `json:"children,omitempty"`

	Removable  bool   `json:"removable"`
	Rotational bool   `json:"rotational"`
	ReadOnly   bool   `json:"read_only"`
	SizeBytes  uint64 `json:"size_bytes"`

	Filesystem  string   `json:"filesystem,omitempty"`
	Label       string   `json:"label,omitempty"`
	UUID        string   `json:"uuid,omitempty"`
	Mountpoints []string `json:"mountpoints,omitempty"`
	Holders     []string `json:"holders,omitempty"`

	// SupportsDiscard reports whether the block stack advertises discard.
	// Advertised, not proven: what the controller does with the deallocated
	// blocks is device firmware's business, and this package does not claim
	// otherwise.
	SupportsDiscard bool `json:"supports_discard"`

	// Protected and ProtectionReasons are the safety verdict. Empty reasons
	// with Protected=false is the ONLY eligible state.
	Protected         bool               `json:"protected"`
	ProtectionReasons []ProtectionReason `json:"protection_reasons,omitempty"`
}

// Eligible reports whether this device may be the target of a destructive
// external-storage operation.
//
// External-or-removable is required deliberately: internal disks do not become
// format targets by accident. An "advanced internal disk management" feature,
// if ever wanted, is a separate authority level with its own confirmation.
func (d Device) Eligible() bool {
	return !d.Protected &&
		!d.ReadOnly &&
		d.Identity.Strength() != StrengthWeak &&
		d.External() &&
		d.SizeBytes > 0
}

// External reports whether the device is attached in a way that makes it user
// storage rather than part of the appliance.
//
// removable=1 alone is not the test: most USB-to-SATA and USB-to-NVMe bridges
// report removable=0, so trusting the flag would refuse every external SSD
// enclosure. Transport is the reliable fact.
func (d Device) External() bool {
	switch d.Transport {
	case TransportUSB, TransportMMC:
		return true
	case TransportLoop:
		// Loop devices are how the automated test lab exercises this code
		// without hardware. They are never system storage.
		return true
	default:
		return d.Removable
	}
}

// Inventory is a point-in-time snapshot of every block device.
type Inventory struct {
	Devices    []Device  `json:"devices"`
	TakenAt    time.Time `json:"taken_at"`
	Complete   bool      `json:"complete"`
	Incomplete []string  `json:"incomplete,omitempty"`
}

// ByFingerprint finds a device by its identity fingerprint.
func (inv Inventory) ByFingerprint(fp string) (Device, bool) {
	for _, d := range inv.Devices {
		if d.Identity.Fingerprint() == fp {
			return d, true
		}
	}
	return Device{}, false
}

// ByName finds a device by kernel name. For display and diagnostics only —
// never bind an operation to the result.
func (inv Inventory) ByName(name string) (Device, bool) {
	for _, d := range inv.Devices {
		if d.Name == name {
			return d, true
		}
	}
	return Device{}, false
}

// Eligible returns the devices a destructive operation may target.
func (inv Inventory) Eligible() []Device {
	var out []Device
	for _, d := range inv.Devices {
		if d.Eligible() {
			out = append(out, d)
		}
	}
	return out
}

// sysfsRoot and procRoot are variables so the loop-device test lab can point
// the inventory at a fixture tree without hardware.
var (
	sysfsRoot = "/sys"
	procRoot  = "/proc"
	devRoot   = "/dev"
	rootsMu   sync.RWMutex
)

// SetRoots redirects discovery, for tests and for fixtures.
func SetRoots(sys, proc, dev string) {
	rootsMu.Lock()
	defer rootsMu.Unlock()
	sysfsRoot, procRoot, devRoot = sys, proc, dev
}

func roots() (string, string, string) {
	rootsMu.RLock()
	defer rootsMu.RUnlock()
	return sysfsRoot, procRoot, devRoot
}

// Discover builds the inventory from sysfs and proc.
//
// sysfs, not lsblk: parsing another program's output adds a dependency, a
// format that changes between distributions, and a failure mode where the tool
// is missing. The kernel's own attributes are the source of truth, and reading
// them directly means a missing attribute is visible as a missing attribute
// rather than as an empty column.
//
// Any device whose facts cannot be established is marked PROTECTED with
// ReasonDiscoveryFailed. A destructive feature must never treat "I could not
// tell" as "safe to destroy".
func Discover() (Inventory, error) {
	sys, prc, dev := roots()
	inv := Inventory{TakenAt: time.Now(), Complete: true}

	entries, err := os.ReadDir(filepath.Join(sys, "block"))
	if err != nil {
		return inv, fmt.Errorf("read %s/block: %w", sys, err)
	}

	partUUIDs, puErr := readPartitionUUIDs(dev)
	if puErr != nil {
		inv.Complete = false
		inv.Incomplete = append(inv.Incomplete, "partition UUIDs: "+puErr.Error())
	}

	mounts, mErr := readMounts(prc)
	if mErr != nil {
		inv.Complete = false
		inv.Incomplete = append(inv.Incomplete, "mount table: "+mErr.Error())
	}
	swaps, sErr := readSwaps(prc)
	if sErr != nil {
		inv.Complete = false
		inv.Incomplete = append(inv.Incomplete, "swap table: "+sErr.Error())
	}

	byName := map[string]*Device{}
	for _, e := range entries {
		name := e.Name()
		base := filepath.Join(sys, "block", name)

		d := &Device{Name: name, Path: filepath.Join(dev, name)}
		if err := readDisk(base, d); err != nil {
			d.Protected = true
			d.ProtectionReasons = append(d.ProtectionReasons, ReasonDiscoveryFailed)
			inv.Complete = false
			inv.Incomplete = append(inv.Incomplete, name+": "+err.Error())
		}
		byName[name] = d

		// Partitions live as subdirectories carrying their own "partition"
		// attribute.
		parts, _ := os.ReadDir(base)
		for _, pe := range parts {
			if !pe.IsDir() || !strings.HasPrefix(pe.Name(), name) {
				continue
			}
			pbase := filepath.Join(base, pe.Name())
			if _, err := os.Stat(filepath.Join(pbase, "partition")); err != nil {
				continue
			}
			p := &Device{
				Name: pe.Name(), Path: filepath.Join(dev, pe.Name()),
				IsPartition: true, Parent: name,
			}
			if err := readPartition(pbase, d, p); err != nil {
				p.Protected = true
				p.ProtectionReasons = append(p.ProtectionReasons, ReasonDiscoveryFailed)
				inv.Complete = false
				inv.Incomplete = append(inv.Incomplete, pe.Name()+": "+err.Error())
			}
			if u := partUUIDs[p.Name]; u != "" {
				p.Identity.PartitionUUIDs = []string{u}
				d.Identity.PartitionUUIDs = append(d.Identity.PartitionUUIDs, u)
			}
			d.Children = append(d.Children, p.Name)
			byName[p.Name] = p
		}
	}

	// Attach mounts, swap and holders, then classify.
	for name, d := range byName {
		d.Mountpoints = mounts[name]
		if swaps[name] {
			d.Protected = true
			d.ProtectionReasons = appendReason(d.ProtectionReasons, ReasonActiveSwap)
		}
	}
	classify(byName)

	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		inv.Devices = append(inv.Devices, *byName[n])
	}
	return inv, nil
}

// DefaultProtectedMountPrefixes is the production set of mountpoints whose
// backing device must never be destroyed. Matching is on the mountpoint
// itself and on anything beneath it. Exported as the single source of truth
// for the default — a test that overrides SetProtectedMountPrefixes restores
// this value rather than retyping the list, and TestDefaultProtectedMountPrefixesCoverTheCriticalPaths
// pins its contents directly.
var DefaultProtectedMountPrefixes = []string{
	"/", "/boot", "/boot/efi", "/efi",
	"/usr", "/var", "/etc", "/home",
	"/var/lib/casanova", "/var/lib/casaos",
	"/var/lib/docker", "/var/lib/containers",
}

var (
	protectedMountPrefixes   = DefaultProtectedMountPrefixes
	protectedMountPrefixesMu sync.RWMutex
)

// SetProtectedMountPrefixes redirects the protected-mount list, for tests and
// fixtures — the same seam sysfsRoot/procRoot/devRoot already have via
// SetRoots. A fixture whose fake device is backed by a real t.TempDir() (for
// real file I/O) needs this: t.TempDir() resolves under $TMPDIR, and if
// $TMPDIR happens to sit under one of the default prefixes (e.g. "/home"),
// the fixture's own scratch directory collides with a real production
// safety path it has nothing to do with.
func SetProtectedMountPrefixes(prefixes []string) {
	protectedMountPrefixesMu.Lock()
	defer protectedMountPrefixesMu.Unlock()
	protectedMountPrefixes = prefixes
}

func currentProtectedMountPrefixes() []string {
	protectedMountPrefixesMu.RLock()
	defer protectedMountPrefixesMu.RUnlock()
	return protectedMountPrefixes
}

// classify decides protection, then propagates it through the topology in both
// directions.
func classify(byName map[string]*Device) {
	for _, d := range byName {
		for _, mp := range d.Mountpoints {
			if isProtectedMount(mp) {
				d.Protected = true
				switch {
				case mp == "/":
					d.ProtectionReasons = appendReason(d.ProtectionReasons, ReasonRootFilesystem)
				case strings.HasPrefix(mp, "/boot") || strings.HasPrefix(mp, "/efi"):
					d.ProtectionReasons = appendReason(d.ProtectionReasons, ReasonBootFilesystem)
				case strings.Contains(mp, "casanova") || strings.Contains(mp, "casaos"):
					d.ProtectionReasons = appendReason(d.ProtectionReasons, ReasonCasaNovaState)
				default:
					d.ProtectionReasons = appendReason(d.ProtectionReasons, ReasonMountedSystem)
				}
			}
		}
		// A device consumed by LVM, mdraid or dm-crypt has holders. Destroying
		// it destroys whatever is layered on top, which discovery cannot see
		// through — so holders alone are disqualifying.
		if len(d.Holders) > 0 {
			d.Protected = true
			d.ProtectionReasons = appendReason(d.ProtectionReasons, ReasonHasHolders)
		}
		if d.ReadOnly {
			d.Protected = true
			d.ProtectionReasons = appendReason(d.ProtectionReasons, ReasonReadOnly)
		}
		if d.Identity.Strength() == StrengthWeak {
			d.Protected = true
			d.ProtectionReasons = appendReason(d.ProtectionReasons, ReasonUnidentifiable)
		}
	}

	// UPWARD: a protected partition protects its whole disk. Wiping the parent's
	// partition table destroys the child just as thoroughly, so offering
	// /dev/sda1 because only /dev/sda2 is root would be the same mistake with
	// extra steps.
	//
	// DOWNWARD: a protected disk protects every partition on it.
	//
	// Fixed-point iteration, because a chain (partition -> disk -> sibling)
	// needs more than one pass, and the topology is far too small for the cost
	// to matter.
	for i := 0; i < 8; i++ {
		changed := false
		for _, d := range byName {
			if !d.Protected {
				continue
			}
			if d.IsPartition {
				if parent, ok := byName[d.Parent]; ok && !parent.Protected {
					parent.Protected = true
					parent.ProtectionReasons = appendReason(parent.ProtectionReasons, ReasonChildProtected)
					changed = true
				}
			}
			for _, c := range d.Children {
				if child, ok := byName[c]; ok && !child.Protected {
					child.Protected = true
					child.ProtectionReasons = appendReason(child.ProtectionReasons, ReasonParentProtected)
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
}

func isProtectedMount(mp string) bool {
	if mp == "" {
		return false
	}
	for _, p := range currentProtectedMountPrefixes() {
		if mp == p {
			return true
		}
		if p != "/" && strings.HasPrefix(mp, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

func appendReason(list []ProtectionReason, r ProtectionReason) []ProtectionReason {
	for _, x := range list {
		if x == r {
			return list
		}
	}
	return append(list, r)
}

// ---------------------------------------------------------------- sysfs reads

func readDisk(base string, d *Device) error {
	var missing []string

	if v, err := readUint(filepath.Join(base, "size")); err == nil {
		d.SizeBytes = v * 512 // sysfs reports 512-byte sectors regardless of physical block size
	} else {
		missing = append(missing, "size")
	}
	d.Removable, _ = readBool(filepath.Join(base, "removable"))
	d.Rotational, _ = readBool(filepath.Join(base, "queue/rotational"))
	d.ReadOnly, _ = readBool(filepath.Join(base, "ro"))

	if v, err := readUint(filepath.Join(base, "queue/discard_max_bytes")); err == nil && v > 0 {
		d.SupportsDiscard = true
	}

	maj, min, err := readDevNum(filepath.Join(base, "dev"))
	if err != nil {
		missing = append(missing, "dev")
	}

	model := readTrimmed(filepath.Join(base, "device/model"))
	vendor := readTrimmed(filepath.Join(base, "device/vendor"))
	serial := firstNonEmpty(
		readTrimmed(filepath.Join(base, "device/serial")),
		readTrimmed(filepath.Join(base, "serial")),
	)
	wwn := firstNonEmpty(
		readTrimmed(filepath.Join(base, "wwid")),
		readTrimmed(filepath.Join(base, "device/wwid")),
	)

	real, rerr := filepath.EvalSymlinks(base)
	if rerr != nil {
		real = base
		missing = append(missing, "topology")
	}

	d.Transport = transportOf(real, d.Name)
	d.Identity = DeviceIdentity{
		WWN: wwn, Serial: serial, Model: model, Vendor: vendor,
		SizeBytes: d.SizeBytes, SysfsPath: real, Major: maj, Minor: min,
	}

	holders, _ := os.ReadDir(filepath.Join(base, "holders"))
	for _, h := range holders {
		d.Holders = append(d.Holders, h.Name())
	}

	if len(missing) > 0 {
		return fmt.Errorf("unreadable attributes: %s", strings.Join(missing, ", "))
	}
	return nil
}

func readPartition(pbase string, parent *Device, p *Device) error {
	var missing []string
	if v, err := readUint(filepath.Join(pbase, "size")); err == nil {
		p.SizeBytes = v * 512
	} else {
		missing = append(missing, "size")
	}
	p.ReadOnly, _ = readBool(filepath.Join(pbase, "ro"))
	maj, min, err := readDevNum(filepath.Join(pbase, "dev"))
	if err != nil {
		missing = append(missing, "dev")
	}
	real, rerr := filepath.EvalSymlinks(pbase)
	if rerr != nil {
		real = pbase
		missing = append(missing, "topology")
	}

	// A partition inherits its transport and hardware facts from its disk;
	// its own identity adds the partition's position and size.
	p.Transport = parent.Transport
	p.Removable = parent.Removable
	p.Rotational = parent.Rotational
	p.SupportsDiscard = parent.SupportsDiscard
	p.Identity = DeviceIdentity{
		WWN: parent.Identity.WWN, Serial: parent.Identity.Serial,
		Model: parent.Identity.Model, Vendor: parent.Identity.Vendor,
		SizeBytes: p.SizeBytes, SysfsPath: real, Major: maj, Minor: min,
	}

	holders, _ := os.ReadDir(filepath.Join(pbase, "holders"))
	for _, h := range holders {
		p.Holders = append(p.Holders, h.Name())
	}
	if len(missing) > 0 {
		return fmt.Errorf("unreadable attributes: %s", strings.Join(missing, ", "))
	}
	return nil
}

// transportOf derives attachment from the device's real sysfs topology path,
// which names the bus it hangs off.
func transportOf(realPath, name string) Transport {
	p := strings.ToLower(realPath)
	switch {
	case strings.HasPrefix(name, "loop"):
		return TransportLoop
	case strings.Contains(p, "/usb"):
		return TransportUSB
	case strings.HasPrefix(name, "nvme") || strings.Contains(p, "/nvme"):
		return TransportNVMe
	case strings.HasPrefix(name, "mmcblk") || strings.Contains(p, "/mmc"):
		return TransportMMC
	case strings.Contains(p, "/ata"):
		return TransportSATA
	case strings.Contains(p, "/virtio") || strings.HasPrefix(name, "vd"):
		return TransportVirtual
	case strings.Contains(p, "/scsi") || strings.HasPrefix(name, "sd"):
		return TransportSCSI
	default:
		return TransportUnknown
	}
}

// ---------------------------------------------------------------- proc reads

// readMounts maps a kernel device name to the mountpoints it serves.
func readMounts(prc string) (map[string][]string, error) {
	f, err := os.Open(filepath.Join(prc, "self", "mountinfo"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string][]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+3:])
		if len(left) < 5 || len(right) < 2 {
			continue
		}
		mp := unescapeMountinfo(left[4])
		source := right[1]
		if !strings.HasPrefix(source, "/dev/") {
			continue
		}
		name := strings.TrimPrefix(source, "/dev/")
		out[name] = append(out[name], mp)
	}
	return out, sc.Err()
}

// readPartitionUUIDs maps a kernel partition name to its partition-table UUID,
// using udev's /dev/disk/by-partuuid symlink farm.
//
// The farm, not an external tool: it is kernel/udev state like everything else
// this package reads, so a missing entry is visible as a missing entry rather
// than as a parsing difference between distributions.
//
// A device with no partition table has no entries, which is correct — there is
// no table to pin. Its identity then falls back to WWN or serial, and if it has
// neither, Strength() reports weak and every destructive operation refuses it.
func readPartitionUUIDs(dev string) (map[string]string, error) {
	dir := filepath.Join(dev, "disk", "by-partuuid")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No farm on this host (or a fixture tree without one). Not an
			// error: identities simply lose this discriminator, and Strength()
			// downgrades accordingly rather than pretending to match.
			return map[string]string{}, nil
		}
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		target, rerr := os.Readlink(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		out[filepath.Base(target)] = e.Name()
	}
	return out, nil
}

func readSwaps(prc string) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(prc, "swaps"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	out := map[string]bool{}
	for i, line := range strings.Split(string(data), "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}
		out[strings.TrimPrefix(fields[0], "/dev/")] = true
	}
	return out, nil
}

func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			v, ok := 0, true
			for _, c := range s[i+1 : i+4] {
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + int(c-'0')
			}
			if ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---------------------------------------------------------------- small reads

func readTrimmed(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readUint(p string) (uint64, error) {
	s := readTrimmed(p)
	if s == "" {
		return 0, errors.New("empty")
	}
	return strconv.ParseUint(s, 10, 64)
}

func readBool(p string) (bool, error) {
	v, err := readUint(p)
	return v == 1, err
}

func readDevNum(p string) (uint32, uint32, error) {
	s := readTrimmed(p)
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("malformed dev number %q", s)
	}
	maj, err1 := strconv.ParseUint(parts[0], 10, 32)
	min, err2 := strconv.ParseUint(parts[1], 10, 32)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("malformed dev number %q", s)
	}
	return uint32(maj), uint32(min), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
