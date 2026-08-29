package service

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Driver/model"
	"github.com/LeewayOpenSoftware/CasaNova-Driver/pkg/config"
)

// DriverService reports on out-of-tree driver packages installed on disk. The
// state layout mirrors the oracle's zimaos-driver package manager (renamed to a
// generic root): modules.d/<name>.conf, disabled.d/<name>.conf, meta/<name>.env.
// Reading the state directly means listing works with or without the CLI.
type DriverService interface {
	ListInstalled() ([]model.InstalledDriver, error)
}

type driverService struct{}

func NewDriverService() DriverService { return &driverService{} }

func (d *driverService) ListInstalled() ([]model.InstalledDriver, error) {
	root := config.ServerInfo.DriverDataRoot
	enabledDir := filepath.Join(root, "modules.d")
	disabledDir := filepath.Join(root, "disabled.d")
	metaDir := filepath.Join(root, "meta")

	names := map[string]bool{}
	collectConfNames(enabledDir, names)
	collectConfNames(disabledDir, names)
	collectMetaNames(metaDir, names)

	if len(names) == 0 {
		return []model.InstalledDriver{}, nil
	}

	// Snapshot loaded module names once for runtime classification.
	loaded := map[string]bool{}
	if mods, err := NewModuleService().List(); err == nil {
		for _, m := range mods {
			loaded[m.Name] = true
		}
	}

	drivers := make([]model.InstalledDriver, 0, len(names))
	for name := range names {
		enabledConf := filepath.Join(enabledDir, name+".conf")
		disabledConf := filepath.Join(disabledDir, name+".conf")

		drv := model.InstalledDriver{
			Name:    name,
			Payload: metaValue(filepath.Join(metaDir, name+".env"), "PAYLOAD_KIND"),
		}

		var confModules []string
		switch {
		case fileExists(enabledConf):
			drv.State = "enabled"
			confModules = readModuleConf(enabledConf)
		case fileExists(disabledConf):
			drv.State = "disabled"
			confModules = readModuleConf(disabledConf)
		default:
			drv.State = "unknown"
		}
		drv.Modules = confModules
		drv.Runtime = classifyRuntime(drv.State, confModules, loaded)
		if drv.Payload == "" {
			drv.Payload = "-"
		}
		drivers = append(drivers, drv)
	}

	sort.Slice(drivers, func(i, j int) bool { return drivers[i].Name < drivers[j].Name })
	return drivers, nil
}

// classifyRuntime reproduces the oracle's runtime column semantics.
func classifyRuntime(state string, confModules []string, loaded map[string]bool) string {
	if len(confModules) == 0 {
		if state == "enabled" {
			return "firmware"
		}
		return state
	}
	loadedCount := 0
	for _, m := range confModules {
		if loaded[normalizeModuleName(m)] {
			loadedCount++
		}
	}
	switch {
	case state == "disabled" && loadedCount > 0:
		return "residual"
	case state == "disabled":
		return "disabled"
	case loadedCount == len(confModules):
		return "loaded"
	case loadedCount > 0:
		return "partial"
	default:
		return "inactive"
	}
}

func collectConfNames(dir string, out map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".conf"); ok {
			out[n] = true
		}
	}
}

func collectMetaNames(dir string, out map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".env"); ok {
			out[n] = true
		}
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// readModuleConf returns non-comment, non-blank module names from a .conf file.
func readModuleConf(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var modules []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		modules = append(modules, line)
	}
	return modules
}

// metaValue reads KEY=value from a <name>.env metadata file.
func metaValue(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	prefix := key + "="
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
