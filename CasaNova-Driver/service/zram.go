package service

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// zram — a compressed RAM swap device. 100% sysfs after a modprobe. Enabling:
// modprobe zram → set comp_algorithm + disksize (clamped to <= RAM) →
// mkswap + swapon with high priority. Disabling: swapoff + reset. Kept to a
// single device (/dev/zram0) — one knob, one toggle.

const (
	zramDevice  = "zram0"
	zramSysBase = "/sys/block/zram0"
	zramControl = "/sys/class/zram-control"
)

// ZramState is what we persist.
type ZramState struct {
	Enabled   bool   `json:"enabled"`
	Algorithm string `json:"algorithm"`
	SizeMiB   int    `json:"sizeMiB"`
}

// ZramInfo is the live status returned to the UI.
type ZramInfo struct {
	Enabled      bool     `json:"enabled"`
	Algorithm    string   `json:"algorithm"`
	Available    []string `json:"available"` // comp_algorithm options
	DiskSizeMiB  int      `json:"diskSizeMiB"`
	OrigDataMiB  int      `json:"origDataMiB"`  // uncompressed stored
	ComprDataMiB int      `json:"comprDataMiB"` // compressed stored
	MemTotalMiB  int      `json:"memTotalMiB"`  // host RAM (for the size slider ceiling)
}

func (s *tuningService) Zram() (ZramInfo, error) {
	info := ZramInfo{Available: []string{"lzo", "lz4", "zstd"}, MemTotalMiB: int(meminfoField("MemTotal") / 1024)}
	if _, err := os.Stat(zramSysBase); err != nil {
		return info, nil // module not loaded / no device yet — disabled
	}
	info.Enabled = zramIsSwapOn()
	// comp_algorithm lists options with the active one in [brackets].
	if raw := readSysAttr(zramSysBase, "comp_algorithm"); raw != "" {
		opts, active := parseScheduler(raw) // same [active] bracket format
		info.Available = opts
		info.Algorithm = active
	}
	if ds := readSysAttr(zramSysBase, "disksize"); ds != "" {
		if n, err := strconv.ParseInt(ds, 10, 64); err == nil {
			info.DiskSizeMiB = int(n / 1024 / 1024)
		}
	}
	// mm_stat: orig_data_size compr_data_size mem_used_total ...
	if raw := readSysAttr(zramSysBase, "mm_stat"); raw != "" {
		f := strings.Fields(raw)
		if len(f) >= 2 {
			if n, err := strconv.ParseInt(f[0], 10, 64); err == nil {
				info.OrigDataMiB = int(n / 1024 / 1024)
			}
			if n, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				info.ComprDataMiB = int(n / 1024 / 1024)
			}
		}
	}
	return info, nil
}

func zramIsSwapOn() bool {
	raw, err := os.ReadFile("/proc/swaps")
	if err != nil {
		return false
	}
	return strings.Contains(string(raw), "/dev/"+zramDevice)
}

func (s *tuningService) SetZram(enable bool, algorithm string, sizeMiB int) (ZramInfo, error) {
	if enable {
		if err := s.enableZram(algorithm, sizeMiB); err != nil {
			return ZramInfo{}, err
		}
		s.persist(func(st *tuningState) { st.Zram = &ZramState{Enabled: true, Algorithm: algorithm, SizeMiB: sizeMiB} })
	} else {
		if err := s.disableZram(); err != nil {
			return ZramInfo{}, err
		}
		s.persist(func(st *tuningState) { st.Zram = &ZramState{Enabled: false} })
	}
	return s.Zram()
}

func (s *tuningService) enableZram(algorithm string, sizeMiB int) error {
	// Validate algorithm against a small known set; the kernel also rejects
	// unknown ones, but we keep the API surface tight.
	switch algorithm {
	case "lzo", "lz4", "zstd":
	default:
		algorithm = "zstd"
	}
	// Clamp size to (0, RAM]. A zram larger than RAM is nonsensical.
	memMiB := int(meminfoField("MemTotal") / 1024)
	if sizeMiB <= 0 {
		sizeMiB = memMiB / 4 // sane default: 25% of RAM
	}
	if memMiB > 0 && sizeMiB > memMiB {
		sizeMiB = memMiB
	}

	// Load the module (idempotent). exec avoids re-implementing modprobe.
	if _, err := os.Stat(zramControl); err != nil {
		if out, err := runCmd(15*time.Second, "modprobe", "zram"); err != nil {
			return fmt.Errorf("modprobe zram: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	// Reset if the device is already INITIALISED (disksize != 0), not merely
	// if swap is on. comp_algorithm and disksize are one-shot: the kernel
	// rejects writes with EBUSY while disksize != 0 — including after a prior
	// enable that set disksize but failed at mkswap/swapon (swap never came
	// on). Gating the reset on swap-on would then leave zram permanently
	// un-enableable. disableZram does swapoff (guarded) + an unconditional
	// reset, which clears disksize back to 0.
	if ds := readSysAttr(zramSysBase, "disksize"); ds != "" && ds != "0" {
		_ = s.disableZram()
	}
	// Order matters: algorithm before disksize, disksize is one-shot.
	if err := writeSysAttr(zramSysBase+"/comp_algorithm", algorithm); err != nil {
		return err
	}
	if err := writeSysAttr(zramSysBase+"/disksize", strconv.Itoa(sizeMiB)+"M"); err != nil {
		return err
	}
	if out, err := runCmd(15*time.Second, "mkswap", "/dev/"+zramDevice); err != nil {
		return fmt.Errorf("mkswap: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := runCmd(15*time.Second, "swapon", "-p", "100", "/dev/"+zramDevice); err != nil {
		return fmt.Errorf("swapon: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *tuningService) disableZram() error {
	if zramIsSwapOn() {
		if out, err := runCmd(15*time.Second, "swapoff", "/dev/"+zramDevice); err != nil {
			return fmt.Errorf("swapoff: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	// reset frees the compressed pages and lets disksize be set again.
	_ = writeSysAttr(zramSysBase+"/reset", "1")
	return nil
}
