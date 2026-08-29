// Package smart provides SMART disk health monitoring.
// Uses smartctl's JSON output mode (v7+) for structured, parseable data.
//
// Health assessment logic inspired by OpenMediaVault's SMART implementation:
//   - Checks reallocated sectors, pending sectors, uncorrectable errors
//   - Computes an overall health status: GOOD / WARNING / CRITICAL / UNKNOWN
//   - Caches results per device to avoid hammering disks
package smart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// smartctlTimeout caps how long we'll wait for smartctl to return.
// On a healthy disk the call completes in well under a second; the
// long tail is bad-controller / dying-disk territory where smartctl
// goes into an uninterruptible sleep waiting on the kernel ATA
// timeout (which can be 30+ seconds per command, multiplied by every
// attribute it tries to read). Without an explicit context kill the
// HTTP request that triggered the query stays pinned indefinitely
// and the cache mutex below is held for the duration.
const smartctlTimeout = 30 * time.Second

// Assessment represents the overall health status of a disk.
type Assessment string

const (
	AssessmentGood     Assessment = "GOOD"
	AssessmentWarning  Assessment = "WARNING"  // Past issues detected
	AssessmentCritical Assessment = "CRITICAL" // Active failures
	AssessmentUnknown  Assessment = "UNKNOWN"  // SMART not supported or unavailable
)

// DiskHealth contains the parsed SMART health data for a single disk.
type DiskHealth struct {
	DevicePath   string     `json:"device_path"`
	Supported    bool       `json:"supported"`
	Assessment   Assessment `json:"assessment"`
	Passed       bool       `json:"passed"`      // smartctl overall-health test
	Temperature  int        `json:"temperature"` // Celsius, 0 if unavailable
	PowerOnHours int        `json:"power_on_hours"`
	PowerCycles  int        `json:"power_cycles"`

	// Critical indicators
	ReallocatedSectors int `json:"reallocated_sectors"` // Bad sectors remapped
	PendingSectors     int `json:"pending_sectors"`     // Sectors waiting for remap
	UncorrectableErrs  int `json:"uncorrectable_errors"`
	CRCErrors          int `json:"crc_errors"` // Cable/connection issues

	// Raw attributes for advanced users
	Attributes []Attribute `json:"attributes,omitempty"`

	QueriedAt time.Time `json:"queried_at"`
}

// Attribute is a single SMART attribute.
type Attribute struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Value     int    `json:"value"`
	Worst     int    `json:"worst"`
	Threshold int    `json:"threshold"`
	RawValue  int64  `json:"raw_value"`
	Failing   bool   `json:"failing"` // value <= threshold
}

