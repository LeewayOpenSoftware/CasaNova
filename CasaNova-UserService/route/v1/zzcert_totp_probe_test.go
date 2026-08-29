package v1

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/filelock"
	"golang.org/x/crypto/bcrypt"
)

// CERTIFIER PROBE (e): the same backup code, consumed from several REAL
// processes at once. Written from scratch: it does not call the author's
// harness, so a bug shared between the fixture and the code under test cannot
// make both agree.
//
// Two of the children run the production UserService path
// (consumeUserBackupCode). The other two emulate the CasaNova service: take the
// same sidecar lock, read, mark used, publish by rename. Each publishes through
// its OWN temp name so a collision on the shared temp file can never be
// mistaken for the exclusion working.

type certCfg struct {
	Enabled     bool          `json:"enabled"`
	Secret      string        `json:"secret"`
	Verified    bool          `json:"verified"`
	BackupCodes []certBackupC `json:"backup_codes"`
}

type certBackupC struct {
	Hash string `json:"hash"`
	Used bool   `json:"used"`
}

// TestCertChildConsumer is the child entry point. It is inert unless the
// environment names a config to work on.
func TestCertChildConsumer(t *testing.T) {
	cfgPath := os.Getenv("CERT_CFG")
	if cfgPath == "" {
		t.Skip("child helper")
	}
	userTOTPConfigFile = cfgPath
	code := os.Getenv("CERT_CODE")
	gate := os.Getenv("CERT_GATE")
	mode := os.Getenv("CERT_MODE")
	out := os.Getenv("CERT_OUT")

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		time.Sleep(200 * time.Microsecond)
	}

	var err error
	switch mode {
	case "user":
		err = consumeUserBackupCode(strings.ToUpper(code))
	case "peer":
		err = certPeerConsume(cfgPath, strings.ToUpper(code))
	default:
		t.Fatalf("bad mode %q", mode)
	}
	verdict := "consumed"
	if err != nil {
		verdict = "refused: " + err.Error()
	}
	if wErr := os.WriteFile(out, []byte(verdict), 0o644); wErr != nil {
		t.Fatalf("write result: %v", wErr)
	}
}

// certPeerConsume is the CasaNova-side shape of the same operation, written
// independently of the code under test.
func certPeerConsume(path, codeUpper string) error {
	lk, err := filelock.Acquire(path, 10*time.Second)
	if err != nil {
		return err
	}
	defer lk.Release()

	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg certCfg
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	for i := range cfg.BackupCodes {
		bc := &cfg.BackupCodes[i]
		if bc.Used || bc.Hash == "" {
			continue
		}
		if bcrypt.CompareHashAndPassword([]byte(bc.Hash), []byte(codeUpper)) != nil {
			continue
		}
		bc.Used = true
		out, mErr := json.MarshalIndent(&cfg, "", "  ")
		if mErr != nil {
			return mErr
		}
		tmp := fmt.Sprintf("%s.cert-%d.tmp", path, os.Getpid())
		if wErr := os.WriteFile(tmp, out, 0o600); wErr != nil {
			return wErr
		}
		return os.Rename(tmp, path)
	}
	return errTOTPInvalid
}

func certWriteFixture(t *testing.T, dir string, plain []string) string {
	t.Helper()
	cfg := certCfg{Enabled: true, Verified: true, Secret: "JBSWY3DPEHPK3PXP"}
	for _, p := range plain {
		h, err := bcrypt.GenerateFromPassword([]byte(strings.ToUpper(p)), bcrypt.MinCost)
		if err != nil {
			t.Fatalf("bcrypt: %v", err)
		}
		cfg.BackupCodes = append(cfg.BackupCodes, certBackupC{Hash: string(h)})
	}
	raw, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(dir, "2fa.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func certReadFixture(t *testing.T, path string) certCfg {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var cfg certCfg
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal (%q): %v", string(raw), err)
	}
	return cfg
}

