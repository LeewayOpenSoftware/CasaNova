package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	net2 "net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/command"
	exec2 "github.com/LeewayOpenSoftware/CasaNova-Common/utils/exec"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/shell"
	"github.com/LeewayOpenSoftware/CasaNova/common"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/httper"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/ip_helper"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
)

type SystemService interface {
	UpdateSystemVersion(version string)
	GetSystemConfigDebug() []string
	GetCasaOSLogs(lineNumber int) string
	UpdateAssist()
	UpSystemPort(port string)
	GetTimeZone() string
	UpAppOrderFile(str, id string)
	GetAppOrderFile(id string) []byte
	GetNet(physics bool) []string
	GetNetInfo() []net.IOCountersStat
	GetCpuCoreNum() int
	GetCpuPercent() float64
	GetMemInfo() map[string]interface{}
	GetCpuInfo() []cpu.InfoStat
	GetDirPath(path string) ([]model.Path, error)
	GetDirPathOne(path string) (m model.Path)
	GetNetState(name string) string
	GetDiskInfo() *disk.UsageStat
	GetSysInfo() host.InfoStat
	GetDeviceTree() string
	GetDeviceInfo() model.DeviceInfo
	CreateFile(path string) (int, error)
	RenameFile(oldF, newF string) (int, error)
	MkdirAll(path string) (int, error)
	GetCPUTemperature() int
	GetCPUPower() map[string]string
	GetMacAddress() (string, error)
	GetNetDetail(name string) (ipv4 string, mac string, speed int)
	SystemReboot() error
	SystemShutdown() error
	RestartService(name string) error
	GetSystemEntry() string
	GenreateSystemEntry()
}
type systemService struct{}

func (c *systemService) GetDeviceInfo() model.DeviceInfo {
	m := model.DeviceInfo{}
	m.OS_Version = common.VERSION
	err, portStr := MyService.Gateway().GetPort()
	if err != nil {
		m.Port = 80
	} else {
		port := gjson.Get(portStr, "data")
		if len(port.Raw) == 0 {
			m.Port = 80
		} else {
			p, err := strconv.Atoi(port.Raw)
			if err != nil {
				m.Port = 80
			} else {
				m.Port = p
			}
		}
	}
	allIpv4 := ip_helper.GetDeviceAllIPv4()
	ip := []string{}
	nets := MyService.System().GetNet(true)
	for _, n := range nets {
		if v, ok := allIpv4[n]; ok {
			{
				ip = append(ip, v)
			}
		}
	}

	m.LanIpv4 = ip
	h, err := host.Info() /*  */
	if err == nil {
		m.DeviceName = h.Hostname
	}
	mb := model.BaseInfo{}

	err = json.Unmarshal(file.ReadFullFile(config.AppInfo.DBPath+"/baseinfo.conf"), &mb)
	if err == nil {
		m.Hash = mb.Hash
	}

	osRelease, _ := file.ReadOSRelease()
	m.DeviceModel = osRelease["MODEL"]
	m.DeviceSN = osRelease["SN"]
	res := httper.Get("http://127.0.0.1:"+strconv.Itoa(m.Port)+"/v1/users/status", nil)
	init := gjson.Get(res, "data.initialized")
	m.Initialized, _ = strconv.ParseBool(init.Raw)

	return m
}

