package v1

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/model"
	"github.com/LeewayOpenSoftware/CasaNova-Common/pkg/rbac"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/common_err"
	commonfile "github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// ---- Alert types ----

type AlertRule struct {
	ID              string  `json:"id"`
	Metric          string  `json:"metric"`
	Operator        string  `json:"operator"`
	Threshold       float64 `json:"threshold"`
	DurationSeconds int     `json:"duration_seconds"`
	Enabled         bool    `json:"enabled"`
	Notify          string  `json:"notify"`
	Label           string  `json:"label"`
}

type AlertEvent struct {
	RuleID      string  `json:"rule_id"`
	TriggeredAt string  `json:"triggered_at"`
	Value       float64 `json:"value"`
	ResolvedAt  string  `json:"resolved_at,omitempty"`
}

type alertConfig struct {
	Rules   []AlertRule  `json:"rules"`
	History []AlertEvent `json:"history"`
}

// ---- Persistence ----

const alertConfigPath = "/var/lib/casanova/alerts.json"

var (
	alertMu    sync.Mutex
	alertCache *alertConfig
)

var validMetrics = map[string]bool{
	"cpu_percent":  true,
	"mem_percent":  true,
	"disk_percent": true,
	"cpu_temp":     true,
	"swap_percent": true,
}

var validOperators = map[string]bool{
	"gt": true,
	"lt": true,
	"eq": true,
}

func defaultAlertRules() []AlertRule {
	return []AlertRule{
		{
			ID:              "cpu-high",
			Metric:          "cpu_percent",
			Operator:        "gt",
			Threshold:       90,
			DurationSeconds: 300,
			Enabled:         true,
			Notify:          "dashboard",
			Label:           "High CPU usage",
		},
		{
			ID:              "mem-high",
			Metric:          "mem_percent",
			Operator:        "gt",
			Threshold:       90,
			DurationSeconds: 300,
			Enabled:         true,
			Notify:          "dashboard",
			Label:           "High memory usage",
		},
		{
			ID:              "disk-high",
			Metric:          "disk_percent",
			Operator:        "gt",
			Threshold:       85,
			DurationSeconds: 0,
			Enabled:         true,
			Notify:          "dashboard",
			Label:           "Disk almost full",
		},
		{
			ID:              "temp-high",
			Metric:          "cpu_temp",
			Operator:        "gt",
			Threshold:       80,
			DurationSeconds: 60,
			Enabled:         true,
			Notify:          "dashboard",
			Label:           "CPU temperature high",
		},
	}
}

func loadAlertConfig() (*alertConfig, error) {
	alertMu.Lock()
	defer alertMu.Unlock()

	if alertCache != nil {
		return alertCache, nil
	}

	data, err := os.ReadFile(alertConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := &alertConfig{
				Rules:   defaultAlertRules(),
				History: []AlertEvent{},
			}
			alertCache = cfg
			_ = saveAlertConfigLocked(cfg)
			return cfg, nil
		}
		return nil, err
	}

	var cfg alertConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Rules == nil {
		cfg.Rules = []AlertRule{}
	}
	if cfg.History == nil {
		cfg.History = []AlertEvent{}
	}
	alertCache = &cfg
	return alertCache, nil
}

func saveAlertConfigLocked(cfg *alertConfig) error {
	dir := filepath.Dir(alertConfigPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// Wave 35-R: previously this was write-tmp + fsync(file) + rename,
	// which misses the parent-directory fsync. On btrfs/xfs/zfs the
	// rename and the data can land in separate transactions; a power
	// loss between them yields a renamed-but-empty alerts.json and the
	// user's entire alert rule set disappears at once. The event
	// history (previous incidents) is stored in the same file, so the
	// loss is total — there is no per-rule recovery. commonfile.WriteFileAtomic
	// does the full write-tmp + fsync(file) + rename + fsync(parent dir)
	// sequence so the rename is durable before we return success.
	return commonfile.WriteFileAtomic(alertConfigPath, data, 0o644)
}

func saveAlertConfig(cfg *alertConfig) error {
	alertMu.Lock()
	defer alertMu.Unlock()
	err := saveAlertConfigLocked(cfg)
	// Invalidate the cache so the next load re-reads from disk. This
	// prevents stale-cache races when two concurrent handlers both load,
	// mutate, and save — the second save won't silently lose the first.
	alertCache = nil
	return err
}

func invalidateAlertCache() {
	alertMu.Lock()
	defer alertMu.Unlock()
	alertCache = nil
}

// ---- Validation ----

func validateAlertRule(rule *AlertRule) string {
	if !validMetrics[rule.Metric] {
		return "invalid metric: must be one of cpu_percent, mem_percent, disk_percent, cpu_temp, swap_percent"
	}
	if !validOperators[rule.Operator] {
		return "invalid operator: must be one of gt, lt, eq"
	}
	if rule.Threshold < 0 {
		return "threshold must be >= 0"
	}
	// Percentage metrics capped at 100
	if rule.Metric != "cpu_temp" && rule.Threshold > 100 {
		return "threshold for percentage metrics must be <= 100"
	}
	// Temperature upper bound sanity check
	if rule.Metric == "cpu_temp" && rule.Threshold > 150 {
		return "threshold for cpu_temp must be <= 150"
	}
	if rule.DurationSeconds < 0 {
		return "duration_seconds must be >= 0"
	}
	if rule.Label == "" {
		return "label is required"
	}
	if rule.Notify == "" {
		rule.Notify = "dashboard"
	}
	return ""
}

// ---- Handlers ----

// GET /v1/sys/alerts/rules — List all alert rules.
func GetAlertRules(ctx echo.Context) error {
	// Gated: alert thresholds describe the box's monitoring posture; PermSystemRead is held by guest.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}

	cfg, err := loadAlertConfig()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to load alert config: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    cfg.Rules,
	})
}

