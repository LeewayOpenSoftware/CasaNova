package service

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// A compiled allowlist of tunable sysctls. The client sends a KEY from this map
// (never a path), and each entry knows its procfs path and how to validate the
// value. Nothing outside this table is reachable, so there is no way to reach
// an arbitrary /proc/sys node. Kept intentionally small and NAS-relevant.
type sysctlDef struct {
	Path     string
	Label    string
	Kind     string // "int" | "enum"
	Min, Max int    // for int
	// validate returns the value to write (possibly normalised) or an error.
	validate func(def sysctlDef, raw string) (string, error)
}

// SysctlEntry is what the API returns for one tunable.
type SysctlEntry struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Value   string   `json:"value"`
	Default string   `json:"default"`
	Kind    string   `json:"kind"`
	Min     int      `json:"min,omitempty"`
	Max     int      `json:"max,omitempty"`
	Options []string `json:"options,omitempty"` // for enum kinds
}

func intValidator(def sysctlDef, raw string) (string, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("value must be an integer")
	}
	if n < def.Min {
		n = def.Min
	}
	if n > def.Max {
		n = def.Max
	}
	return strconv.Itoa(n), nil
}

// sysctlTable — the entire reachable surface. Defaults chosen conservative.
var sysctlTable = map[string]sysctlDef{
	"swappiness":             {Path: "/proc/sys/vm/swappiness", Label: "vm.swappiness", Kind: "int", Min: 0, Max: 100, validate: intValidator},
	"vfs_cache_pressure":     {Path: "/proc/sys/vm/vfs_cache_pressure", Label: "vm.vfs_cache_pressure", Kind: "int", Min: 1, Max: 500, validate: intValidator},
	"dirty_ratio":            {Path: "/proc/sys/vm/dirty_ratio", Label: "vm.dirty_ratio", Kind: "int", Min: 1, Max: 90, validate: intValidator},
	"dirty_background_ratio": {Path: "/proc/sys/vm/dirty_background_ratio", Label: "vm.dirty_background_ratio", Kind: "int", Min: 1, Max: 90, validate: intValidator},
	"max_user_watches":       {Path: "/proc/sys/fs/inotify/max_user_watches", Label: "fs.inotify.max_user_watches", Kind: "int", Min: 8192, Max: 4194304, validate: intValidator},
	"netdev_max_backlog":     {Path: "/proc/sys/net/core/netdev_max_backlog", Label: "net.core.netdev_max_backlog", Kind: "int", Min: 1000, Max: 250000, validate: intValidator},
	"rmem_max":               {Path: "/proc/sys/net/core/rmem_max", Label: "net.core.rmem_max", Kind: "int", Min: 65536, Max: 67108864, validate: intValidator},
	"wmem_max":               {Path: "/proc/sys/net/core/wmem_max", Label: "net.core.wmem_max", Kind: "int", Min: 65536, Max: 67108864, validate: intValidator},
}

// sysctlDefaults are conservative kernel-ish defaults for the reset button.
var sysctlDefaults = map[string]string{
	"swappiness": "60", "vfs_cache_pressure": "100", "dirty_ratio": "20",
	"dirty_background_ratio": "10", "max_user_watches": "65536",
	"netdev_max_backlog": "1000", "rmem_max": "212992", "wmem_max": "212992",
}

func (s *tuningService) Sysctls() ([]SysctlEntry, error) {
	// stable order
	keys := []string{"swappiness", "vfs_cache_pressure", "dirty_ratio", "dirty_background_ratio", "max_user_watches", "netdev_max_backlog", "rmem_max", "wmem_max"}
	out := make([]SysctlEntry, 0, len(keys))
	for _, k := range keys {
		def := sysctlTable[k]
		cur := ""
		if raw, err := os.ReadFile(def.Path); err == nil {
			cur = strings.TrimSpace(string(raw))
		}
		out = append(out, SysctlEntry{
			Key: k, Label: def.Label, Value: cur, Default: sysctlDefaults[k],
			Kind: def.Kind, Min: def.Min, Max: def.Max,
		})
	}
	return out, nil
}

func (s *tuningService) SetSysctl(key, value string) error {
	def, ok := sysctlTable[key]
	if !ok {
		return fmt.Errorf("unknown sysctl key %q", key)
	}
	val, err := def.validate(def, value)
	if err != nil {
		return err
	}
	// Cross-field constraint: dirty_background_ratio must stay below
	// dirty_ratio or the kernel silently clamps and behaviour surprises.
	if key == "dirty_background_ratio" {
		if raw, err := os.ReadFile(sysctlTable["dirty_ratio"].Path); err == nil {
			if dr, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				if n, _ := strconv.Atoi(val); n >= dr {
					val = strconv.Itoa(dr - 1)
				}
			}
		}
	}
	if err := writeSysAttr(def.Path, val); err != nil {
		return err
	}
	s.persist(func(st *tuningState) {
		if st.Sysctls == nil {
			st.Sysctls = map[string]string{}
		}
		st.Sysctls[key] = val
	})
	return nil
}
