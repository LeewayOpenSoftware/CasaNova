package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// runCmd runs an external command with a hard deadline so a wedged tool (most
// dangerously `hdparm` on a flaky/half-connected disk, whose kernel ATA/SCSI
// retries can block far longer than a service start timeout) can never hang the
// caller — including the boot-time Reapply path.
func runCmd(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("%s timed out after %s", name, timeout)
	}
	return out, err
}

// Disk-indexed tuning: I/O scheduler, read-ahead, HDD spindown. Every disk is
// validated by MEMBERSHIP in os.ReadDir(/sys/block) — a name the kernel didn't
// enumerate can never reach a write or an hdparm exec, which closes path
// traversal by construction. Persistence keys by /dev/disk/by-id (stable across
// reboots) never sdX (which reorders), so a reapply can't hit the wrong disk.

const devDiskByID = "/dev/disk/by-id"

// DiskScheduler is one block device's I/O tuning state.
type DiskScheduler struct {
	Name         string   `json:"name"` // sda, nvme0n1
	ByID         string   `json:"byId"` // stable id (empty if none)
	Model        string   `json:"model"`
	Rotational   bool     `json:"rotational"` // true = spinning HDD
	Removable    bool     `json:"removable"`
	SizeBytes    int64    `json:"sizeBytes"`
	Available    []string `json:"available"` // scheduler options
	Current      string   `json:"current"`   // active scheduler (the [bracketed] one)
	ReadAheadKiB int      `json:"readAheadKiB"`
	Spindown     string   `json:"spindown"` // persisted timeout key, "" = unmanaged
}

