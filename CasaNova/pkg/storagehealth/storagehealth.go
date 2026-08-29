// Package storagehealth validates that the destructive storage-maintenance
// subsystem's SAFETY MACHINERY still works, without destroying anything.
//
// # WHY A SEPARATE PACKAGE
//
// The same report is served by the HTTP surface (route/v1) and by the
// diagnostics engine (pkg/diagnosis). route/v1 already imports pkg/diagnosis,
// so the evaluator cannot live in route/v1 without an import cycle, and it
// cannot live in pkg/storagemaint without colliding with the format and
// sanitize implementations being written alongside it.
//
// # WHAT "HEALTHY" MEANS HERE
//
// Not "the functions exist". A self-test that calls a protection classifier and
// checks it returned something would pass just as happily with the classifier
// replaced by `return false`. Every invariant below is written as a NEGATIVE
// BOUNDARY: the check that must FAIL if the safety property is gone.
//
// The load-bearing one is protected_device_detection. It finds the device
// backing THIS host's root filesystem by reading the mount table directly —
// independently of blockdev, so the classifier is cross-examined by a second
// reader rather than asked to grade itself — and then asserts that blockdev
// reports that exact device as protected and absent from the eligible set. If
// protection ever stops working, the root disk of the box running the check
// becomes eligible for formatting, and this invariant is what says so.
//
// # WHAT IS NEVER DONE
//
// Nothing here writes to a block device, opens one for writing, unmounts
// anything, or runs a mkfs/discard helper — not even against a device it
// believes is disposable. The only write is a probe file inside the
// subsystem's own state directory. There is no non-destructive way to prove a
// formatter formats, so this package does not claim to have proven it: it
// proves the guards that decide WHETHER to format, and says so.
package storagehealth

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/blockdev"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/storagemaint"
)

// State is the machine-readable verdict for one invariant and for the report.
type State string

const (
	// StateHealthy: the invariant was actively demonstrated.
	StateHealthy State = "healthy"
	// StateDegraded: the subsystem still refuses correctly, but something is
	// missing or could not be demonstrated. Destructive work may be refused.
	StateDegraded State = "degraded"
	// StateUnavailable: a prerequisite is absent, so nothing can be validated.
	StateUnavailable State = "unavailable"
	// StateValidationFailed: an invariant was demonstrated to be BROKEN. This
	// is the alarm state — a safety guard did not hold.
	StateValidationFailed State = "validation_failed"
)

// severity orders the states so a report can take the worst one. Note that
// validation_failed outranks unavailable: "the guard is broken" is worse news
// than "the guard could not be reached".
func severity(s State) int {
	switch s {
	case StateHealthy:
		return 0
	case StateDegraded:
		return 1
	case StateUnavailable:
		return 2
	case StateValidationFailed:
		return 3
	}
	return 3
}

// Invariant names of the subsystem. Stable strings: a UI, an alert rule and a
// support transcript all key on these, so they are part of the contract.
const (
	InvTopologyDiscovery  = "topology_discovery"
	InvDiscoveryComplete  = "discovery_complete"
	InvProtectedDetection = "protected_device_detection"
	InvStableIdentity     = "stable_identity_generation"
	InvMountHolder        = "mount_and_holder_inspection"
	InvFilesystemHelpers  = "filesystem_helpers_present"
	InvDiscardDetection   = "discard_detection"
	InvJobStatePersist    = "job_state_persistence"
)

// Check is one validated invariant.
type Check struct {
	Invariant string `json:"invariant"`
	Title     string `json:"title"`
	State     State  `json:"state"`
	// Detail says what was actually demonstrated, in terms of the boundary
	// that was tested — not "ok".
	Detail string `json:"detail"`
	// ActionableCause is what an operator can do about it. Empty when healthy.
	ActionableCause string `json:"actionable_cause,omitempty"`
}