// smartctl JSON output structures (v7+)
type smartctlOutput struct {
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current int `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours int `json:"hours"`
	} `json:"power_on_time"`
	PowerCycleCount    int `json:"power_cycle_count"`
	ATASmartAttributes *struct {
		Table []struct {
			ID     int    `json:"id"`
			Name   string `json:"name"`
			Value  int    `json:"value"`
			Worst  int    `json:"worst"`
			Thresh int    `json:"thresh"`
			Raw    struct {
				Value int64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	SmartctlExitStatus int `json:"smartctl_exit_status"`
}

// Cache to avoid hammering disks with smartctl on every API call
var (
	cache    = make(map[string]*DiskHealth)
	cacheMu  sync.RWMutex
	cacheTTL = 5 * time.Minute
)

// Query returns the SMART health data for a device.
// Results are cached for 5 minutes to avoid excessive disk queries.
func Query(devicePath string) (*DiskHealth, error) {
	// Check cache
	cacheMu.RLock()
	if cached, ok := cache[devicePath]; ok && time.Since(cached.QueriedAt) < cacheTTL {
		cacheMu.RUnlock()
		return cached, nil
	}
	cacheMu.RUnlock()

	health, err := querySmartctl(devicePath)
	if err != nil {
		return &DiskHealth{
			DevicePath: devicePath,
			Supported:  false,
			Assessment: AssessmentUnknown,
			QueriedAt:  time.Now(),
		}, nil // Return unknown, not error — many devices don't support SMART
	}

	// Update cache
	cacheMu.Lock()
	cache[devicePath] = health
	cacheMu.Unlock()

	return health, nil
}

// querySmartctl executes smartctl and parses the JSON output.
func querySmartctl(devicePath string) (*DiskHealth, error) {
	// --json=c: compact JSON output
	// --all: all available info
	// --nocheck=standby: don't wake sleeping disks
	//
	// smartctl exit codes are bit flags — non-zero doesn't always mean failure:
	//   Bit 0: command parse error
	//   Bit 1: device open failed
	//   Bits 2+: SMART status flags (not errors)
	// So we use CombinedOutput and only treat bits 0-1 as real errors.
	ctx, cancel := context.WithTimeout(context.Background(), smartctlTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "smartctl", "--json=c", "--all", "--nocheck=standby", devicePath).CombinedOutput()
	if err != nil {
		// Surface the timeout as a real error so the caller knows the
		// disk is unresponsive — without this we'd fall through to
		// the JSON-parse path on a truncated CombinedOutput buffer.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("smartctl timed out after %s on %s", smartctlTimeout, devicePath)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode()&0x03 != 0 {
				return nil, fmt.Errorf("smartctl failed (exit %d): %w", exitErr.ExitCode(), err)
			}
			// Bits 2+ are SMART status flags — output is still valid JSON
		} else {
			return nil, fmt.Errorf("smartctl exec failed: %w", err)
		}
	}

	var parsed smartctlOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse smartctl JSON: %w", err)
	}

	health := &DiskHealth{
		DevicePath: devicePath,
		Supported:  true,
		QueriedAt:  time.Now(),
	}

	// Overall SMART test
	if parsed.SmartStatus != nil {
		health.Passed = parsed.SmartStatus.Passed
	}

	// Temperature
	if parsed.Temperature != nil {
		health.Temperature = parsed.Temperature.Current
	}

	// Power on hours
	if parsed.PowerOnTime != nil {
		health.PowerOnHours = parsed.PowerOnTime.Hours
	}
	health.PowerCycles = parsed.PowerCycleCount

	// Parse attributes
	if parsed.ATASmartAttributes != nil {
		for _, attr := range parsed.ATASmartAttributes.Table {
			a := Attribute{
				ID:        attr.ID,
				Name:      attr.Name,
				Value:     attr.Value,
				Worst:     attr.Worst,
				Threshold: attr.Thresh,
				RawValue:  attr.Raw.Value,
				Failing:   attr.Thresh > 0 && attr.Value <= attr.Thresh,
			}
			health.Attributes = append(health.Attributes, a)

			// Extract critical indicators
			switch attr.ID {
			case 5: // Reallocated Sector Count
				health.ReallocatedSectors = int(attr.Raw.Value)
			case 197: // Current Pending Sector Count
				health.PendingSectors = int(attr.Raw.Value)
			case 198: // Uncorrectable Sector Count
				health.UncorrectableErrs = int(attr.Raw.Value)
			case 199: // UltraDMA CRC Error Count
				health.CRCErrors = int(attr.Raw.Value)
			}
		}
	}

	// Compute assessment
	health.Assessment = computeAssessment(health)

	return health, nil
}

// computeAssessment determines the overall health status based on SMART data.
// Logic inspired by OpenMediaVault's assessment algorithm.
func computeAssessment(h *DiskHealth) Assessment {
	if !h.Supported {
		return AssessmentUnknown
	}

	// CRITICAL: active failures
	if !h.Passed {
		return AssessmentCritical
	}
	if h.ReallocatedSectors > 100 || h.PendingSectors > 10 || h.UncorrectableErrs > 10 {
		return AssessmentCritical
	}

	// WARNING: early signs of degradation
	if h.ReallocatedSectors > 0 || h.PendingSectors > 0 || h.UncorrectableErrs > 0 {
		return AssessmentWarning
	}
	if h.CRCErrors > 100 { // Cable issue
		return AssessmentWarning
	}

	// Check if any attribute is below threshold
	for _, attr := range h.Attributes {
		if attr.Failing {
			return AssessmentCritical
		}
	}

	return AssessmentGood
}

// InvalidateCache removes a device from the cache (e.g., after hot-plug).
func InvalidateCache(devicePath string) {
	cacheMu.Lock()
	delete(cache, devicePath)
	cacheMu.Unlock()
}