// GenreateSystemEntry walks each module directory and concatenates its
// entry.json into one combined index. Wave 35-Q residual: previously
// (a) wrote the result with mode 0o666 (group + world writable), letting
// any UID on the host overwrite the entry index that the frontend uses
// to render module URLs — a one-line XSS+phish vector — and (b) hand-
// concatenated the entries with `"["+s+","+s+"]"` and TrimRight, which
// silently produced invalid JSON whenever any module's entry.json
// already had a leading/trailing newline (it inserts a stray comma
// between values). Use json.RawMessage + json.Marshal to round-trip the
// result and write it 0o644.
func (c *systemService) GenreateSystemEntry() {
	modelsPath := "/var/lib/casaos/www/modules"
	entryFileName := "entry.json"
	entryFilePath := filepath.Join(config.AppInfo.DBPath, "db", entryFileName)
	file.IsNotExistCreateFile(entryFilePath)

	dir, err := os.ReadDir(modelsPath)
	if err != nil {
		logger.Error("read dir error", zap.Error(err))
		return
	}
	entries := make([]json.RawMessage, 0, len(dir))
	for _, v := range dir {
		data, err := os.ReadFile(filepath.Join(modelsPath, v.Name(), entryFileName))
		if err != nil {
			logger.Error("read entry file error", zap.Error(err))
			continue
		}
		// Skip anything that isn't valid JSON rather than corrupting the
		// combined index.
		if !json.Valid(data) {
			logger.Error("module entry.json is not valid JSON; skipping", zap.String("module", v.Name()))
			continue
		}
		entries = append(entries, json.RawMessage(data))
	}
	out, err := json.Marshal(entries)
	if err != nil {
		logger.Error("marshal entry list error", zap.Error(err))
		return
	}
	// Wave 35-X: atomic write — torn entry.json blocks module
	// loading in the dashboard.
	if err := file.WriteFileAtomic(entryFilePath, out, 0o644); err != nil {
		logger.Error("write entry file error", zap.Error(err))
		return
	}
}

func (c *systemService) GetSystemEntry() string {
	modelsPath := "/var/lib/casaos/www/modules"
	entryFileName := "entry.json"
	dir, err := os.ReadDir(modelsPath)
	if err != nil {
		logger.Error("read dir error", zap.Error(err))
		return ""
	}
	json := "["
	for _, v := range dir {
		data, err := os.ReadFile(filepath.Join(modelsPath, v.Name(), entryFileName))
		if err != nil {
			logger.Error("read entry file error", zap.Error(err))
			continue
		}
		json += string(data) + ","
	}
	json = strings.TrimRight(json, ",")
	json += "]"
	if err != nil {
		logger.Error("write entry file error", zap.Error(err))
		return ""
	}
	return json
}

func (c *systemService) GetMacAddress() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	nets := MyService.System().GetNet(true)
	for _, v := range interfaces {
		for _, n := range nets {
			if v.Name == n {
				return v.HardwareAddr, nil
			}
		}
	}
	return "", errors.New("not found")
}

// GetNetDetail returns a NIC's display identity: its first non-loopback IPv4
// (CIDR stripped), MAC address, and link speed in Mbps (read from sysfs;
// virtual or down links report 0). The utilization endpoint attaches these to
// each interface so the UI can label it beyond the bare kernel name. Uses the
// gopsutil `net` binding already imported here (aliased as `net`); IPv4 is
// picked by string shape to avoid pulling in the stdlib net package.
func (c *systemService) GetNetDetail(name string) (ipv4 string, mac string, speed int) {
	if interfaces, err := net.Interfaces(); err == nil {
		for _, iface := range interfaces {
			if iface.Name != name {
				continue
			}
			mac = iface.HardwareAddr
			for _, addr := range iface.Addrs {
				ip := addr.Addr
				if idx := strings.IndexByte(ip, '/'); idx >= 0 {
					ip = ip[:idx]
				}
				if strings.Contains(ip, ".") && !strings.Contains(ip, ":") && ip != "127.0.0.1" {
					ipv4 = ip
					break
				}
			}
			break
		}
	}
	if b, err := os.ReadFile("/sys/class/net/" + name + "/speed"); err == nil {
		if s, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && s > 0 {
			speed = s
		}
	}
	return ipv4, mac, speed
}

func (c *systemService) MkdirAll(path string) (int, error) {
	_, err := os.Stat(path)
	if err == nil {
		return common_err.DIR_ALREADY_EXISTS, nil
	} else {
		if os.IsNotExist(err) {
			os.MkdirAll(path, os.ModePerm)
			return common_err.SUCCESS, nil
		} else if strings.Contains(err.Error(), ": not a directory") {
			return common_err.FILE_OR_DIR_EXISTS, err
		}
	}
	return common_err.SERVICE_ERROR, err
}

func (c *systemService) RenameFile(oldF, newF string) (int, error) {
	_, err := os.Stat(newF)
	if err == nil {
		return common_err.DIR_ALREADY_EXISTS, nil
	} else {
		if os.IsNotExist(err) {
			// SafeRename transparently handles cross-filesystem moves —
			// a plain os.Rename would fail with EXDEV between mounts.
			err := file.SafeRename(oldF, newF)
			if err != nil {
				return common_err.SERVICE_ERROR, err
			}
			return common_err.SUCCESS, nil
		}
	}
	return common_err.SERVICE_ERROR, err
}