// Report is the subsystem's health.
type Report struct {
	State State `json:"state"`
	// Invariant names the FAILING invariant that set State, empty when healthy.
	Invariant       string `json:"invariant,omitempty"`
	ActionableCause string `json:"actionable_cause,omitempty"`
	// DestructiveOperationsAllowed is the consequence an operator actually
	// cares about: whether a format or sanitize would be accepted right now.
	DestructiveOperationsAllowed bool      `json:"destructive_operations_allowed"`
	Checks                       []Check   `json:"checks"`
	HeldDevices                  int       `json:"held_device_locks"`
	CheckedAt                    time.Time `json:"checked_at"`
	// Note is a standing honesty statement about what this report does and
	// does not prove. It is part of the payload so it cannot be dropped by a
	// UI that only renders the state.
	Note string `json:"note"`
}

const reportNote = "This report validates the guards that decide whether a device may be " +
	"destroyed — topology discovery, protection classification, stable identity, mount and " +
	"holder inspection, helper availability and job-state durability. It does not, and cannot " +
	"without destroying data, prove that a format or a sanitize run completes correctly."

// roots mirror blockdev's, so a fixture tree can be validated end to end. They
// are separate variables because blockdev does not expose the ones it holds;
// SetRoots here MUST be called together with blockdev.SetRoots or the two
// readers will disagree, which is precisely the disagreement this package is
// built to detect.
var (
	rootsMu  sync.RWMutex
	sysfsDir = "/sys"
	procDir  = "/proc"
)

// SetRoots redirects the independent mount-table reader. Tests only.
func SetRoots(sys, proc string) {
	rootsMu.Lock()
	defer rootsMu.Unlock()
	sysfsDir, procDir = sys, proc
}

func roots() (string, string) {
	rootsMu.RLock()
	defer rootsMu.RUnlock()
	return sysfsDir, procDir
}

// Evaluate runs every non-destructive validation and returns the report.
//
// It never returns an error: an evaluator that can fail to produce a verdict
// gives its caller a third outcome to forget about. A failure to evaluate IS a
// verdict, and it is not "healthy".
func Evaluate(ctx context.Context) Report {
	rep := Report{CheckedAt: time.Now(), State: StateHealthy, Note: reportNote}

	inv, discErr := blockdev.Discover()

	rep.Checks = append(rep.Checks,
		checkTopologyDiscovery(inv, discErr),
		checkDiscoveryComplete(inv, discErr),
		checkProtectedDetection(inv, discErr),
		checkStableIdentity(inv, discErr),
		checkMountAndHolders(inv, discErr),
		checkFilesystemHelpers(ctx),
		checkDiscardDetection(ctx, inv, discErr),
		checkJobStatePersistence(),
	)

	for _, c := range rep.Checks {
		if severity(c.State) > severity(rep.State) {
			rep.State = c.State
			rep.Invariant = c.Invariant
			rep.ActionableCause = c.ActionableCause
		}
	}
	rep.HeldDevices = len(storagemaint.HeldDevices())
	// Destructive work is allowed only from a fully healthy subsystem. Degraded
	// is not "mostly fine": every degraded case above is one where CasaNova
	// cannot prove something it relies on to pick the right disk.
	rep.DestructiveOperationsAllowed = rep.State == StateHealthy
	return rep
}

// ---------------------------------------------------------------- invariants

func checkTopologyDiscovery(inv blockdev.Inventory, err error) Check {
	c := Check{Invariant: InvTopologyDiscovery, Title: "Block-device topology discovery"}
	if err != nil {
		c.State = StateUnavailable
		c.Detail = "sysfs could not be enumerated: " + err.Error()
		c.ActionableCause = "CasaNova cannot see the block devices at all. Check that /sys is mounted " +
			"and that the service is not running in a namespace without it."
		return c
	}
	if len(inv.Devices) == 0 {
		c.State = StateUnavailable
		c.Detail = "sysfs was readable but reported no block devices, which cannot be true of a booted host"
		c.ActionableCause = "Check that /sys/block is populated; an empty tree means discovery is looking " +
			"at the wrong root."
		return c
	}
	var disks, parts int
	for _, d := range inv.Devices {
		if d.IsPartition {
			parts++
		} else {
			disks++
		}
	}
	c.State = StateHealthy
	c.Detail = fmt.Sprintf("%d whole device(s) and %d partition(s) enumerated from sysfs, "+
		"with parent/child links resolved", disks, parts)
	return c
}

