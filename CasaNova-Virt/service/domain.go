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
	"sync/atomic"
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

	// isoMu guards isoDownloads, keyed by destination path. Two downloads of
	// the same ISO share one "<name>.part" temp file and interleave their
	// writes into it, so the resulting image is silently corrupt — the claim
	// made when starting one makes the second request fail instead.
	//
	// It used to hold struct{}: enough to answer "is this one busy" and
	// nothing else. A download reported nothing back — progress lived only in
	// the transferred bytes and failures only in the log, so a 3 GiB image on
	// a slow link was indistinguishable from one that had died ten minutes
	// ago, and there was no way to stop one that was going nowhere.
	isoMu        sync.Mutex
	isoDownloads map[string]*isoDownload
}

// isoDownload is one transfer, as the UI needs to see it.
type isoDownload struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	TotalBytes int64  `json:"total_bytes"` // 0 when the server sends no Content-Length
	StartedAt  int64  `json:"started_at"`  // unix seconds
	Error      string `json:"error,omitempty"`
	Cancelled  bool   `json:"cancelled,omitempty"`
	Done       bool   `json:"done"`

	// done counts bytes written and is read from the HTTP handler while the
	// transfer goroutine writes it, so it is atomic rather than mutex-guarded:
	// the reader wants a recent number, not a consistent snapshot with
	// everything else.
	done atomic.Int64

	// cancel stops the request. Terminal entries keep a no-op so a cancel
	// arriving just after a finish is a no-op rather than a nil dereference.
	cancel context.CancelFunc

	// finishedAt is when a terminal entry became terminal. Terminal entries
	// linger briefly so the UI can say what happened — a failure that vanishes
	// from the list looks exactly like one that completed.
	finishedAt time.Time
}

// isoTerminalLinger is how long a finished, failed or cancelled download stays
// visible. Long enough for a UI polling every couple of seconds to show the
// outcome, short enough that it does not accumulate.
const isoTerminalLinger = 30 * time.Second

// Snapshot copies the parts a caller may read, resolving the atomic counter.
func (d *isoDownload) Snapshot() model.ISODownload {
	return model.ISODownload{
		Name:       d.Name,
		URL:        d.URL,
		TotalBytes: d.TotalBytes,
		DoneBytes:  d.done.Load(),
		StartedAt:  d.StartedAt,
		Error:      d.Error,
		Cancelled:  d.Cancelled,
		Done:       d.Done,
	}
}

func NewDomainService(conn *libvirtconn.Manager, diskDir string) *DomainService {
	return &DomainService{conn: conn, diskDir: diskDir, isoDownloads: make(map[string]*isoDownload)}
}

// ISODownloads reports every transfer worth showing, pruning terminal entries
// that have been on display long enough.
func (s *DomainService) ISODownloads() []model.ISODownload {
	s.isoMu.Lock()
	defer s.isoMu.Unlock()
	out := []model.ISODownload{}
	for dest, d := range s.isoDownloads {
		if d.Done && time.Since(d.finishedAt) > isoTerminalLinger {
			delete(s.isoDownloads, dest)
			continue
		}
		out = append(out, d.Snapshot())
	}
	return out
}

