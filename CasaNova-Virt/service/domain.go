package service

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/model"
	"github.com/LeewayOpenSoftware/CasaNova-Virt/pkg/libvirtconn"
	"github.com/digitalocean/go-libvirt"
	"go.uber.org/zap"
)

// DomainService implements the VM lifecycle operations backed by libvirt.
type DomainService struct {
	conn    *libvirtconn.Manager
	diskDir string

	// isoMu guards isoDownloads, the set of destination paths with a download
	// already in flight. Two downloads of the same ISO share one "<name>.part"
	// temp file and interleave their writes into it, so the resulting image is
	// silently corrupt — the claim below makes the second request fail instead.
	isoMu        sync.Mutex
	isoDownloads map[string]struct{}
}

func NewDomainService(conn *libvirtconn.Manager, diskDir string) *DomainService {
	return &DomainService{conn: conn, diskDir: diskDir, isoDownloads: make(map[string]struct{})}
}

// List returns all domains (active + inactive), each with lightweight info.
func (s *DomainService) List() ([]model.Domain, error) {
	l, err := s.conn.Conn()
	if err != nil {
		return nil, err
	}
	flags := libvirt.ConnectListDomainsActive | libvirt.ConnectListDomainsInactive
	domains, _, err := l.ConnectListAllDomains(1, flags)
	if err != nil {
		return nil, err
	}
	out := make([]model.Domain, 0, len(domains))
	for _, d := range domains {
		out = append(out, s.summary(l, d))
	}
	return out, nil
}

// Get returns the full detail (parsed from the domain XML) for one VM.
func (s *DomainService) Get(name string) (*model.Domain, error) {
	l, err := s.conn.Conn()
	if err != nil {
		return nil, err
	}
	d, err := l.DomainLookupByName(name)
	if err != nil {
		return nil, err
	}
	sum := s.summary(l, d)

	if raw, err := l.DomainGetXMLDesc(d, 0); err == nil {
		if v, err := parseDomainXML(raw); err == nil {
			sum.Os = v.os()
			sum.Disks = v.disks()
			sum.Interfaces = v.interfaces()
			sum.Graphics = v.graphics()
		}
	}
	return &sum, nil
}

// Define creates (persists) a new domain from the supplied configuration. When
// a disk carries SizeGiB>0 and its backing file is absent, a qcow2 is
// provisioned first via qemu-img (the oracle likewise shelled out for image
// provisioning).
func (s *DomainService) Define(cfg *model.VirtualMachineConfiguration) (*model.Domain, error) {
	l, err := s.conn.Conn()
	if err != nil {
		return nil, err
	}

	for i := range cfg.Disks {
		d := &cfg.Disks[i]
		if d.Device == model.DiskDeviceCDROM {
			continue
		}
		if d.Source == "" {
			if s.diskDir == "" {
				return nil, fmt.Errorf("disk %d: no source path and no disk dir configured", i)
			}
			d.Source = filepath.Join(s.diskDir, fmt.Sprintf("%s-%d.qcow2", cfg.Name, i))
		}
		if d.SizeGiB > 0 {
			if err := ensureQcow2(d.Source, d.SizeGiB); err != nil {
				return nil, fmt.Errorf("provision disk %s: %w", d.Source, err)
			}
		}
	}

	// cloud-init: build a NoCloud cidata seed ISO and attach it as a cdrom so
	// the guest self-configures on first boot.
	if cfg.CloudInit != nil && strings.TrimSpace(cfg.CloudInit.UserData) != "" {
		seedPath, err := s.makeCloudInitSeed(cfg.Name, cfg.CloudInit)
		if err != nil {
			return nil, fmt.Errorf("cloud-init seed: %w", err)
		}
		cfg.Disks = append(cfg.Disks, model.DomainDisk{
			Device:   model.DiskDeviceCDROM,
			Source:   seedPath,
			ReadOnly: true,
		})
	}

	xmlDesc, err := buildDomainXML(cfg)
	if err != nil {
		return nil, err
	}
	d, err := l.DomainDefineXML(xmlDesc)
	if err != nil {
		return nil, fmt.Errorf("define domain: %w", err)
	}
	sum := s.summary(l, d)
	return &sum, nil
}