func checkDiscoveryComplete(inv blockdev.Inventory, err error) Check {
	c := Check{Invariant: InvDiscoveryComplete, Title: "Discovery completeness"}
	if err != nil {
		c.State = StateUnavailable
		c.Detail = "discovery did not run: " + err.Error()
		c.ActionableCause = "See " + InvTopologyDiscovery + "."
		return c
	}
	if !inv.Complete {
		// Not merely cosmetic. An unreadable mount table makes EVERY device
		// look unmounted, which is the one way the root disk could pass the
		// protection classifier. Destructive work is refused wholesale while
		// this is true.
		c.State = StateDegraded
		c.Detail = "discovery reported missing facts: " + strings.Join(inv.Incomplete, "; ")
		c.ActionableCause = "Destructive storage operations are being refused because a device fact " +
			"could not be read. An unreadable mount table in particular would make a mounted system " +
			"disk look free. Resolve the listed read failures."
		return c
	}
	c.State = StateHealthy
	c.Detail = "every device fact discovery needs was readable; no device fell back to the " +
		"protected-because-unknown path"
	return c
}

// checkProtectedDetection is the negative boundary: it proves the classifier
// still refuses THIS host's own root disk.
func checkProtectedDetection(inv blockdev.Inventory, err error) Check {
	c := Check{Invariant: InvProtectedDetection, Title: "Protected-device detection"}
	if err != nil {
		c.State = StateUnavailable
		c.Detail = "discovery did not run, so protection could not be exercised: " + err.Error()
		c.ActionableCause = "See " + InvTopologyDiscovery + "."
		return c
	}

	_, prc := roots()
	source, srcErr := rootFilesystemSource(prc)
	if srcErr != nil {
		c.State = StateDegraded
		c.Detail = "the mount table could not be read independently: " + srcErr.Error()
		c.ActionableCause = "Protection cannot be cross-checked while /proc/self/mountinfo is " +
			"unreadable, so destructive operations are refused."
		return c
	}
	if source == "" {
		c.State = StateDegraded
		c.Detail = "the mount table names no /dev source for /, so there is no root disk to test the " +
			"classifier against"
		c.ActionableCause = "This host's root filesystem is not backed by a block device this check can " +
			"name (overlay, NFS or a container root). The classifier cannot be demonstrated here."
		return c
	}

	name := strings.TrimPrefix(source, "/dev/")
	dev, found := inv.ByName(name)
	if !found {
		// LVM, mdraid or dm-crypt: the root filesystem sits on a mapper node
		// that is not itself in /sys/block. The equivalent boundary is that
		// every device something else is layered on top of must be protected.
		return checkHolderChainProtection(inv, source)
	}

	if !dev.Protected {
		c.State = StateValidationFailed
		c.Detail = fmt.Sprintf("the device backing / (%s) is NOT reported as protected. "+
			"The classifier would offer this host's own system disk as a format target.", source)
		c.ActionableCause = "Do not run any storage-maintenance operation. The protection classifier " +
			"in pkg/blockdev is not classifying the root device; this is a code defect, not a " +
			"configuration problem."
		return c
	}
	// It is not enough that the device is flagged: it must also be absent from
	// the set the API actually offers.
	for _, e := range inv.Eligible() {
		if e.Name == dev.Name {
			c.State = StateValidationFailed
			c.Detail = fmt.Sprintf("the device backing / (%s) is flagged protected but still appears "+
				"in the eligible set", source)
			c.ActionableCause = "Do not run any storage-maintenance operation. Eligible() and the " +
				"protection flag disagree."
			return c
		}
	}
	// Upward propagation: the whole disk carrying the root partition must be
	// protected too, or /dev/sda1 gets offered because only /dev/sda2 is root.
	if dev.IsPartition && dev.Parent != "" {
		parent, ok := inv.ByName(dev.Parent)
		if !ok || !parent.Protected {
			c.State = StateValidationFailed
			c.Detail = fmt.Sprintf("the root partition %s is protected but its whole disk %s is not; "+
				"wiping the disk's partition table would destroy root anyway", source, dev.Parent)
			c.ActionableCause = "Do not run any storage-maintenance operation. Upward protection " +
				"propagation is not being applied."
			return c
		}
	}

	c.State = StateHealthy
	c.Detail = fmt.Sprintf("this host's root device (%s) is classified protected (%s) and is absent "+
		"from the eligible set", source, joinReasons(dev.ProtectionReasons))
	return c
}