func TestCertSameBackupCodeFromConcurrentProcesses(t *testing.T) {
	dir := t.TempDir()
	// four distinct codes; every child races for the SAME one.
	path := certWriteFixture(t, dir, []string{"AAAA1111", "BBBB2222", "CCCC3333", "DDDD4444"})
	gate := filepath.Join(dir, "gate")

	modes := []string{"user", "user", "peer", "peer"}
	outs := make([]string, len(modes))
	var wg sync.WaitGroup
	var mu sync.Mutex
	failures := []string{}

	for i, mode := range modes {
		outs[i] = filepath.Join(dir, fmt.Sprintf("out-%d", i))
		cmd := exec.Command(os.Args[0], "-test.run=^TestCertChildConsumer$", "-test.timeout=120s")
		cmd.Env = append(os.Environ(),
			"CERT_CFG="+path,
			"CERT_CODE=BBBB2222",
			"CERT_GATE="+gate,
			"CERT_MODE="+mode,
			"CERT_OUT="+outs[i],
		)
		wg.Add(1)
		go func(i int, cmd *exec.Cmd) {
			defer wg.Done()
			b, err := cmd.CombinedOutput()
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("child %d: %v\n%s", i, err, b))
				mu.Unlock()
			}
		}(i, cmd)
	}

	// let every child reach the gate, then release them together
	time.Sleep(400 * time.Millisecond)
	if err := os.WriteFile(gate, []byte("go"), 0o644); err != nil {
		t.Fatalf("gate: %v", err)
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("child process failed:\n%s", strings.Join(failures, "\n"))
	}

	consumed := 0
	verdicts := []string{}
	for i, o := range outs {
		raw, err := os.ReadFile(o)
		if err != nil {
			t.Fatalf("child %d produced no verdict: %v", i, err)
		}
		verdicts = append(verdicts, fmt.Sprintf("%s#%d=%s", modes[i], i, string(raw)))
		if string(raw) == "consumed" {
			consumed++
		}
	}
	if consumed != 1 {
		t.Fatalf("CERT: %d of %d processes accepted the SAME backup code, want exactly 1: %v",
			consumed, len(modes), verdicts)
	}

	cfg := certReadFixture(t, path)
	used := 0
	for _, bc := range cfg.BackupCodes {
		if bc.Used {
			used++
		}
	}
	if used != 1 {
		t.Fatalf("CERT: the file on disk records %d used codes after 1 accepted consumption; "+
			"verdicts=%v", used, verdicts)
	}
	if len(cfg.BackupCodes) != 4 {
		t.Fatalf("CERT: the file lost codes (%d of 4 left)", len(cfg.BackupCodes))
	}
}

// The second half of (e): FOUR consumptions of FOUR DIFFERENT codes racing must
// all reach disk. A lock that made only one winner by losing the other writes
// would pass the test above.
func TestCertFourDistinctConsumptionsAllSurvive(t *testing.T) {
	dir := t.TempDir()
	codes := []string{"AAAA1111", "BBBB2222", "CCCC3333", "DDDD4444"}
	path := certWriteFixture(t, dir, codes)
	gate := filepath.Join(dir, "gate")

	modes := []string{"user", "peer", "user", "peer"}
	outs := make([]string, len(codes))
	var wg sync.WaitGroup
	for i, code := range codes {
		outs[i] = filepath.Join(dir, fmt.Sprintf("out-%d", i))
		cmd := exec.Command(os.Args[0], "-test.run=^TestCertChildConsumer$", "-test.timeout=120s")
		cmd.Env = append(os.Environ(),
			"CERT_CFG="+path,
			"CERT_CODE="+code,
			"CERT_GATE="+gate,
			"CERT_MODE="+modes[i],
			"CERT_OUT="+outs[i],
		)
		wg.Add(1)
		go func(cmd *exec.Cmd) {
			defer wg.Done()
			_, _ = cmd.CombinedOutput()
		}(cmd)
	}
	time.Sleep(400 * time.Millisecond)
	if err := os.WriteFile(gate, []byte("go"), 0o644); err != nil {
		t.Fatalf("gate: %v", err)
	}
	wg.Wait()

	accepted := 0
	for _, o := range outs {
		raw, err := os.ReadFile(o)
		if err == nil && string(raw) == "consumed" {
			accepted++
		}
	}
	cfg := certReadFixture(t, path)
	used := 0
	for _, bc := range cfg.BackupCodes {
		if bc.Used {
			used++
		}
	}
	if used != accepted {
		t.Fatalf("CERT: %d consumptions were accepted but only %d are marked used on disk; "+
			"%d consumption(s) were lost", accepted, used, accepted-used)
	}
	if accepted != 4 {
		t.Fatalf("CERT: only %d of 4 distinct codes were accepted", accepted)
	}
}