// CancelISODownload stops a transfer in flight. The partial file is removed by
// the transfer goroutine's own cleanup, which already runs on every exit path.
func (s *DomainService) CancelISODownload(name string) error {
	if s.diskDir == "" {
		return fmt.Errorf("no disk dir configured")
	}
	base, err := safeDiskDirName(s.diskDir, name, "ISO name")
	if err != nil {
		return err
	}
	s.isoMu.Lock()
	d, ok := s.isoDownloads[filepath.Join(s.diskDir, base)]
	if !ok || d.Done {
		s.isoMu.Unlock()
		return fmt.Errorf("no download in progress for %s", base)
	}
	d.Cancelled = true
	cancel := d.cancel
	s.isoMu.Unlock()

	// Called outside the lock: cancel unblocks the copy, whose deferred
	// cleanup takes the same lock to mark the entry terminal.
	cancel()
	return nil
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
	// cfg.Name reaches the filesystem three times from here -- the qcow2 path
	// below, the cloud-init seed ISO in makeCloudInitSeed, and the MkDirAll
	// ensureQcow2 does on the parent of both. The route handler checked it
	// against "" and nothing else. Refuse before dialling libvirt: an invalid
	// request should not cost a connection.
	if _, err := safeDiskDirName(s.diskDir, cfg.Name, "domain name"); err != nil {
		return nil, err
	}

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

	// The context is created here rather than in the goroutine so the entry
	// can carry its cancel func before anything is observable.
	ctx, cancel := context.WithCancel(context.Background())

	// Claim the destination before the goroutine starts: two downloads of the
	// same name would interleave writes into one ".part" and silently corrupt
	// it. A terminal entry from an earlier attempt does not hold the claim —
	// it is only still there so the UI can show what happened — so it is
	// replaced rather than treated as busy.
	s.isoMu.Lock()
	if prev, busy := s.isoDownloads[dest]; busy && !prev.Done {
		s.isoMu.Unlock()
		cancel()
		return fmt.Errorf("%s is already being downloaded", name)
	}
	entry := &isoDownload{
		Name:      name,
		URL:       rawURL,
		StartedAt: time.Now().Unix(),
		cancel:    cancel,
	}
	s.isoDownloads[dest] = entry
	s.isoMu.Unlock()

	// finish marks the entry terminal with a reason. The entry is NOT removed:
	// a download that vanishes from the list looks identical whether it
	// finished or died, and the failure is the case the operator needs to see.
	finish := func(reason string) {
		s.isoMu.Lock()
		if entry.Cancelled {
			entry.Error = ""
		} else if reason != "" {
			entry.Error = reason
		}
		entry.Done = true
		entry.finishedAt = time.Now()
		s.isoMu.Unlock()
	}

	go func() {
		defer cancel()
		defer func() {
			// Nothing above this line runs under a supervisor: a panic in the
			// transfer would take the whole virt daemon down and with it every
			// VM management route, over a failed ISO fetch.
			if r := recover(); r != nil {
				logger.Error("iso download panicked", zap.Any("panic", r), zap.String("url", rawURL))
				finish("the transfer crashed")
			}
		}()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			logger.Error("iso download bad request", zap.Error(err), zap.String("url", rawURL))
			finish(err.Error())
			return
		}
		resp, err := isoHTTPClient.Do(req) // #nosec G107 — admin-gated, user-supplied distro URL
		if err != nil {
			logger.Error("iso download failed", zap.Error(err), zap.String("url", rawURL))
			finish("could not reach the server")
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			logger.Error("iso download bad status", zap.Int("status", resp.StatusCode), zap.String("url", rawURL))
			finish(fmt.Sprintf("the server answered %s", resp.Status))
			return
		}
		// Content-Length is what makes a percentage possible. Plenty of
		// mirrors send it; the ones that do not leave TotalBytes at zero and
		// the UI shows bytes transferred without a bar, which is honest rather
		// than a progress bar guessing.
		if resp.ContentLength > 0 {
			s.isoMu.Lock()
			entry.TotalBytes = resp.ContentLength
			s.isoMu.Unlock()
		}
		f, err := os.Create(part)
		if err != nil {
			logger.Error("iso download create failed", zap.Error(err))
			finish("could not write to the ISO directory")
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
		if _, err := io.Copy(f, &stallGuardReader{r: resp.Body, timer: stall, done: &entry.done}); err != nil {
			logger.Error("iso download copy failed", zap.Error(err))
			// A cancelled transfer and a stalled one both surface here as a
			// context error, and they mean opposite things to the operator:
			// one is what they asked for, the other is the mirror giving up.
			// Cancelled is recorded on the entry before cancel() is called, so
			// finish can tell them apart.
			finish("the transfer stopped: " + err.Error())
			return
		}
		f.Close()
		if err := os.Rename(part, dest); err != nil {
			logger.Error("iso download rename failed", zap.Error(err))
			finish("could not move the finished image into place")
			return
		}
		renamed = true
		logger.Info("iso download complete", zap.String("iso", dest))
		finish("")
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
	// done accumulates bytes handed to the writer. It rides along here rather
	// than in a second wrapper because this reader already sees every byte,
	// and one pass over the stream is the whole point of both jobs.
	done *atomic.Int64
}

func (s *stallGuardReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(isoStallTimeout)
		if s.done != nil {
			s.done.Add(int64(n))
		}
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

// safeDiskDirName accepts a caller-supplied name only if it is a bare filename
// component that stays inside diskDir, and returns it.
//
// This is the guard DeleteISO already carried, lifted out so it can be applied
// to the one input that never had it: cfg.Name. A domain name arrives in an
// HTTP body, is joined into diskDir to build "<name>-<i>.qcow2" and
// "<name>-seed.iso", and ensureQcow2 does os.MkdirAll(filepath.Dir(path))
// before creating the image -- so a name carrying "../" did not merely write a
// file elsewhere, it CREATED THE DIRECTORIES to write it in.
//
// Every clause earns its place: filepath.Base strips a directory component,
// the empty/"."/".." checks refuse what Base cannot fix, `base != trimmed`
// refuses anything Base had to change (which is what turns this from a
// normalizer into a refusal), and the filepath.Rel containment catches
// whatever the name-level checks miss.
//
// It REFUSES rather than normalizing on purpose. Silently rewriting
// "../../vm" to "vm" would put the disk somewhere the caller did not ask for
// and report success, which is the failure mode this whole batch is about.
func safeDiskDirName(diskDir, name string, what string) (string, error) {
	trimmed := strings.TrimSpace(name)
	base := filepath.Base(trimmed)
	if base == "" || base == "." || base == ".." || base != trimmed {
		return "", fmt.Errorf("invalid %s", what)
	}
	if diskDir != "" {
		full := filepath.Join(diskDir, base)
		if rel, err := filepath.Rel(diskDir, full); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("invalid %s", what)
		}
	}
	return base, nil
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
	base, err := safeDiskDirName(s.diskDir, name, "ISO name")
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.ToLower(base), ".iso") {
		return fmt.Errorf("not an ISO")
	}

	full := filepath.Join(s.diskDir, base)

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