// checkHolderChainProtection is the substitute boundary when root sits on a
// device-mapper or md node: every physical member with holders must be
// protected, because discovery cannot see through the layer above it.
func checkHolderChainProtection(inv blockdev.Inventory, source string) Check {
	c := Check{Invariant: InvProtectedDetection, Title: "Protected-device detection"}
	var withHolders, unprotected []string
	for _, d := range inv.Devices {
		if len(d.Holders) == 0 {
			continue
		}
		withHolders = append(withHolders, d.Name)
		if !d.Protected {
			unprotected = append(unprotected, d.Name)
		}
	}
	if len(unprotected) > 0 {
		c.State = StateValidationFailed
		c.Detail = fmt.Sprintf("root is on %s, and these devices carry holders but are not protected: %s",
			source, strings.Join(unprotected, ", "))
		c.ActionableCause = "Do not run any storage-maintenance operation. A device consumed by LVM, " +
			"RAID or dm-crypt is being offered as free."
		return c
	}
	if len(withHolders) == 0 {
		c.State = StateDegraded
		c.Detail = fmt.Sprintf("root is on %s, which is not a device in /sys/block, and no device "+
			"reports holders — so there is nothing to test the classifier against", source)
		c.ActionableCause = "The protection classifier could not be demonstrated on this host. " +
			"Destructive operations are refused rather than trusted."
		return c
	}
	c.State = StateHealthy
	c.Detail = fmt.Sprintf("root is on %s; all %d device(s) carrying holders are classified protected",
		source, len(withHolders))
	return c
}

func checkStableIdentity(inv blockdev.Inventory, err error) Check {
	c := Check{Invariant: InvStableIdentity, Title: "Stable device identity"}
	if err != nil {
		c.State = StateUnavailable
		c.Detail = "discovery did not run: " + err.Error()
		c.ActionableCause = "See " + InvTopologyDiscovery + "."
		return c
	}

	// Fingerprints must be unique among WHOLE devices. Partitions deliberately
	// inherit their disk's serial and WWN — one physical device, one
	// fingerprint, which is what makes the per-device lock correct — so
	// partitions are not part of the uniqueness question.
	seen := map[string]string{}
	var collisions, weakEligible []string
	for _, d := range inv.Devices {
		if d.IsPartition || d.Identity.Strength() == blockdev.StrengthWeak {
			continue
		}
		fp := d.Identity.Fingerprint()
		if prev, dup := seen[fp]; dup {
			collisions = append(collisions, fmt.Sprintf("%s and %s both fingerprint as %q", prev, d.Name, fp))
			continue
		}
		seen[fp] = d.Name
	}
	for _, d := range inv.Eligible() {
		if d.Identity.Strength() == blockdev.StrengthWeak {
			weakEligible = append(weakEligible, d.Name)
		}
	}

	if len(collisions) > 0 {
		c.State = StateValidationFailed
		c.Detail = "two distinct physical devices produce the same identity fingerprint: " +
			strings.Join(collisions, "; ")
		c.ActionableCause = "Do not run any storage-maintenance operation. An operation prepared " +
			"against one of these devices could execute against the other, and the per-device lock " +
			"cannot tell them apart."
		return c
	}
	if len(weakEligible) > 0 {
		c.State = StateValidationFailed
		c.Detail = "device(s) with an unusable identity are being offered as eligible: " +
			strings.Join(weakEligible, ", ")
		c.ActionableCause = "Do not run any storage-maintenance operation. A device CasaNova cannot " +
			"identify must never be a destructive target."
		return c
	}
	c.State = StateHealthy
	c.Detail = fmt.Sprintf("%d whole device(s) produced distinct fingerprints, and no device with a "+
		"weak identity is eligible", len(seen))
	return c
}

