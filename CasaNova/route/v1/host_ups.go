package v1

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	commonfile "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/labstack/echo/v4"
)

// UPS status via NUT (upsc) or apcupsd (apcaccess). Supports a REMOTE NUT
// server: many home labs run upsd on a dedicated box (here 192.0.2.10) and
// every other host just queries it read-only. When a remote host is configured
// we run `upsc <name>@<host>`; otherwise we probe the local daemon. host/name
// are validated to a safe charset and passed as separate argv (no shell), so
// there is no injection path.
var upsConfigFile = "/var/lib/casanova/ups-config.json"

// hostname / IPv4 / IPv6-in-brackets, plus the NUT ups name charset.
var upsHostRe = regexp.MustCompile(`^[a-zA-Z0-9._:\[\]-]{1,255}$`)
var upsNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

type UPSConfig struct {
	Host    string `json:"host"`    // remote NUT host, "" = local
	UPSName string `json:"upsName"` // specific UPS name, "" = first found
}

type UPSInfo struct {
	Detected     bool    `json:"detected"`
	Tool         string  `json:"tool"`   // "nut" | "apcupsd" | ""
	Remote       bool    `json:"remote"` // queried a remote host
	Host         string  `json:"host"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	ChargePct    float64 `json:"chargePct"`
	LoadPct      float64 `json:"loadPct"`
	RuntimeSec   int     `json:"runtimeSec"`
	InputVoltage float64 `json:"inputVoltage"`
	Model        string  `json:"model"`
	Error        string  `json:"error,omitempty"` // reachability/parse error for the UI
}

func loadUPSConfig() UPSConfig {
	var c UPSConfig
	if raw, err := os.ReadFile(upsConfigFile); err == nil {
		_ = json.Unmarshal(raw, &c)
	}
	return c
}

// GET /v1/sys/host/ups/config
func GetUPSConfig(ctx echo.Context) error {
	// requireSystemPrincipal: see route/v1/antivirus.go's GetAntivirusStatus
	// comment — guest holds PermSystemRead, so a bare requirePerm never
	// refused a tokenless caller.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: loadUPSConfig()})
}

// PUT /v1/sys/host/ups/config  body: {"host":"192.0.2.10","upsName":"ups"}
func PutUPSConfig(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var c UPSConfig
	if err := ctx.Bind(&c); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.CLIENT_ERROR, Message: err.Error()})
	}
	c.Host = strings.TrimSpace(c.Host)
	c.UPSName = strings.TrimSpace(c.UPSName)
	if c.Host != "" && !upsHostRe.MatchString(c.Host) {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.CLIENT_ERROR, Message: "invalid host"})
	}
	if c.UPSName != "" && !upsNameRe.MatchString(c.UPSName) {
		return ctx.JSON(http.StatusBadRequest, model.Result{Success: common_err.CLIENT_ERROR, Message: "invalid UPS name"})
	}
	// Every step of the persist is reported. This handler answers by calling
	// GetHostUPS, which re-reads upsConfigFile from disk — so a discarded
	// write error returned the PREVIOUS configuration under a 200, and the
	// user was told their UPS was configured while nothing had been stored.
	// The consequence is not cosmetic: no configuration means no monitoring,
	// and no monitoring means no clean shutdown when the power actually goes.
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to encode UPS config: " + err.Error(),
		})
	}
	// commonfile.WriteFileAtomic does write-tmp + fsync(file) + close + rename
	// + fsync(parent dir) (it also creates the parent directory). A plain
	// os.WriteFile leaves a window where a crash mid-write, or a crash after
	// the write but before the page cache flushes, can leave upsConfigFile
	// truncated or torn; loadUPSConfig ignores json.Unmarshal errors on a
	// corrupt file and silently falls back to a zero-value config, so the
	// box would quietly stop monitoring the UPS with no error surfaced
	// anywhere.
	if err := commonfile.WriteFileAtomic(upsConfigFile, raw, 0o644); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to save UPS config: " + err.Error(),
		})
	}
	return GetHostUPS(ctx)
}

// GET /v1/sys/host/ups
func GetHostUPS(ctx echo.Context) error {
	// requireSystemPrincipal: see GetUPSConfig above.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	cfg := loadUPSConfig()
	var info UPSInfo
	if cfg.Host != "" {
		info = probeNUTRemote(cfg.Host, cfg.UPSName)
	} else {
		if _, err := exec.LookPath("upsc"); err == nil {
			info = probeNUT("", cfg.UPSName)
		}
		if !info.Detected {
			if _, err := exec.LookPath("apcaccess"); err == nil {
				info = probeAPC()
			}
		}
	}
	return ctx.JSON(http.StatusOK, model.Result{Success: common_err.SUCCESS, Message: common_err.GetMsg(common_err.SUCCESS), Data: info})
}

// probeNUTRemote queries a remote upsd. Even without a local NUT install the
// upsc client binary is enough; if it is missing we say so cleanly.
func probeNUTRemote(host, name string) UPSInfo {
	info := UPSInfo{Tool: "nut", Remote: true, Host: host}
	if _, err := exec.LookPath("upsc"); err != nil {
		info.Error = "upsc client not installed on this host"
		return info
	}
	return probeNUT(host, name)
}

// probeNUT runs upsc against a local ("") or remote host. name "" => first UPS
// reported by `upsc -l [host]`.
func probeNUT(host, name string) UPSInfo {
	info := UPSInfo{Tool: "nut", Host: host, Remote: host != ""}
	if name == "" {
		listArgs := []string{"-l"}
		if host != "" {
			listArgs = append(listArgs, host)
		}
		out, err := execOutBounded(15*time.Second, "upsc", listArgs...)
		if err != nil {
			info.Error = "cannot reach upsd" + hostSuffix(host)
			return info
		}
		names := strings.Fields(strings.TrimSpace(string(out)))
		if len(names) == 0 {
			info.Error = "no UPS configured on upsd" + hostSuffix(host)
			return info
		}
		name = names[0]
	}
	info.Name = name
	// upsc target syntax: <upsname>[@<host>]
	target := name
	if host != "" {
		target = name + "@" + host
	}
	vals, err := execOutBounded(15*time.Second, "upsc", target)
	if err != nil {
		info.Error = "cannot query " + target
		return info
	}
	kv := parseColonKV(string(vals))
	info.Detected = true
	info.Status = kv["ups.status"]
	info.Model = strings.TrimSpace(kv["ups.model"] + " " + kv["device.model"])
	info.ChargePct = atof(kv["battery.charge"])
	info.LoadPct = atof(kv["ups.load"])
	info.RuntimeSec = int(atof(kv["battery.runtime"]))
	info.InputVoltage = atof(kv["input.voltage"])
	return info
}

func hostSuffix(host string) string {
	if host == "" {
		return ""
	}
	return " at " + host
}

func probeAPC() UPSInfo {
	info := UPSInfo{Tool: "apcupsd"}
	out, err := execOutBounded(15*time.Second, "apcaccess", "status")
	if err != nil {
		return info
	}
	kv := parseColonKV(string(out))
	if len(kv) == 0 {
		return info
	}
	info.Detected = true
	info.Status = kv["STATUS"]
	info.Model = kv["MODEL"]
	info.ChargePct = atofUnit(kv["BCHARGE"])
	info.LoadPct = atofUnit(kv["LOADPCT"])
	info.RuntimeSec = int(atofUnit(kv["TIMELEFT"]) * 60)
	info.InputVoltage = atofUnit(kv["LINEV"])
	return info
}

func parseColonKV(s string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		v := strings.TrimSpace(line[i+1:])
		if k != "" {
			m[k] = v
		}
	}
	return m
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func atofUnit(s string) float64 {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	return atof(fields[0])
}
