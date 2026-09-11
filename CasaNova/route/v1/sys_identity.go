package v1

// System-identity endpoints — hostname & timezone.
//
// Kept in their own file because they each shell out to a different
// systemd CLI (hostnamectl / timedatectl) and the surrounding system.go
// is already large.
//
//   GET  /v1/sys/hostname    → { hostname: string }
//   PUT  /v1/sys/hostname    { hostname: "new-name" }
//   GET  /v1/sys/timezone    → { timezone: "Europe/Madrid", list: [...] }
//   PUT  /v1/sys/timezone    { timezone: "Europe/Madrid" }
//
// All four require PermSystemConfig (admin-only). Both PUTs reject any
// value that doesn't survive a strict regex/whitelist check — the values
// flow into a shell command, so this is the only sanitisation layer.

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	commonmw "github.com/LeewayOpenSoftware/CasaNova-Common/middleware"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova/model"
	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/common_err"
	"github.com/labstack/echo/v4"
)

// hostnameRe matches a valid Linux hostname: 1–63 chars, ASCII letters /
// digits / hyphens, no leading/trailing hyphen. This is stricter than
// RFC 1123 (we forbid dots; FQDN editing is a different control).
var hostnameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// tzRe matches valid IANA timezone identifiers: two OR MORE segments
// separated by "/", each segment ASCII letters / digits / "_" / "-" / "+".
// Three-segment zones are real and common (America/Argentina/Buenos_Aires,
// America/Indiana/Indianapolis, …) and timedatectl accepts them. Bars a
// surprising amount of shell-meta while covering every real zone in tzdata.
var tzRe = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)+$`)

func GetSystemHostname(ctx echo.Context) error {
	// Guest holds PermSystemRead, but hostname/timezone have no internal peer
	// caller and the UI only reads them from Settings (post-login). Require a
	// real session so an unauthenticated or guest caller can't fingerprint
	// the box (UserIDFromContext is 0 unless the JWT middleware validated a token).
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	name, err := os.ReadFile("/etc/hostname")
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to read /etc/hostname: " + err.Error(),
		})
	}
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]string{
			"hostname": strings.TrimSpace(string(name)),
		},
	})
}

func PutSystemHostname(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	body := map[string]string{}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	name := strings.TrimSpace(body["hostname"])
	if !hostnameRe.MatchString(name) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "hostname must be 1–63 chars, letters/digits/hyphens, no leading or trailing hyphen",
		})
	}

	// systemd CLIs that hit dbus can hang under bus congestion; 30s
	// covers a sluggish reload without locking the operator out.
	cmdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "hostnamectl", "set-hostname", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "hostnamectl failed: " + err.Error() + ": " + string(out),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    map[string]string{"hostname": name},
	})
}

func GetSystemTimezone(ctx echo.Context) error {
	// Guest holds PermSystemRead, but hostname/timezone have no internal peer
	// caller and the UI only reads them from Settings (post-login). Require a
	// real session so an unauthenticated or guest caller can't fingerprint
	// the box (UserIDFromContext is 0 unless the JWT middleware validated a token).
	if commonmw.UserIDFromContext(ctx) == 0 {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	tz := readCurrentTimezone()
	zones := listTimezones()
	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data: map[string]interface{}{
			"timezone": tz,
			"list":     zones,
		},
	})
}

func PutSystemTimezone(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	body := map[string]string{}
	if err := ctx.Bind(&body); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: err.Error(),
		})
	}
	tz := strings.TrimSpace(body["timezone"])
	if !tzRe.MatchString(tz) {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "timezone must be an IANA identifier like 'Europe/Madrid'",
		})
	}
	// Defence in depth — the tzdata zoneinfo file must exist. Catches
	// typos like "Europe/Madird" and prevents timedatectl from emitting
	// a less helpful error.
	if _, err := os.Stat("/usr/share/zoneinfo/" + tz); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.INVALID_PARAMS,
			Message: "unknown timezone " + tz,
		})
	}

	tzCtx, tzCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer tzCancel()
	tzCmd := exec.CommandContext(tzCtx, "timedatectl", "set-timezone", tz)
	if out, err := tzCmd.CombinedOutput(); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "timedatectl failed: " + err.Error() + ": " + string(out),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    map[string]string{"timezone": tz},
	})
}

// readCurrentTimezone resolves the /etc/localtime symlink (what the system
// ACTUALLY uses, updated atomically by timedatectl), falling back to
// /etc/timezone for systems without the symlink. Order matters: preferring
// the file showed a stale zone right after a successful set — timedatectl
// swaps the symlink but doesn't always rewrite /etc/timezone.
func readCurrentTimezone() string {
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		const prefix = "/usr/share/zoneinfo/"
		if idx := strings.Index(target, prefix); idx >= 0 {
			return target[idx+len(prefix):]
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return ""
}

// listTimezones walks /usr/share/zoneinfo RECURSIVELY and returns every
// multi-segment IANA zone (matching tzRe), sorted. Zones can be three
// segments deep (America/Argentina/Buenos_Aires) — the previous two-level
// walk listed the intermediate DIRECTORY ("America/Argentina") as if it
// were a zone (timedatectl rejects it with "Invalid or not installed")
// and never offered the real zones beneath it. Only zone FILES count as
// zones now. Single-segment zones like "UTC" and the alias dirs stay
// excluded — they confuse the UI dropdown.
func listTimezones() []string {
	out := []string{}
	root := "/usr/share/zoneinfo"
	skip := map[string]bool{"Etc": true, "right": true, "posix": true, "SystemV": true}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, never abort the whole list
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil || rel == "." {
			return nil
		}
		if d.IsDir() {
			if skip[rel] { // exact match ⇒ top-level alias dirs only
				return fs.SkipDir
			}
			return nil
		}
		// Zone files are regular (mostly hardlinks); a few aliases are
		// symlinks — both are fine. Anything else (sockets, etc.) is not.
		if !d.Type().IsRegular() && d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		if tzRe.MatchString(rel) {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}