func checkMountAndHolders(inv blockdev.Inventory, err error) Check {
	c := Check{Invariant: InvMountHolder, Title: "Mount and holder inspection"}
	if err != nil {
		c.State = StateUnavailable
		c.Detail = "discovery did not run: " + err.Error()
		c.ActionableCause = "See " + InvTopologyDiscovery + "."
		return c
	}
	sys, prc := roots()

	if _, statErr := os.Stat(filepath.Join(prc, "self", "mountinfo")); statErr != nil {
		c.State = StateValidationFailed
		c.Detail = "the mount table is unreadable: " + statErr.Error() +
			" — every device would look unmounted"
		c.ActionableCause = "Destructive operations are refused. Restore access to " +
			filepath.Join(prc, "self", "mountinfo") + "; a service namespace that hides it makes a " +
			"mounted system disk indistinguishable from a free one."
		return c
	}
	if _, statErr := os.Stat(filepath.Join(prc, "swaps")); statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		c.State = StateDegraded
		c.Detail = "the swap table could not be read: " + statErr.Error()
		c.ActionableCause = "An active swap device may not be recognised. Restore access to " +
			filepath.Join(prc, "swaps") + "."
		return c
	}

	// The holders directory is how a device consumed by LVM/RAID/crypt is
	// recognised. If sysfs does not present it, that whole class of protection
	// silently evaluates to "no holders".
	var probed int
	for _, d := range inv.Devices {
		if d.IsPartition {
			continue
		}
		hd := filepath.Join(sys, "block", d.Name, "holders")
		if _, statErr := os.Stat(hd); statErr == nil {
			probed++
		} else if !errors.Is(statErr, os.ErrNotExist) {
			c.State = StateDegraded
			c.Detail = fmt.Sprintf("the holders directory for %s could not be read: %v", d.Name, statErr)
			c.ActionableCause = "Devices consumed by LVM, RAID or dm-crypt may not be recognised as " +
				"in use. Check sysfs permissions."
			return c
		}
	}

	var mounted int
	for _, d := range inv.Devices {
		mounted += len(d.Mountpoints)
	}
	c.State = StateHealthy
	c.Detail = fmt.Sprintf("the mount table parsed and attributed %d mountpoint(s) to devices, and "+
		"the holder directory was readable for %d whole device(s)", mounted, probed)
	return c
}

func checkFilesystemHelpers(ctx context.Context) Check {
	c := Check{Invariant: InvFilesystemHelpers, Title: "Filesystem helper availability"}
	caps := storagemaint.CurrentCapabilities(ctx)

	var have, missing []string
	for _, fs := range storagemaint.SupportedFilesystems {
		if ok, _ := caps.Available(fs); ok {
			have = append(have, string(fs))
		} else {
			missing = append(missing, string(fs))
		}
	}
	sort.Strings(have)
	sort.Strings(missing)

	switch {
	case len(have) == 0:
		c.State = StateUnavailable
		c.Detail = "no mkfs helper is installed, so no filesystem can be created: missing " +
			strings.Join(missing, ", ")
		c.ActionableCause = "Install e2fsprogs (ext4), exfatprogs (exFAT), ntfs-3g (NTFS) and " +
			"dosfstools (FAT32). Until then the format API offers nothing."
	case len(missing) > 0:
		c.State = StateDegraded
		c.Detail = "available: " + strings.Join(have, ", ") + "; unavailable: " + strings.Join(missing, ", ")
		c.ActionableCause = "The unavailable filesystems are not offered by the API rather than " +
			"failing mid-format. Install the packages named in the capabilities endpoint to enable them."
	default:
		c.State = StateHealthy
		c.Detail = "a helper is present for every supported filesystem: " + strings.Join(have, ", ")
	}

	// Signature wiping is not optional: leaving an old filesystem signature
	// behind after a format produces a device two tools disagree about.
	if !caps.CanWipeSignatures && severity(StateDegraded) > severity(c.State) {
		c.State = StateDegraded
		c.Detail += "; wipefs is missing, so stale filesystem signatures cannot be removed"
		c.ActionableCause = "Install util-linux (wipefs)."
	}
	return c
}