// CreateFile reports whether the file was created, already existed, or could
// not be created.
//
// The middle branch used to call file.CreateFile(path) and discard its error,
// then return SUCCESS unconditionally. Every reason a create can actually fail
// — a missing parent directory, a read-only mount, ENOSPC, EDQUOT, a
// permission the stat above could not predict — was reported to the caller as
// success, for a file that was never written. Nothing logged it either, so the
// only visible symptom was a later operation failing on a file the client had
// been told existed.
func (c *systemService) CreateFile(path string) (int, error) {
	_, err := os.Stat(path)
	if err == nil {
		return common_err.FILE_OR_DIR_EXISTS, nil
	}
	if os.IsNotExist(err) {
		if createErr := file.CreateFile(path); createErr != nil {
			return common_err.SERVICE_ERROR, createErr
		}
		return common_err.SUCCESS, nil
	}
	return common_err.SERVICE_ERROR, err
}

func (c *systemService) GetDeviceTree() string {
	if output, err := command.OnlyExec("source " + config.AppInfo.ShellPath + "/helper.sh ;GetDeviceTree"); err != nil {
		return ""
	} else {
		return output
	}
}

func (c *systemService) GetSysInfo() host.InfoStat {
	info, _ := host.Info()
	return *info
}

func (c *systemService) GetDiskInfo() *disk.UsageStat {
	path := "/"
	if runtime.GOOS == "windows" {
		path = "C:"
	}
	diskInfo, _ := disk.Usage(path)
	diskInfo.UsedPercent, _ = strconv.ParseFloat(fmt.Sprintf("%.1f", diskInfo.UsedPercent), 64)
	diskInfo.InodesUsedPercent, _ = strconv.ParseFloat(fmt.Sprintf("%.1f", diskInfo.InodesUsedPercent), 64)
	return diskInfo
}

// GetNetState returns the kernel's operstate for an interface ("up\n",
// "down\n", ...) — callers trim it. It used to fork bash to `cat
// /sys/class/net/<name>/operstate`, once per interface on every hardware
// broadcast, forever; the file is one read away. The name is rejected outright
// when it carries a path separator: the shell version sanitized it with
// SanitizeArg, which lets "/" and "." through, so a traversal reached cat.
func (c *systemService) GetNetState(name string) string {
	if strings.Contains(name, "/") {
		return ""
	}
	b, err := os.ReadFile(sysClassNetPath + "/" + name + "/operstate")
	if err != nil {
		return ""
	}
	return string(b)
}

func (c *systemService) GetDirPathOne(path string) (m model.Path) {
	f, err := os.Stat(path)
	if err != nil {
		return
	}
	m.IsDir = f.IsDir()
	m.Name = f.Name()
	m.Path = path
	m.Size = f.Size()
	m.Date = f.ModTime()
	return
}

func (c *systemService) GetDirPath(path string) ([]model.Path, error) {
	if path == "/DATA" {
		sysType := runtime.GOOS
		if sysType == "windows" {
			path = "C:\\CasaOS\\DATA"
		}
		if sysType == "darwin" {
			path = "./CasaOS/DATA"
		}

	}

	ls, err := os.ReadDir(path)
	if err != nil {
		logger.Error("when read dir", zap.Error(err))
		return []model.Path{}, err
	}
	dirs := []model.Path{}
	if len(path) > 0 {
		// v0.1 perf: the previous loop called filepath.EvalSymlinks(filePath)
		// for every entry — even for plain files/directories. EvalSymlinks
		// walks every path component with lstat syscalls, so listing a
		// 1000-entry directory issued ~5000-7000 syscalls just to find
		// out (usually) that nothing was a symlink. On NAS hardware with
		// HDDs this turned a sub-second list into multiple seconds.
		//
		// New loop: trust the DirEntry's reported type. Only call
		// EvalSymlinks when the entry IS a symlink (l.Type()&os.ModeSymlink != 0).
		// For ext4/xfs/btrfs/zfs/mergerfs the type is read from the
		// directory entry without any extra syscall — l.Info() is the
		// only stat we issue per entry, and that one is cheap.
		for _, l := range ls {
			filePath := filepath.Join(path, l.Name())
			tempFile, err := l.Info()
			if err != nil {
				logger.Error("when read dir", zap.Error(err))
				return []model.Path{}, err
			}
			temp := model.Path{
				Name:  l.Name(),
				Path:  filePath,
				IsDir: l.IsDir(),
				Date:  tempFile.ModTime(),
				Size:  tempFile.Size(),
			}
			// Only resolve symlinks when the DirEntry tells us it IS
			// one — saves the syscall waterfall on the 95%+ of entries
			// that are plain files/dirs.
			if tempFile.Mode()&os.ModeSymlink != 0 {
				if link, lerr := filepath.EvalSymlinks(filePath); lerr == nil && link != filePath {
					if linkInfo, serr := os.Stat(link); serr == nil {
						temp.IsDir = linkInfo.IsDir()
					}
				}
			}
			dirs = append(dirs, temp)
		}
	} else {
		dirs = append(dirs, model.Path{Name: "DATA", Path: "/DATA/", IsDir: true, Date: time.Now()})
	}
	return dirs, nil
}