// Undefine removes a domain definition. The domain is destroyed first if it is
// still running.
// undefineFlags is what Undefine passes to libvirt. Package-level so the test
// can state which leftovers are covered without needing a hypervisor.
var undefineFlags = libvirt.DomainUndefineNvram |
	libvirt.DomainUndefineManagedSave |
	libvirt.DomainUndefineSnapshotsMetadata

func (s *DomainService) Undefine(name string) error {
	l, err := s.conn.Conn()
	if err != nil {
		return err
	}
	d, err := l.DomainLookupByName(name)
	if err != nil {
		return err
	}
	// Best-effort stop of a running guest before removing the definition.
	if state, _, err := l.DomainGetState(d, 0); err == nil && libvirt.DomainState(state) == libvirt.DomainRunning {
		_ = l.DomainDestroy(d)
	}

	// Plain DomainUndefine refuses a domain that owns state outside its XML,
	// and gives back a message the UI shows verbatim:
	//
	//   Requested operation is not valid: cannot undefine domain with nvram
	//
	// A UEFI guest gets an NVRAM file the first time it STARTS — not when it
	// is defined — so a VM created through this API deletes fine until it has
	// been booted once, and is then undeletable forever. Reproduced on the
	// test box: create + delete a UEFI domain succeeds; create, start, stop,
	// delete returns 500 and leaves the domain defined.
	//
	// The three flags cover the three kinds of leftover libvirt refuses to
	// orphan. NVRAM is firmware state, not user data, so removing it with the
	// domain is right — and keeping it would leave a file nothing references.
	// Managed-save and snapshot metadata are the same argument: they describe
	// a domain that is about to stop existing.
	//
	// Disks are deliberately NOT touched, which is what the UI promises.
	if err := l.DomainUndefineFlags(d, undefineFlags); err != nil {
		// Older libvirt rejects unknown flags outright rather than ignoring
		// them. Falling back keeps the common case working there instead of
		// turning a partial capability into no capability.
		if fallbackErr := l.DomainUndefine(d); fallbackErr != nil {
			return err
		}
	}
	return nil
}

