package service

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Driver/model"
)

const procModules = "/proc/modules"

// A module name must start with an alphanumeric — never a dash — so it can't
// be mistaken for an option flag by modprobe/rmmod (e.g. "-r", "--remove").
// exec.Command already avoids the shell, but this closes the arg-injection gap.
var moduleNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ModuleService reports and manages loaded kernel modules.
type ModuleService interface {
	List() ([]model.KernelModule, error)
	Load(name string) error
	Unload(name string) error
	IsLoaded(name string) (bool, error)
}

type moduleService struct{}

func NewModuleService() ModuleService { return &moduleService{} }

// List parses /proc/modules. Each line:
//
//	name size refcount used_by state address
func (m *moduleService) List() ([]model.KernelModule, error) {
	f, err := os.Open(procModules)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	mods := make([]model.KernelModule, 0, 128)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		size, _ := strconv.ParseInt(fields[1], 10, 64)
		ref, _ := strconv.Atoi(fields[2])

		var usedBy []string
		if fields[3] != "-" {
			for _, u := range strings.Split(strings.TrimRight(fields[3], ","), ",") {
				if u != "" {
					usedBy = append(usedBy, u)
				}
			}
		}

		km := model.KernelModule{
			Name:     fields[0],
			Size:     size,
			RefCount: ref,
			UsedBy:   usedBy,
		}
		if len(fields) >= 5 {
			km.State = fields[4]
		}
		if len(fields) >= 6 {
			km.Address = fields[5]
		}
		km.OutOfTree = moduleIsOutOfTree(fields[0])
		mods = append(mods, km)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	sort.Slice(mods, func(i, j int) bool { return mods[i].Name < mods[j].Name })
	return mods, nil
}

// moduleIsOutOfTree consults modinfo (when present) to flag modules living
// under updates/ or extra/ — i.e. installed via a driver package rather than
// shipped in-tree. Best-effort: returns false when modinfo is unavailable.
func moduleIsOutOfTree(name string) bool {
	out, err := exec.Command("modinfo", "-F", "filename", name).Output()
	if err != nil {
		return false
	}
	path := strings.TrimSpace(string(out))
	return strings.Contains(path, "/updates/") || strings.Contains(path, "/extra/")
}

func normalizeModuleName(name string) string {
	name = strings.TrimSuffix(name, ".ko")
	return strings.ReplaceAll(name, "-", "_")
}

func (m *moduleService) IsLoaded(name string) (bool, error) {
	name = normalizeModuleName(name)
	mods, err := m.List()
	if err != nil {
		return false, err
	}
	for _, mod := range mods {
		if mod.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// Load runs modprobe. The name is validated to keep it out of the shell.
func (m *moduleService) Load(name string) error {
	if !moduleNameRe.MatchString(name) {
		return fmt.Errorf("invalid module name %q", name)
	}
	out, err := exec.Command("modprobe", normalizeModuleName(name)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("modprobe %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Unload runs `modprobe -r`, falling back to rmmod.
func (m *moduleService) Unload(name string) error {
	if !moduleNameRe.MatchString(name) {
		return fmt.Errorf("invalid module name %q", name)
	}
	mod := normalizeModuleName(name)
	if out, err := exec.Command("modprobe", "-r", mod).CombinedOutput(); err != nil {
		if out2, err2 := exec.Command("rmmod", mod).CombinedOutput(); err2 != nil {
			return fmt.Errorf("unload %s: %v: %s", name, err2, strings.TrimSpace(string(out2)))
		}
		_ = out
	}
	return nil
}