func checkDiscardDetection(ctx context.Context, inv blockdev.Inventory, err error) Check {
	c := Check{Invariant: InvDiscardDetection, Title: "Discard (TRIM) detection"}
	if err != nil {
		c.State = StateUnavailable
		c.Detail = "discovery did not run: " + err.Error()
		c.ActionableCause = "See " + InvTopologyDiscovery + "."
		return c
	}
	sys, _ := roots()

	// The property is that the PROBE works, not that some disk supports TRIM:
	// a host of spinning rust legitimately reports discard nowhere. So the
	// boundary is a queue attribute that exists but cannot be read or parsed —
	// which would silently report "no discard" for a device that has it.
	var probed, advertised int
	for _, d := range inv.Devices {
		if d.IsPartition {
			continue
		}
		attr := filepath.Join(sys, "block", d.Name, "queue", "discard_max_bytes")
		raw, readErr := os.ReadFile(attr)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue // legitimately absent for this device class
			}
			c.State = StateDegraded
			c.Detail = fmt.Sprintf("the discard attribute for %s exists but could not be read: %v",
				d.Name, readErr)
			c.ActionableCause = "Discard support may be under-reported, which makes a sanitize fall " +
				"back to overwriting. Check sysfs permissions."
			return c
		}
		probed++
		if strings.TrimSpace(string(raw)) != "0" {
			advertised++
		}
		if d.SupportsDiscard != (strings.TrimSpace(string(raw)) != "0") {
			c.State = StateValidationFailed
			c.Detail = fmt.Sprintf("discovery reports SupportsDiscard=%t for %s but the kernel "+
				"attribute says %q", d.SupportsDiscard, d.Name, strings.TrimSpace(string(raw)))
			c.ActionableCause = "Do not rely on a discard-based sanitize. The detected value does not " +
				"match the kernel's own attribute."
			return c
		}
	}

	caps := storagemaint.CurrentCapabilities(ctx)
	if !caps.CanDiscard {
		c.State = StateDegraded
		c.Detail = fmt.Sprintf("the kernel attribute was read for %d device(s) (%d advertise discard), "+
			"but neither blkdiscard nor fstrim is installed", probed, advertised)
		c.ActionableCause = "Install util-linux (blkdiscard, fstrim) to enable discard-based " +
			"sanitization. Note that discard asks the device to deallocate blocks; it is not a " +
			"cryptographic erase and CasaNova does not verify the device honoured it."
		return c
	}
	c.State = StateHealthy
	c.Detail = fmt.Sprintf("the kernel discard attribute was read and matched for %d whole device(s) "+
		"(%d advertise discard), and a discard helper is installed", probed, advertised)
	return c
}