// isRealDisk filters out loop/ram/zram/dm virtual devices.
func isRealDisk(name string) bool {
	for _, p := range []string{"loop", "ram", "zram", "dm-", "md", "sr"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

func (s *tuningService) IOSchedulers() ([]DiskScheduler, error) {
	entries, err := os.ReadDir(sysfsBlock)
	if err != nil {
		return nil, err
	}
	st := loadTuningState()
	out := make([]DiskScheduler, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !isRealDisk(name) {
			continue
		}
		base := filepath.Join(sysfsBlock, name)
		d := DiskScheduler{Name: name}
		d.Rotational = readSysAttr(filepath.Join(base, "queue"), "rotational") == "1"
		d.Removable = readSysAttr(base, "removable") == "1"
		d.Model = readSysAttr(filepath.Join(base, "device"), "model")
		if sectors := readSysAttr(base, "size"); sectors != "" {
			if n, err := strconv.ParseInt(sectors, 10, 64); err == nil {
				d.SizeBytes = n * 512 // sysfs size is in 512-byte sectors
			}
		}
		d.Available, d.Current = parseScheduler(readSysAttr(filepath.Join(base, "queue"), "scheduler"))
		if ra := readSysAttr(filepath.Join(base, "queue"), "read_ahead_kb"); ra != "" {
			d.ReadAheadKiB, _ = strconv.Atoi(ra)
		}
		d.ByID = diskByID(name)
		if d.ByID != "" && st.Spindown != nil {
			d.Spindown = st.Spindown[d.ByID]
		}
		out = append(out, d)
	}
	return out, nil
}

// parseScheduler turns "none [mq-deadline] kyber bfq" into (["none",...], "mq-deadline").
func parseScheduler(raw string) ([]string, string) {
	fields := strings.Fields(raw)
	opts := make([]string, 0, len(fields))
	active := ""
	for _, f := range fields {
		if strings.HasPrefix(f, "[") && strings.HasSuffix(f, "]") {
			active = strings.Trim(f, "[]")
			opts = append(opts, active)
		} else {
			opts = append(opts, f)
		}
	}
	return opts, active
}

func (s *tuningService) SetIOScheduler(disk, scheduler string) error {
	if !diskExists(disk) {
		return fmt.Errorf("unknown disk %q", disk)
	}
	if err := s.setIOSchedulerByName(disk, scheduler); err != nil {
		return err
	}
	if byID := diskByID(disk); byID != "" {
		s.persist(func(st *tuningState) {
			if st.IOSchedulers == nil {
				st.IOSchedulers = map[string]string{}
			}
			st.IOSchedulers[byID] = scheduler
		})
	}
	return nil
}

// setIOSchedulerByName validates the value against the disk's own scheduler
// file (the kernel's allowlist) and writes it. Used by SetIOScheduler and by
// Reapply (which resolves a by-id back to the current name).
func (s *tuningService) setIOSchedulerByName(name, scheduler string) error {
	scheduler = strings.TrimSpace(scheduler)
	schedPath := filepath.Join(sysfsBlock, name, "queue", "scheduler")
	opts, _ := parseScheduler(readSysAttr(filepath.Join(sysfsBlock, name, "queue"), "scheduler"))
	if !contains(opts, scheduler) {
		return fmt.Errorf("scheduler %q not available for %s (allowed: %s)", scheduler, name, strings.Join(opts, ", "))
	}
	return writeSysAttr(schedPath, scheduler)
}

func (s *tuningService) SetReadAhead(disk string, kb int) error {
	if !diskExists(disk) {
		return fmt.Errorf("unknown disk %q", disk)
	}
	if kb < 0 {
		kb = 0
	}
	if kb > 16384 {
		kb = 16384
	}
	if err := writeSysAttr(filepath.Join(sysfsBlock, disk, "queue", "read_ahead_kb"), strconv.Itoa(kb)); err != nil {
		return err
	}
	if byID := diskByID(disk); byID != "" {
		s.persist(func(st *tuningState) {
			if st.ReadAhead == nil {
				st.ReadAhead = map[string]int{}
			}
			st.ReadAhead[byID] = kb
		})
	}
	return nil
}

// ── HDD spindown (hdparm) ────────────────────────────────────────────────
// Timeout keys map to hdparm -S values: 0=off, 1-240 = value*5s,
// 241-251 = (value-240)*30min. Only these five presets are accepted.
var spindownValues = map[string]string{
	"off": "0",
	"10m": "120", // 120 * 5s = 600s
	"20m": "240", // 240 * 5s = 1200s
	"30m": "241", // (241-240) * 30min
	"1h":  "242", // 2 * 30min
}

func (s *tuningService) SetSpindown(disk, timeout string) error {
	if !diskExists(disk) {
		return fmt.Errorf("unknown disk %q", disk)
	}
	// Only spinning disks — spindown on an SSD is meaningless and, on some
	// USB bridges, harmful.
	if readSysAttr(filepath.Join(sysfsBlock, disk, "queue"), "rotational") != "1" {
		return fmt.Errorf("%s is not a rotational disk; spindown does not apply", disk)
	}
	if err := s.setSpindownByName(disk, timeout); err != nil {
		return err
	}
	if byID := diskByID(disk); byID != "" {
		s.persist(func(st *tuningState) {
			if st.Spindown == nil {
				st.Spindown = map[string]string{}
			}
			st.Spindown[byID] = timeout
		})
	}
	return nil
}

// StandbyNow spins a rotational disk down immediately (hdparm -y). Used by the
// power-saving ladder on UPS prepare_shutdown. Unlike SetSpindown this changes
// no timeout and persists nothing — the next disk access simply wakes it, so
// there is no revert lever.
func (s *tuningService) StandbyNow(disk string) error {
	// Accept a stable /dev/disk/by-id name too (kernel sdX names reorder
	// across boots) — resolve it back to the current kernel name first.
	if !diskExists(disk) {
		if name := nameByID(disk); name != "" {
			disk = name
		}
	}
	if !diskExists(disk) {
		return fmt.Errorf("unknown disk %q", disk)
	}
	if readSysAttr(filepath.Join(sysfsBlock, disk, "queue"), "rotational") != "1" {
		return fmt.Errorf("%s is not a rotational disk; standby does not apply", disk)
	}
	out, err := runCmd(20*time.Second, "hdparm", "-y", "/dev/"+disk)
	if err != nil {
		return fmt.Errorf("hdparm -y: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *tuningService) setSpindownByName(name, timeout string) error {
	val, ok := spindownValues[strings.TrimSpace(timeout)]
	if !ok {
		return fmt.Errorf("invalid spindown timeout %q (allowed: off, 10m, 20m, 30m, 1h)", timeout)
	}
	// name comes from /sys/block enumeration or a by-id resolution, never the
	// raw client string, so /dev/<name> is safe to hand to hdparm. Bounded by
	// runCmd so a stuck disk can't hang startup.
	out, err := runCmd(20*time.Second, "hdparm", "-S", val, "/dev/"+name)
	if err != nil {
		return fmt.Errorf("hdparm: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ── by-id helpers ────────────────────────────────────────────────────────

func diskExists(name string) bool {
	if !isRealDisk(name) || strings.Contains(name, "/") || strings.Contains(name, "..") {
		return false
	}
	fi, err := os.Stat(filepath.Join(sysfsBlock, name))
	return err == nil && fi.IsDir()
}

// diskByID returns a stable /dev/disk/by-id name for a whole disk, preferring
// wwn-/nvme-eui (globally unique) over ata-/scsi- ids, and never a partition.
func diskByID(name string) string {
	entries, err := os.ReadDir(devDiskByID)
	if err != nil {
		return ""
	}
	best := ""
	for _, e := range entries {
		link := filepath.Join(devDiskByID, e.Name())
		target, err := os.Readlink(link)
		if err != nil {
			continue
		}
		if filepath.Base(target) != name {
			continue // resolves to a different disk or a partition
		}
		id := e.Name()
		if strings.HasPrefix(id, "wwn-") || strings.HasPrefix(id, "nvme-eui") {
			return id // most stable — take immediately
		}
		if best == "" {
			best = id
		}
	}
	return best
}

// nameByID resolves a stored by-id back to the current kernel name (sda), or ""
// if that disk is no longer present.
func nameByID(byID string) string {
	target, err := os.Readlink(filepath.Join(devDiskByID, byID))
	if err != nil {
		return ""
	}
	name := filepath.Base(target)
	if diskExists(name) {
		return name
	}
	return ""
}