// Action applies a lifecycle transition to a domain.
func (s *DomainService) Action(name string, action model.DomainAction) error {
	l, err := s.conn.Conn()
	if err != nil {
		return err
	}
	d, err := l.DomainLookupByName(name)
	if err != nil {
		return err
	}
	switch action {
	case model.ActionStart:
		return l.DomainCreate(d)
	case model.ActionShutdown:
		return l.DomainShutdown(d)
	case model.ActionReboot:
		return l.DomainReboot(d, libvirt.DomainRebootDefault)
	case model.ActionStop:
		return l.DomainDestroy(d)
	case model.ActionSuspend:
		return l.DomainSuspend(d)
	case model.ActionResume:
		return l.DomainResume(d)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
}

// Status reports the current libvirt state of a domain.
func (s *DomainService) Status(name string) (*model.DomainStatus, error) {
	l, err := s.conn.Conn()
	if err != nil {
		return nil, err
	}
	d, err := l.DomainLookupByName(name)
	if err != nil {
		return nil, err
	}
	state, reason, err := l.DomainGetState(d, 0)
	if err != nil {
		return nil, err
	}
	return &model.DomainStatus{
		Status: stateToStatus(libvirt.DomainState(state)),
		Reason: reason,
	}, nil
}

// SetAutostart toggles whether the domain starts on host boot.
func (s *DomainService) SetAutostart(name string, enabled bool) error {
	l, err := s.conn.Conn()
	if err != nil {
		return err
	}
	d, err := l.DomainLookupByName(name)
	if err != nil {
		return err
	}
	v := int32(0)
	if enabled {
		v = 1
	}
	return l.DomainSetAutostart(d, v)
}

// Console returns the remote-console (VNC/SPICE) coordinates for a running
// domain. The address is derived from the live domain XML; libvirt fills in the
// concrete autoport once the guest is running.
func (s *DomainService) Console(name string) (*model.ConsoleInfo, error) {
	l, err := s.conn.Conn()
	if err != nil {
		return nil, err
	}
	d, err := l.DomainLookupByName(name)
	if err != nil {
		return nil, err
	}
	raw, err := l.DomainGetXMLDesc(d, 0)
	if err != nil {
		return nil, err
	}
	v, err := parseDomainXML(raw)
	if err != nil {
		return nil, err
	}
	gs := v.graphics()
	if len(gs) == 0 {
		return nil, fmt.Errorf("domain %q has no graphics device", name)
	}
	g := gs[0]
	host := g.Listen
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	return &model.ConsoleInfo{
		Type:     g.Type,
		Host:     host,
		Port:     g.Port,
		Password: g.Password,
	}, nil
}

// summary builds a Domain summary from an info call. Errors on individual
// sub-calls degrade gracefully to zero values rather than failing the list.
func (s *DomainService) summary(l *libvirt.Libvirt, d libvirt.Domain) model.Domain {
	out := model.Domain{
		Name: d.Name,
		UUID: uuidString(d.UUID),
		ID:   d.ID,
	}
	if state, maxMem, _, nrCPU, _, err := l.DomainGetInfo(d); err == nil {
		out.Status = stateToStatus(libvirt.DomainState(state))
		out.MemoryMiB = maxMem / 1024 // libvirt reports KiB
		out.Vcpu = nrCPU
	}
	if as, err := l.DomainGetAutostart(d); err == nil {
		out.Autostart = as != 0
	}
	return out
}

// ListISOs returns the .iso boot/installer images available in diskDir (the
// cloud-init "-seed.iso" images are hidden — they're an implementation detail).
func (s *DomainService) ListISOs() ([]model.ISOEntry, error) {
	out := []model.ISOEntry{}
	if s.diskDir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(s.diskDir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".iso") || strings.HasSuffix(name, "-seed.iso") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, model.ISOEntry{Name: name, Path: filepath.Join(s.diskDir, name), SizeBytes: info.Size()})
	}
	return out, nil
}