// checkJobStatePersistence validates the PROPERTY — a record survives a
// round-trip through the state directory — rather than the existence of the
// directory. It also provisions the directory, so a fresh box self-heals
// instead of needing an operator to mkdir it over SSH.
func checkJobStatePersistence() Check {
	c := Check{Invariant: InvJobStatePersist, Title: "Job-state persistence"}
	dir := storagemaint.StateDir()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		c.State = StateUnavailable
		c.Detail = "the state directory could not be created: " + err.Error()
		c.ActionableCause = "An interrupted format or sanitize could not be diagnosed after a " +
			"restart. Ensure " + dir + " is writable by the service."
		return c
	}

	// A distinct name per probe so two concurrent evaluations cannot grade
	// each other's file and report a spurious mismatch.
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		c.State = StateUnavailable
		c.Detail = "the probe nonce could not be generated: " + err.Error()
		c.ActionableCause = "The system random source is unavailable; this affects far more than " +
			"storage maintenance."
		return c
	}
	token := hex.EncodeToString(nonce[:])
	probe := filepath.Join(dir, ".health-probe-"+token)
	defer os.Remove(probe)

	payload := []byte("storage-maintenance health probe " + token + "\n")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		c.State = StateUnavailable
		c.Detail = "a probe record could not be written to " + dir + ": " + err.Error()
		c.ActionableCause = "Job state cannot be persisted, so an operation interrupted by a crash " +
			"or a power cut would leave a disk in an undiagnosable state. Make " + dir + " writable."
		return c
	}
	if _, err := f.Write(payload); err == nil {
		// fsync, because the whole point of this state is surviving the crash
		// it describes. A buffered write that never reaches the platter is not
		// persistence.
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		c.State = StateUnavailable
		c.Detail = "a probe record could not be durably written: " + err.Error()
		c.ActionableCause = "Job state cannot be persisted. Check free space and the filesystem's " +
			"health on " + dir + "."
		return c
	}
	if closeErr != nil {
		c.State = StateUnavailable
		c.Detail = "the probe record could not be closed: " + closeErr.Error()
		c.ActionableCause = "Job state cannot be persisted. Check free space on " + dir + "."
		return c
	}

	got, err := os.ReadFile(probe)
	if err != nil {
		c.State = StateUnavailable
		c.Detail = "the probe record was written but could not be read back: " + err.Error()
		c.ActionableCause = "Job state cannot be recovered after a restart. Check " + dir + "."
		return c
	}
	if string(got) != string(payload) {
		c.State = StateValidationFailed
		c.Detail = "the probe record read back different content than was written to " + dir
		c.ActionableCause = "Do not run any storage-maintenance operation. The state directory is " +
			"not storing data faithfully, so an interrupted job would be misdiagnosed."
		return c
	}

	if _, err := storagemaint.ReadAudit(); err != nil {
		c.State = StateDegraded
		c.Detail = "state round-tripped, but the audit log could not be read: " + err.Error()
		c.ActionableCause = "Destructive actions would be performed without a readable record of who " +
			"did what. Check " + filepath.Join(dir, "audit.jsonl") + "."
		return c
	}

	c.State = StateHealthy
	c.Detail = "a record written to " + dir + " was fsynced and read back byte-identical, and the " +
		"audit log is readable"
	return c
}

// ---------------------------------------------------------------- mount table

// rootFilesystemSource reads the mount table directly to learn which /dev node
// backs "/". Deliberately a second, independent implementation: asking blockdev
// would let a broken classifier supply its own exam answers.
//
// Returns ("", nil) when the mount table is readable but names no /dev source
// for / — an overlay or network root, where there is no disk to test against.
func rootFilesystemSource(prc string) (string, error) {
	f, err := os.Open(filepath.Join(prc, "self", "mountinfo"))
	if err != nil {
		return "", err
	}
	defer f.Close()

	var source string
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
		if left[4] != "/" {
			continue
		}
		if !strings.HasPrefix(right[1], "/dev/") {
			continue
		}
		// Later entries win: a root remounted or overmounted during boot is
		// described by the last line that claims "/".
		source = right[1]
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return source, nil
}

func joinReasons(rs []blockdev.ProtectionReason) string {
	if len(rs) == 0 {
		return "no reason recorded"
	}
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, string(r))
	}
	return strings.Join(out, ", ")
}