// CPU model and core count are fixed for the life of the process, but both
// were re-read on every utilization poll and every 10s broadcast — cpu.Info()
// parses all of /proc/cpuinfo each time. Compute once and hand out the
// cached value.
var (
	cpuInfoOnce  sync.Once
	cachedCPUInf []cpu.InfoStat
	cpuCountOnce sync.Once
	cachedCPUNum int
)

func (c *systemService) GetCpuInfo() []cpu.InfoStat {
	cpuInfoOnce.Do(func() {
		cachedCPUInf, _ = cpu.Info()
	})
	return cachedCPUInf
}

func (c *systemService) GetMemInfo() map[string]interface{} {
	memInfo, _ := mem.VirtualMemory()
	memInfo.UsedPercent, _ = strconv.ParseFloat(fmt.Sprintf("%.1f", memInfo.UsedPercent), 64)
	memData := make(map[string]interface{})
	memData["total"] = memInfo.Total
	memData["available"] = memInfo.Available
	memData["used"] = memInfo.Used
	memData["free"] = memInfo.Free
	memData["usedPercent"] = memInfo.UsedPercent
	return memData
}

func (c *systemService) GetCpuPercent() float64 {
	percent, _ := cpu.Percent(0, false)
	value, _ := strconv.ParseFloat(fmt.Sprintf("%.1f", percent[0]), 64)
	return value
}

func (c *systemService) GetCpuCoreNum() int {
	cpuCountOnce.Do(func() {
		cachedCPUNum, _ = cpu.Counts(false)
	})
	return cachedCPUNum
}

func (c *systemService) GetNetInfo() []net.IOCountersStat {
	parts, _ := net.IOCounters(true)
	return parts
}

// The kernel exposes every interface under /sys/class/net and the virtual ones
// (lo, docker0, br-*, veth*) again under /sys/devices/virtual/net, so the
// physical set is the difference between the two listings. Kept as vars so the
// tests can point them at a fixture tree.
var (
	sysClassNetPath   = "/sys/class/net"
	sysVirtualNetPath = "/sys/devices/virtual/net"
)

// GetNet lists interface names: the virtual ones when physics is false, the
// physical ones when it is true. This used to fork bash to run
// `ls /sys/class/net | grep -vw "$(ls /sys/devices/virtual/net)"`, and it sits
// inside the permanent hardware broadcast, so the box spawned bash+ls+grep
// every few seconds forever just to read two directory listings. os.ReadDir
// hands back names in byte order — the same order ls produced under the
// daemon's C locale — and the trailing empty element is what the pipeline's
// final newline turned into via strings.Split. Callers match interface names
// against this slice so the empty entry is inert, but it is part of the shape
// this has always returned and nobody asked for that to change.
//
// The names are sorted explicitly before that sentinel is appended: `ls` had
// ordered them by LC_COLLATE, which under a UTF-8 locale ignores punctuation
// and interleaves br-0/enp4s0/veth0 differently than byte order does, so the
// order the UI draws the NIC cards in used to follow whatever locale the unit
// happened to inherit. sort.Strings pins it to byte order for everyone.
func (c *systemService) GetNet(physics bool) []string {
	virtual, err := os.ReadDir(sysVirtualNetPath)
	if err != nil {
		return []string{""}
	}

	if !physics {
		names := make([]string, 0, len(virtual)+1)
		for _, v := range virtual {
			names = append(names, v.Name())
		}
		sort.Strings(names)
		return append(names, "")
	}

	isVirtual := make(map[string]struct{}, len(virtual))
	for _, v := range virtual {
		isVirtual[v.Name()] = struct{}{}
	}

	all, err := os.ReadDir(sysClassNetPath)
	if err != nil {
		return []string{""}
	}
	names := make([]string, 0, len(all)+1)
	for _, n := range all {
		if _, ok := isVirtual[n.Name()]; ok {
			continue
		}
		names = append(names, n.Name())
	}
	sort.Strings(names)
	return append(names, "")
}

