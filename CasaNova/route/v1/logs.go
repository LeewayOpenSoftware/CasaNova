package v1

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/labstack/echo/v4"
)

// ---- System Logs (journalctl) ----

// LogEntry is a single parsed journal entry returned to the frontend.
type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Unit      string `json:"unit"`
	Priority  int    `json:"priority"`
	Message   string `json:"message"`
}

// journalctlRaw represents one JSON line from `journalctl -o json`.
type journalctlRaw struct {
	RealtimeTimestamp string `json:"__REALTIME_TIMESTAMP"` // microseconds since epoch
	SyslogIdentifier  string `json:"SYSLOG_IDENTIFIER"`
	SystemdUnit       string `json:"_SYSTEMD_UNIT"`
	Priority          string `json:"PRIORITY"`
	Message           string `json:"MESSAGE"`
}

// validPriorities lists the journalctl -p values the frontend is
// allowed to request. Anything outside this set is silently ignored.
var validPriorities = map[string]bool{
	"emerg":   true,
	"alert":   true,
	"crit":    true,
	"err":     true,
	"warning": true,
	"notice":  true,
	"info":    true,
	"debug":   true,
}

// GET /v1/sys/logs — Fetch system logs from journalctl.
//
// Query params:
//   - unit    — systemd unit name (e.g. "casanova", "docker")
//   - lines   — number of lines to return (default 200, max 5000)
//   - priority — minimum priority filter (emerg..debug)
//   - since   — journalctl --since value (e.g. "1 hour ago", "today")
func GetSystemLogs(ctx echo.Context) error {
	// Journal messages can contain credentials and other secrets emitted by any
	// service. Treat reading them like the daemon log tail: an identified
	// system administrator, not a guest who holds the general read bit.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	if _, err := exec.LookPath("journalctl"); err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "journalctl not found",
			Data:    []LogEntry{},
		})
	}

	// Build command arguments.
	args := []string{"--no-pager", "-o", "json"}

	// Unit filter.
	unit := ctx.QueryParam("unit")
	if unit != "" {
		// Sanitize: only allow alphanumeric, dash, underscore, dot, @.
		if isValidUnitName(unit) {
			args = append(args, "--unit="+unit)
		}
	}

	// Number of lines.
	lines := 200
	if l := ctx.QueryParam("lines"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 5000 {
			lines = n
		}
	}
	args = append(args, "-n", strconv.Itoa(lines))

	// Priority filter.
	if p := ctx.QueryParam("priority"); p != "" && validPriorities[p] {
		args = append(args, "-p", p)
	}

	// Since filter.
	if since := ctx.QueryParam("since"); since != "" {
		// Translate shorthand values the frontend sends.
		switch since {
		case "1h":
			args = append(args, "--since=1 hour ago")
		case "today":
			args = append(args, "--since=today")
		case "24h":
			args = append(args, "--since=24 hours ago")
		case "7d":
			args = append(args, "--since=7 days ago")
		default:
			// Only allow ISO date patterns (e.g. "2026-04-08" or "2026-04-08 14:30").
			// Reject anything else to prevent command injection via journalctl.
			if matched, _ := regexp.MatchString(`^\d{4}-\d{2}-\d{2}( \d{2}:\d{2}(:\d{2})?)?$`, since); matched {
				args = append(args, "--since="+since)
			}
		}
	}

	// journalctl can stall on a slow/large journal. 60s is generous for
	// any reasonable line count without locking the operator out.
	jctlCtx, jctlCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer jctlCancel()
	out, err := exec.CommandContext(jctlCtx, "journalctl", args...).Output()
	if jctlCtx.Err() == context.DeadlineExceeded {
		return ctx.JSON(http.StatusGatewayTimeout, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "journalctl timed out after 60s",
			Data:    []LogEntry{},
		})
	}
	if err != nil {
		// journalctl may exit non-zero with partial output; try to parse
		// whatever we got. If truly empty, return an error message.
		if len(out) == 0 {
			return ctx.JSON(http.StatusOK, model.Result{
				Success: common_err.SUCCESS,
				Message: "journalctl returned no output: " + err.Error(),
				Data:    []LogEntry{},
			})
		}
	}

	// Parse JSON-lines output. Each line is a self-contained JSON object.
	var entries []LogEntry
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	// Increase scanner buffer for potentially long log messages.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var raw journalctlRaw
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}

		entry := LogEntry{
			Timestamp: parseJournalTimestamp(raw.RealtimeTimestamp),
			Unit:      raw.SystemdUnit,
			Message:   raw.Message,
		}
		if entry.Unit == "" {
			entry.Unit = raw.SyslogIdentifier
		}

		if p, err := strconv.Atoi(raw.Priority); err == nil {
			entry.Priority = p
		}

		entries = append(entries, entry)
	}

	if entries == nil {
		entries = []LogEntry{}
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    entries,
	})
}

// GET /v1/sys/logs/units — List available systemd service units.
func GetLogUnits(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	// systemctl list-units occasionally stalls when a unit is in a
	// transitioning state (deactivating with a long-running ExecStop).
	// 20s is plenty for a healthy host and bounds the worst case.
	sctlCtx, sctlCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer sctlCancel()
	out, err := exec.CommandContext(sctlCtx,
		"systemctl", "list-units", "--type=service", "--no-pager", "--plain", "--no-legend",
	).Output()
	if err != nil {
		return ctx.JSON(http.StatusOK, model.Result{
			Success: common_err.SUCCESS,
			Message: "failed to list units: " + err.Error(),
			Data:    []string{},
		})
	}

	var units []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Each line starts with the unit name, followed by load/active/sub/description.
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		unitName := fields[0]
		// Return all service units — the frontend can filter further.
		units = append(units, unitName)
	}

	if units == nil {
		units = []string{}
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    units,
	})
}

// parseJournalTimestamp converts the __REALTIME_TIMESTAMP (microseconds
// since Unix epoch) into an RFC3339 string.
func parseJournalTimestamp(usec string) string {
	us, err := strconv.ParseInt(usec, 10, 64)
	if err != nil {
		return usec
	}
	sec := us / 1_000_000
	nsec := (us % 1_000_000) * 1_000
	return time.Unix(sec, nsec).UTC().Format(time.RFC3339)
}

// isValidUnitName returns true if the string is safe to pass as a
// --unit argument to journalctl. Only allows alphanumeric characters,
// dashes, underscores, dots, and @ signs.
func isValidUnitName(s string) bool {
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '@' || c == '*') {
			return false
		}
	}
	return len(s) > 0 && len(s) <= 256
}