// DownloadISO fetches an ISO from a URL into diskDir in the background. It
// downloads to "<name>.part" and renames to "<name>.iso" only on success, so a
// partial/interrupted download never appears in ListISOs. Admin-gated upstream.
func (s *DomainService) DownloadISO(rawURL, name string) error {
	if s.diskDir == "" {
		return fmt.Errorf("no disk dir configured")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("invalid URL (http/https only)")
	}
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("invalid ISO name")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".iso") {
		name += ".iso"
	}
	if err := os.MkdirAll(s.diskDir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(s.diskDir, name)
	part := dest + ".part"

	// Claim the destination before the goroutine starts. A download reports
	// nothing back to the caller — it only shows up in the log and, once
	// finished, in ListISOs — so there is no progress stream a second request
	// could be attached to; refusing it is the only outcome the caller can see
	// and act on, and it leaves the download already running untouched.
	s.isoMu.Lock()
	if _, busy := s.isoDownloads[dest]; busy {
		s.isoMu.Unlock()
		return fmt.Errorf("%s is already being downloaded", name)
	}
	s.isoDownloads[dest] = struct{}{}
	s.isoMu.Unlock()

	go func() {
		defer func() {
			// Release the claim on every exit path, panic included, or one
			// failed attempt would block that ISO name until a restart.
			s.isoMu.Lock()
			delete(s.isoDownloads, dest)
			s.isoMu.Unlock()
			// Nothing above this line runs under a supervisor: a panic in the
			// transfer would take the whole virt daemon down and with it every
			// VM management route, over a failed ISO fetch.
			if r := recover(); r != nil {
				logger.Error("iso download panicked", zap.Any("panic", r), zap.String("url", rawURL))
			}
		}()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			logger.Error("iso download bad request", zap.Error(err), zap.String("url", rawURL))
			return
		}
		resp, err := isoHTTPClient.Do(req) // #nosec G107 — admin-gated, user-supplied distro URL
		if err != nil {
			logger.Error("iso download failed", zap.Error(err), zap.String("url", rawURL))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			logger.Error("iso download bad status", zap.Int("status", resp.StatusCode), zap.String("url", rawURL))
			return
		}
		f, err := os.Create(part)
		if err != nil {
			logger.Error("iso download create failed", zap.Error(err))
			return
		}
		// The transfer below is the part that can panic, and the recover above
		// swallows it — so the descriptor and the temp file have to be cleaned
		// up from a defer, not from the error branches alone. Otherwise a panic
		// mid-copy leaks the fd until the daemon dies and strands a ".part" in
		// diskDir that nothing ever surfaces (ListISOs only lists ".iso") or
		// removes. renamed flips only once the image is in its final place, so
		// this never touches a completed download.
		renamed := false
		defer func() {
			f.Close() // no-op once the success path below has closed it
			if !renamed {
				os.Remove(part)
			}
		}()
		// A peer that goes silent halfway through the body is past every
		// timeout on the transport, so watch the bytes themselves: cancel the
		// request once the body has delivered nothing for isoStallTimeout.
		stall := time.AfterFunc(isoStallTimeout, cancel)
		defer stall.Stop()
		if _, err := io.Copy(f, &stallGuardReader{r: resp.Body, timer: stall}); err != nil {
			logger.Error("iso download copy failed", zap.Error(err))
			return
		}
		f.Close()
		if err := os.Rename(part, dest); err != nil {
			logger.Error("iso download rename failed", zap.Error(err))
			return
		}
		renamed = true
		logger.Info("iso download complete", zap.String("iso", dest))
	}()
	return nil
}

// isoHTTPClient fetches installer images. It deliberately carries no overall
// Timeout — a multi-GiB distro image on a slow link legitimately takes hours —
// but bounds every phase that can hang without producing a byte, which the bare
// http.Get it replaces did not: a server that accepted the connection and then
// never answered left the download pending for the lifetime of the daemon.
var isoHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// A custom DialContext disables Go's automatic h2 negotiation, so
		// without this every HTTPS fetch would quietly drop to HTTP/1.1 —
		// http.DefaultTransport, which this replaces, sets it too.
		ForceAttemptHTTP2: true,
	},
}

// isoStallTimeout is how long the response body may deliver zero bytes before
// the transfer is treated as dead. Generous on purpose: even a 1 KiB/s link
// returns from a read within about a second, so silence this long means the
// peer is gone rather than slow.
const isoStallTimeout = 2 * time.Minute

// stallGuardReader pushes the watchdog timer back every time the wrapped body
// hands over bytes, so the timer fires on a stalled transfer and not merely on
// a slow one.
type stallGuardReader struct {
	r     io.Reader
	timer *time.Timer
}

func (s *stallGuardReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(isoStallTimeout)
	}
	return n, err
}