func (s *systemService) UpdateSystemVersion(version string) {
	keyName := "casa_version"
	Cache.Delete(keyName)
	if file.Exists(config.AppInfo.LogPath + "/upgrade.log") {
		os.Remove(config.AppInfo.LogPath + "/upgrade.log")
	}
	file.CreateFile(config.AppInfo.LogPath + "/upgrade.log")
	// Legacy self-update path. CasaNova ships UpdateUrl blank and drives
	// updates through UpdateManager, so this only runs if an operator sets
	// their OWN UpdateUrl in casanova.conf. The old IceWhale fallback
	// (curl https://get.casaos.io/update | bash as root) is deliberately
	// gone: it phoned home to upstream and was a remote-code path onto our
	// fork. No UpdateUrl → nothing to do.
	if len(config.ServerInfo.UpdateUrl) == 0 {
		logger.Info("legacy UpdateSystemVersion called with no UpdateUrl set; skipping (use UpdateManager)")
		return
	}
	// Download the update script to a temp file first, then execute it.
	// This avoids piping curl directly to bash (CWE-94 risk).
	go func() {
		tmpScript := config.AppInfo.LogPath + "/update_script.sh"
		url := shell.SanitizeArg(config.ServerInfo.UpdateUrl)
		// Download to file instead of piping to shell
		dlCmd := exec.Command("curl", "-fsSL", "-o", tmpScript, url)
		if out, err := dlCmd.CombinedOutput(); err != nil {
			logger.Error("failed to download update script", zap.Error(err), zap.String("output", string(out)))
			return
		}
		os.Chmod(tmpScript, 0755)
		runCmd := exec.Command("bash", tmpScript)
		if out, err := runCmd.CombinedOutput(); err != nil {
			logger.Error("update script failed", zap.Error(err), zap.String("output", string(out)))
		}
	}()

	// s.log.Error(config.AppInfo.ProjectPath + "/shell/tool.sh -r " + version)
	// s.log.Error(command2.ExecResultStr(config.AppInfo.ProjectPath + "/shell/tool.sh -r " + version))
}

func (s *systemService) UpdateAssist() {
	command.ExecResultStrArray("source " + config.AppInfo.ShellPath + "/assist.sh")
}

func (s *systemService) GetTimeZone() string {
	if output, err := command.OnlyExec("source " + config.AppInfo.ShellPath + "/helper.sh ;GetTimeZone"); err != nil {
		return ""
	} else {
		return output
	}
}

func (s *systemService) GetSystemConfigDebug() []string {
	if output, err := command.OnlyExec("source " + config.AppInfo.ShellPath + "/helper.sh ;GetSysInfo"); err != nil {
		return []string{}
	} else {
		return strings.Split(output, "\n")
	}
}

