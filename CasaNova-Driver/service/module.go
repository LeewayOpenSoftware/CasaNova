package service

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Driver/model"
)

// runBounded executes a kernel-facing binary with both a deadline and a
// WaitDelay. The deadline covers the ordinary slow cases; WaitDelay covers
// the one CommandContext cannot: modprobe stuck in uninterruptible sleep
// because the hardware wedged (a GPU fallen off the bus is the classic —
// and the exact moment an operator starts poking driver buttons). SIGKILL
// does nothing to D-state, so without WaitDelay the handler would inherit
// the hang; with it, the HTTP worker is released and the orphan is left to
// the kernel.
func runBounded(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 10 * time.Second
	return cmd.CombinedOutput()
}

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
	out, err := runBounded(15*time.Second, "modinfo", "-F", "filename", name)
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
	out, err := runBounded(60*time.Second, "modprobe", normalizeModuleName(name))
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
	if out, err := runBounded(60*time.Second, "modprobe", "-r", mod); err != nil {
		if out2, err2 := runBounded(60*time.Second, "rmmod", mod); err2 != nil {
			return fmt.Errorf("unload %s: %v: %s", name, err2, strings.TrimSpace(string(out2)))
		}
		_ = out
	}
	return nil
}