// POST /v1/sys/alerts/rules — Create a new alert rule.
func PostAlertRule(ctx echo.Context) error {
	// Config mutation was ungated.
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	var rule AlertRule
	if err := ctx.Bind(&rule); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	if msg := validateAlertRule(&rule); msg != "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: msg,
		})
	}

	rule.ID = uuid.New().String()

	cfg, err := loadAlertConfig()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to load alert config: " + err.Error(),
		})
	}

	cfg.Rules = append(cfg.Rules, rule)
	if err := saveAlertConfig(cfg); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to save alert config: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    rule,
	})
}

// PUT /v1/sys/alerts/rules/:id — Update an existing alert rule.
func PutAlertRule(ctx echo.Context) error {
	// Config mutation was ungated.
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	id := ctx.Param("id")

	var update AlertRule
	if err := ctx.Bind(&update); err != nil {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "invalid request body",
		})
	}

	if msg := validateAlertRule(&update); msg != "" {
		return ctx.JSON(http.StatusBadRequest, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: msg,
		})
	}

	cfg, err := loadAlertConfig()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to load alert config: " + err.Error(),
		})
	}

	found := false
	for i, r := range cfg.Rules {
		if r.ID == id {
			update.ID = id
			cfg.Rules[i] = update
			found = true
			break
		}
	}

	if !found {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "rule not found",
		})
	}

	if err := saveAlertConfig(cfg); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to save alert config: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    update,
	})
}

// DELETE /v1/sys/alerts/rules/:id — Delete an alert rule.
func DeleteAlertRule(ctx echo.Context) error {
	// Config mutation was ungated.
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	id := ctx.Param("id")

	cfg, err := loadAlertConfig()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to load alert config: " + err.Error(),
		})
	}

	found := false
	for i, r := range cfg.Rules {
		if r.ID == id {
			cfg.Rules = append(cfg.Rules[:i], cfg.Rules[i+1:]...)
			found = true
			break
		}
	}

	if !found {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "rule not found",
		})
	}

	if err := saveAlertConfig(cfg); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to save alert config: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
	})
}

// GET /v1/sys/alerts/history — Get alert trigger history.
func GetAlertHistory(ctx echo.Context) error {
	// Gated: history of fired alerts; PermSystemRead is held by guest.
	if err := requireSystemPrincipal(ctx, rbac.PermSystemRead); err != nil {
		return err
	}

	cfg, err := loadAlertConfig()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to load alert config: " + err.Error(),
		})
	}

	history := cfg.History

	// Filter by ?since= query parameter.
	since := ctx.QueryParam("since")
	if since != "" {
		sinceTime, err := time.Parse(time.RFC3339, since)
		if err == nil {
			var filtered []AlertEvent
			for _, ev := range history {
				t, err := time.Parse(time.RFC3339, ev.TriggeredAt)
				if err == nil && !t.Before(sinceTime) {
					filtered = append(filtered, ev)
				}
			}
			history = filtered
		}
	}

	// Cap at 100 entries, newest first.
	if len(history) > 100 {
		history = history[len(history)-100:]
	}

	// Reverse to newest-first order.
	for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
		history[i], history[j] = history[j], history[i]
	}

	if history == nil {
		history = []AlertEvent{}
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: common_err.GetMsg(common_err.SUCCESS),
		Data:    history,
	})
}

// POST /v1/sys/alerts/test/:id — Test a rule by generating a fake alert event.
func PostAlertTest(ctx echo.Context) error {
	// Same gate as every sibling alert mutation. A test-fire is not read-only:
	// it sends real messages through the configured channel — an ungated
	// endpoint here is a spam primitive wearing the box's identity.
	if err := requirePerm(ctx, rbac.PermSystemConfig); err != nil {
		return err
	}
	id := ctx.Param("id")

	cfg, err := loadAlertConfig()
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to load alert config: " + err.Error(),
		})
	}

	var rule *AlertRule
	for i := range cfg.Rules {
		if cfg.Rules[i].ID == id {
			rule = &cfg.Rules[i]
			break
		}
	}

	if rule == nil {
		return ctx.JSON(http.StatusNotFound, model.Result{
			Success: common_err.CLIENT_ERROR,
			Message: "rule not found",
		})
	}

	now := time.Now().UTC().Format(time.RFC3339)
	event := AlertEvent{
		RuleID:      rule.ID,
		TriggeredAt: now,
		Value:       rule.Threshold + 1,
		ResolvedAt:  now,
	}

	cfg.History = append(cfg.History, event)
	if err := saveAlertConfig(cfg); err != nil {
		return ctx.JSON(http.StatusInternalServerError, model.Result{
			Success: common_err.SERVICE_ERROR,
			Message: "failed to save alert config: " + err.Error(),
		})
	}

	return ctx.JSON(http.StatusOK, model.Result{
		Success: common_err.SUCCESS,
		Message: "Test alert sent",
		Data:    event,
	})
}