// ensureQcow2 creates a qcow2 image of the given size (GiB) if the file does
// not already exist.
func ensureQcow2(path string, sizeGiB uint64) error {
	if _, err := os.Stat(path); err == nil {
		return nil // already present, keep existing data
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	cmd := exec.Command("qemu-img", "create", "-f", "qcow2", path, fmt.Sprintf("%dG", sizeGiB))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// makeCloudInitSeed writes user-data + meta-data and packs them into a NoCloud
// "cidata" ISO (via genisoimage) under diskDir, returning its path. cloud-init
// in the guest reads this on first boot. genisoimage must be installed.
func (s *DomainService) makeCloudInitSeed(name string, ci *model.CloudInitConfig) (string, error) {
	if s.diskDir == "" {
		return "", fmt.Errorf("no disk dir configured for cloud-init seed")
	}
	if err := os.MkdirAll(s.diskDir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "casanova-cloudinit-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	hostname := ci.Hostname
	if hostname == "" {
		hostname = name
	}
	userData := ci.UserData
	if !strings.HasPrefix(strings.TrimSpace(userData), "#cloud-config") {
		userData = "#cloud-config\n" + userData
	}
	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", name, hostname)

	if err := os.WriteFile(filepath.Join(tmp, "user-data"), []byte(userData), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, "meta-data"), []byte(metaData), 0o644); err != nil {
		return "", err
	}

	seedPath := filepath.Join(s.diskDir, name+"-seed.iso")
	// -volid cidata is the label cloud-init's NoCloud datasource looks for.
	cmd := exec.Command("genisoimage", "-quiet", "-output", seedPath,
		"-volid", "cidata", "-joliet", "-rock",
		filepath.Join(tmp, "user-data"), filepath.Join(tmp, "meta-data"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("genisoimage: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return seedPath, nil
}

func stateToStatus(state libvirt.DomainState) model.DomainStatusValue {
	switch state {
	case libvirt.DomainRunning:
		return model.StatusRunning
	case libvirt.DomainBlocked:
		return model.StatusBlocked
	case libvirt.DomainPaused:
		return model.StatusPaused
	case libvirt.DomainShutdown:
		return model.StatusShutdown
	case libvirt.DomainShutoff:
		return model.StatusShutoff
	case libvirt.DomainCrashed:
		return model.StatusCrashed
	case libvirt.DomainPmsuspended:
		return model.StatusPMSuspended
	case libvirt.DomainNostate:
		return model.StatusNoState
	default:
		return model.StatusUnknown
	}
}

// uuidString renders a libvirt UUID (16 raw bytes) as canonical 8-4-4-4-12 hex.
func uuidString(u libvirt.UUID) string {
	h := hex.EncodeToString(u[:])
	if len(h) != 32 {
		return h
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// DeleteISO removes an installation image from the ISO directory.
//
// The name is treated as a bare filename and nothing else. It arrives from an
// HTTP path parameter, so "../../etc/shadow" and "/etc/shadow" are both things
// a caller can send; filepath.Base strips a directory component and the checks
// below refuse what Base cannot fix (an empty result, "." and ".."). The file
// must also still be inside diskDir after the join, which catches anything the
// name-level checks miss.
//
// An ISO that is currently attached to a domain is left alone: removing it
// would leave a guest booting from a path that no longer exists, and the
// operator would meet that as a failure to start rather than as this refusal.
func (s *DomainService) DeleteISO(name string) error {
	if s.diskDir == "" {
		return fmt.Errorf("no disk dir configured")
	}
	base := filepath.Base(strings.TrimSpace(name))
	if base == "" || base == "." || base == ".." || base != strings.TrimSpace(name) {
		return fmt.Errorf("invalid ISO name")
	}
	if !strings.HasSuffix(strings.ToLower(base), ".iso") {
		return fmt.Errorf("not an ISO")
	}

	full := filepath.Join(s.diskDir, base)
	if rel, err := filepath.Rel(s.diskDir, full); err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("invalid ISO name")
	}

	if users, err := s.domainsUsingISO(full); err == nil && len(users) > 0 {
		return fmt.Errorf("in use by %s", strings.Join(users, ", "))
	}

	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no such ISO")
		}
		return err
	}
	return nil
}

// domainsUsingISO names the domains whose XML references this file. Best
// effort: if libvirt cannot be reached the caller proceeds, because refusing
// every delete when the hypervisor is unreachable would be worse than the risk
// it guards against.
func (s *DomainService) domainsUsingISO(path string) ([]string, error) {
	l, err := s.conn.Conn()
	if err != nil {
		return nil, err
	}
	doms, _, err := l.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, err
	}
	var users []string
	for _, d := range doms {
		xml, err := l.DomainGetXMLDesc(d, 0)
		if err != nil {
			continue
		}
		if strings.Contains(xml, path) {
			users = append(users, d.Name)
		}
	}
	return users, nil
}