func (s *systemService) UpAppOrderFile(str, id string) {
	// Validate id to prevent path traversal (e.g. "../../etc").
	cleanID := filepath.Clean(id)
	if cleanID == "." || strings.Contains(cleanID, "..") || filepath.IsAbs(cleanID) || strings.ContainsAny(cleanID, `/\`) {
		return
	}
	dir := filepath.Join(config.AppInfo.DBPath, cleanID)
	if !strings.HasPrefix(dir, filepath.Clean(config.AppInfo.DBPath)+string(filepath.Separator)) {
		return
	}
	file.WriteToPath([]byte(str), dir, "app_order.json")
}

func (s *systemService) GetAppOrderFile(id string) []byte {
	cleanID := filepath.Clean(id)
	if cleanID == "." || strings.Contains(cleanID, "..") || filepath.IsAbs(cleanID) || strings.ContainsAny(cleanID, `/\`) {
		return nil
	}
	target := filepath.Join(config.AppInfo.UserDataPath, cleanID, "app_order.json")
	if !strings.HasPrefix(target, filepath.Clean(config.AppInfo.UserDataPath)+string(filepath.Separator)) {
		return nil
	}
	return file.ReadFullFile(target)
}

func (s *systemService) UpSystemPort(port string) {
	if len(port) > 0 && port != config.ServerInfo.HttpPort {
		config.Cfg.Section("server").Key("HttpPort").SetValue(port)
		config.ServerInfo.HttpPort = port
	}
	config.Cfg.SaveTo(config.SystemConfigInfo.ConfigPath)
}

func (s *systemService) GetCasaOSLogs(lineNumber int) string {
	file, err := os.Open(filepath.Join(config.AppInfo.LogPath, fmt.Sprintf("%s.%s",
		config.AppInfo.LogSaveName,
		config.AppInfo.LogFileExt,
	)))
	if err != nil {
		return err.Error()
	}
	defer file.Close()

	// Wave 27: cap how much we'll buffer. The previous io.ReadAll
	// would happily slurp a multi-GB log file into memory and OOM
	// the daemon — a sysadmin opening "View logs" in the dashboard on
	// a long-running install was a real production crash trigger. We
	// keep the *tail* (the last 4 MiB) because that's almost always
	// what someone actually wants to see when troubleshooting.
	const maxLogBytes int64 = 4 << 20
	stat, err := file.Stat()
	if err != nil {
		return err.Error()
	}
	if stat.Size() > maxLogBytes {
		if _, err := file.Seek(-maxLogBytes, io.SeekEnd); err != nil {
			return err.Error()
		}
	}
	content, err := io.ReadAll(io.LimitReader(file, maxLogBytes))
	if err != nil {
		return err.Error()
	}

	return string(content)
}

func GetDeviceAllIP() []string {
	var address []string
	addrs, err := net2.InterfaceAddrs()
	if err != nil {
		return address
	}
	for _, a := range addrs {
		if ipNet, ok := a.(*net2.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ipNet.IP.To16() != nil {
				address = append(address, ipNet.IP.String())
			}
		}
	}
	return address
}

// find thermal_zone of cpu.
// assertions:
//   - thermal_zone "type" and "temp" are required fields
//     (https://www.kernel.org/doc/Documentation/ABI/testing/sysfs-class-thermal)
func GetCPUThermalZone() string {
	keyName := "cpu_thermal_zone"

	var path string
	if result, ok := Cache.Get(keyName); ok {
		path, ok = result.(string)
		if ok {
			return path
		}
	}

	var name string
	cpu_types := []string{"x86_pkg_temp", "cpu", "CPU", "soc"}
	stub := "/sys/devices/virtual/thermal/thermal_zone"
	for i := 0; i < 100; i++ {
		path = stub + strconv.Itoa(i)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			name = strings.TrimSuffix(string(file.ReadFullFile(path+"/type")), "\n")
			for _, s := range cpu_types {
				if strings.HasPrefix(name, s) {
					//logger.Info(fmt.Sprintf("CPU thermal zone found: %s, path: %s.", name, path))
					Cache.SetDefault(keyName, path)
					return path
				}
			}
		} else {
			if len(name) > 0 { // proves at least one zone
				path = stub + "0"
			} else {
				path = ""
			}
			break
		}
	}

	Cache.SetDefault(keyName, path)
	return path
}

// cpuHwmonNames are the kernel hwmon driver "name" entries we recognise
// as the CPU package sensor. coretemp = Intel, k10temp/zenpower = AMD,
// cpu_thermal = Raspberry Pi & most ARM SoCs, soc_thermal = misc SoCs.
// Enumerated in order; first hit wins.
var cpuHwmonNames = []string{"coretemp", "k10temp", "zenpower", "cpu_thermal", "soc_thermal"}

// findCPUHwmonTempPath walks /sys/class/hwmon looking for a driver whose
// "name" matches a known CPU sensor and returns the absolute path of its
// first temp*_input file. Returns "" when nothing matches — caller is
// expected to fall back to thermal_zone discovery or surface "unknown".
func findCPUHwmonTempPath() string {
	entries, err := filepath.Glob("/sys/class/hwmon/hwmon*")
	if err != nil {
		return ""
	}
	for _, dir := range entries {
		name := strings.TrimSpace(string(file.ReadFullFile(dir + "/name")))
		matched := false
		for _, want := range cpuHwmonNames {
			if name == want {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		// Prefer temp1_input (package); if not present pick the first.
		if file.Exists(dir + "/temp1_input") {
			return dir + "/temp1_input"
		}
		inputs, _ := filepath.Glob(dir + "/temp*_input")
		if len(inputs) > 0 {
			return inputs[0]
		}
	}
	return ""
}

func (s *systemService) GetCPUTemperature() int {
	outPut := ""
	if path := GetCPUThermalZone(); len(path) > 0 {
		outPut = string(file.ReadFullFile(path + "/temp"))
	} else if path := findCPUHwmonTempPath(); len(path) > 0 {
		outPut = string(file.ReadFullFile(path))
	}
	if len(strings.TrimSpace(outPut)) == 0 {
		return 0
	}

	celsius, _ := strconv.Atoi(strings.TrimSpace(outPut))
	if celsius > 1000 {
		celsius = celsius / 1000
	}
	return celsius
}

// GetCPUPower returns a power reading map for the UI. The result has
// "timestamp" (Unix seconds) and either "value" (microjoules from RAPL on
// Intel, microwatts from hwmon on other platforms) or "value":"" + an
// "available":"false" flag so the UI can show "—" instead of a fake 0.
//
//	Intel:      /sys/class/powercap/intel-rapl/intel-rapl:0/energy_uj
//	AMD (k10):  /sys/class/hwmon/hwmon*/power1_input  (name == "k10temp")
//	ARM/SoC:    /sys/class/hwmon/hwmon*/power*_input  (when present)
//
// Anything else returns available=false rather than the misleading "0".
func (s *systemService) GetCPUPower() map[string]string {
	data := make(map[string]string, 3)
	data["timestamp"] = strconv.FormatInt(time.Now().Unix(), 10)

	if file.Exists("/sys/class/powercap/intel-rapl/intel-rapl:0/energy_uj") {
		data["value"] = strings.TrimSpace(string(file.ReadFullFile("/sys/class/powercap/intel-rapl/intel-rapl:0/energy_uj")))
		data["available"] = "true"
		return data
	}

	if matches, err := filepath.Glob("/sys/class/hwmon/hwmon*/power1_input"); err == nil {
		for _, p := range matches {
			data["value"] = strings.TrimSpace(string(file.ReadFullFile(p)))
			data["available"] = "true"
			return data
		}
	}

	data["value"] = ""
	data["available"] = "false"
	return data
}

func (s *systemService) SystemReboot() error {
	cmd := exec2.Command("systemctl", "reboot")
	_, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}
	return nil
}

func (s *systemService) SystemShutdown() error {
	cmd := exec2.Command("systemctl", "poweroff")
	_, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}
	return nil
}

// allowedRestartServices is the exhaustive whitelist of systemd service
// names that RestartService may restart.  Any name not in this map is
// rejected, preventing arbitrary-command injection via systemctl.
var allowedRestartServices = map[string]bool{
	"casanova":                true,
	"casanova-gateway":        true,
	"casanova-message-bus":    true,
	"casanova-user-service":   true,
	"casanova-local-storage":  true,
	"casanova-app-management": true,
	"casanova-job-management": true,
	"docker":                  true,
	// Legacy CasaOS-era service names (some deployments still use these).
	"casaos":                true,
	"casaos-gateway":        true,
	"casaos-message-bus":    true,
	"casaos-user-service":   true,
	"casaos-local-storage":  true,
	"casaos-app-management": true,
	"casaos-job-management": true,
}

// RestartService restarts a systemd service by name. Only services in
// allowedRestartServices are accepted.
func (s *systemService) RestartService(name string) error {
	if !allowedRestartServices[name] {
		return fmt.Errorf("service %q is not in the allowed restart list", name)
	}
	cmd := exec2.Command("systemctl", "restart", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart %s: %s (%w)", name, string(out), err)
	}
	return nil
}

func NewSystemService() SystemService {
	return &systemService{}
}
