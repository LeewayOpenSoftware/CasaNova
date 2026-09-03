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
	"sync"
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
		mods = append(mods, km)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	// Wave 26.10: this used to call moduleIsOutOfTree(fields[0]) right here,
	// one modinfo subprocess (up to 15s each) per loaded module, one at a
	// time. /proc/modules routinely holds 150-300+ entries on a NAS box, so a
	// single GET /modules could cost seconds of pure spawn overhead even when
	// every module answers instantly, and minutes when a few are slow. The
	// classification is now cached (a module's on-disk location does not
	// change while it stays loaded) and, on a cache miss, fanned out over a
	// bounded worker pool instead of one call at a time — see
	// classifyOutOfTree below. Output shape and sort order are unchanged:
	// this only fills in OutOfTree before the final sort.
	classifyOutOfTree(mods)

	sort.Slice(mods, func(i, j int) bool { return mods[i].Name < mods[j].Name })
	return mods, nil
}

// moduleOutOfTreeCacheTTL bounds how long a module's out-of-tree
// classification is trusted. The answer is effectively immutable while a
// given instance of the module stays loaded (it is the module's own on-disk
// path), so this is generous — it exists only to eventually self-correct if
// a module is unloaded and a differently-located one with the same name is
// loaded in its place, not because the answer is expected to change on any
// normal timescale. Variable rather than const so tests can compress it.
var moduleOutOfTreeCacheTTL = 5 * time.Minute

// moduleOutOfTreeMaxConcurrency bounds how many modinfo subprocesses run at
// once during a cache-miss-heavy List(). High enough that a cold cache still
// finishes in a small multiple of one modinfo call rather than N of them;
// low enough not to fork-bomb a box that just booted with 300 modules and
// nothing warm yet. Variable rather than const so tests can shrink it to
// something they can actually observe.
var moduleOutOfTreeMaxConcurrency = 8

// runModinfoFilename is the seam moduleIsOutOfTree calls through, so tests can
// substitute a fake modinfo without a real kernel/module database and can
// count/observe concurrent calls.
var runModinfoFilename = func(name string) ([]byte, error) {
	return runBounded(15*time.Second, "modinfo", "-F", "filename", name)
}

type outOfTreeCacheEntry struct {
	value     bool
	expiresAt time.Time
}

var (
	outOfTreeCacheMu sync.Mutex
	outOfTreeCache   = map[string]outOfTreeCacheEntry{}
)

// classifyOutOfTree fills in OutOfTree for every module, running the
// cache-miss modinfo calls concurrently (bounded) rather than one at a time.
// Each goroutine only ever touches its own index of mods, so there is no
// shared mutable state between them beyond the cache (which has its own
// lock).
func classifyOutOfTree(mods []model.KernelModule) {
	sem := make(chan struct{}, moduleOutOfTreeMaxConcurrency)
	var wg sync.WaitGroup
	for i := range mods {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			mods[i].OutOfTree = moduleIsOutOfTree(mods[i].Name)
		}(i)
	}
	wg.Wait()
}

// moduleIsOutOfTree consults modinfo (when present) to flag modules living
// under updates/ or extra/ — i.e. installed via a driver package rather than
// shipped in-tree. Best-effort: returns false when modinfo is unavailable.
// Cached per name with moduleOutOfTreeCacheTTL — see the cache's own comment.
func moduleIsOutOfTree(name string) bool {
	now := time.Now()

	outOfTreeCacheMu.Lock()
	if entry, ok := outOfTreeCache[name]; ok && now.Before(entry.expiresAt) {
		outOfTreeCacheMu.Unlock()
		return entry.value
	}
	outOfTreeCacheMu.Unlock()

	out, err := runModinfoFilename(name)
	value := false
	if err == nil {
		path := strings.TrimSpace(string(out))
		value = strings.Contains(path, "/updates/") || strings.Contains(path, "/extra/")
	}

	outOfTreeCacheMu.Lock()
	outOfTreeCache[name] = outOfTreeCacheEntry{value: value, expiresAt: now.Add(moduleOutOfTreeCacheTTL)}
	outOfTreeCacheMu.Unlock()

	return value
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
