package service

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/common"
	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/pkg/smart"
	"go.uber.org/zap"
)

const smartCheckInterval = 15 * time.Minute

// publishSmartEvent emits a bus event for a degrading disk so the
// notification system, dashboard banner, and admin UI all see it.
// Best-effort: a bus hiccup must not stop subsequent checks. Bounded
// 5s — same as every other publish in this package.
func publishSmartEvent(devPath, model, level string, h *smart.DiskHealth) {
	if h == nil {
		return
	}
	if MyService == nil || MyService.MessageBus() == nil {
		return
	}
	props := map[string]string{
		"device":              devPath,
		"model":               model,
		"level":               level,
		"reallocated_sectors": strconv.Itoa(h.ReallocatedSectors),
		"pending_sectors":     strconv.Itoa(h.PendingSectors),
		"temperature":         strconv.Itoa(h.Temperature),
		"message":             fmt.Sprintf("SMART %s on %s (%s)", level, devPath, model),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := MyService.MessageBus().PublishEventWithResponse(ctx,
		common.ServiceName, common.ServiceName+":smart:"+level, props); err != nil {
		logger.Error("smart_monitor: failed to publish smart event",
			zap.Error(err), zap.String("device", devPath))
	}
}

// StartSmartMonitor launches a background goroutine that periodically
// checks SMART health on all disks and logs warnings for degrading drives.
func StartSmartMonitor(ctx context.Context) {
	go func() {
		// Wave 30: recover() so a panic inside checkAllDisks (LSBLK or
		// smart.Query parsing a malformed device output, for example)
		// does not silently take the SMART monitor offline for the
		// remaining lifetime of the daemon. Without this, a sysadmin
		// gets *no* drive-health warnings until the next reboot, and
		// nothing in the logs explains why monitoring stopped.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("SMART monitor goroutine panicked — drive health monitoring is OFFLINE",
					zap.Any("panic", r))
			}
		}()

		// Wave 35-X: initial delay must respect ctx.Done so shutdown
		// during boot doesn't block for 30s.
		select {
		case <-ctx.Done():
			logger.Info("SMART monitor stopped during initial delay")
			return
		case <-time.After(30 * time.Second):
		}
		checkAllDisks()

		ticker := time.NewTicker(smartCheckInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				logger.Info("SMART monitor stopped")
				return
			case <-ticker.C:
				checkAllDisks()
			}
		}
	}()
	logger.Info("SMART monitor started", zap.Duration("interval", smartCheckInterval))
}

func checkAllDisks() {
	disks := MyService.Disk().LSBLK(true)

	for _, disk := range disks {
		if disk.Tran == "" && disk.Type != "disk" {
			continue // Skip non-physical devices
		}

		health, err := smart.Query(disk.Path)
		if err != nil {
			continue // SMART not supported on this device
		}

		if !health.Supported {
			continue
		}

		switch health.Assessment {
		case smart.AssessmentCritical:
			logger.Error("SMART CRITICAL: disk is failing!",
				zap.String("device", disk.Path),
				zap.String("model", disk.Model),
				zap.Int("reallocated_sectors", health.ReallocatedSectors),
				zap.Int("pending_sectors", health.PendingSectors),
				zap.Int("temperature", health.Temperature),
			)
			// Wave 35-X: wire up the bus notification that was
			// TODO'd in this file. Critical SMART = disk is dying;
			// the user MUST see this in the dashboard or they lose
			// data. Without this publish, the only signal of imminent
			// failure was a daemon log line nobody reads.
			publishSmartEvent(disk.Path, disk.Model, "critical", health)
		case smart.AssessmentWarning:
			logger.Error("SMART WARNING: disk showing early signs of degradation",
				zap.String("device", disk.Path),
				zap.String("model", disk.Model),
				zap.Int("reallocated_sectors", health.ReallocatedSectors),
				zap.Int("pending_sectors", health.PendingSectors),
				zap.Int("temperature", health.Temperature),
			)
			publishSmartEvent(disk.Path, disk.Model, "warning", health)
		case smart.AssessmentGood:
			logger.Info("SMART: disk healthy",
				zap.String("device", disk.Path),
				zap.Int("temperature", health.Temperature),
				zap.Int("power_on_hours", health.PowerOnHours),
			)
		}
	}
}
