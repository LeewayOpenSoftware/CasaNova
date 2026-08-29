package service

import (
	"sync"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/auditlog"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/notifications"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Audit + notifications singletons for the main CasaNova service.
//
// Each service process keeps its own SQLite-backed audit log. A future
// aggregator could walk all services' logs for a unified view, but for
// now each domain's events live with the service that emits them:
//   - UserService: auth events (already wired)
//   - CasaNova:    file operations, system config, trash, shield bans
//   - AppManagement: container lifecycle
var (
	auditLogger     *auditlog.Logger
	notificationSvc *notifications.Service
	instanceMu      sync.Mutex
)

// InitAuditAndNotifications wires both the audit logger and notification
// service to the given DB. Called once at startup after sqlite is ready.
func InitAuditAndNotifications(db *gorm.DB) {
	instanceMu.Lock()
	defer instanceMu.Unlock()

	if db == nil {
		return
	}
	if l, err := auditlog.NewLogger(db); err != nil {
		logger.Error("failed to initialize audit logger", zap.Error(err))
	} else {
		auditLogger = l
		logger.Info("audit logger initialized (casanova main)")
	}
	if s, err := notifications.NewService(db); err != nil {
		logger.Error("failed to initialize notification service", zap.Error(err))
	} else {
		notificationSvc = s
		logger.Info("notification service initialized (casanova main)")
	}
}

// AuditLogger returns the global audit logger for this process.
// Returns nil before Init — callers must check.
func AuditLogger() *auditlog.Logger {
	instanceMu.Lock()
	defer instanceMu.Unlock()
	return auditLogger
}

// Notifications returns the global notification service.
// Returns nil before Init — callers must check.
func Notifications() *notifications.Service {
	instanceMu.Lock()
	defer instanceMu.Unlock()
	return notificationSvc
}

// AuditConfig is a convenience emitter for config change events.
func AuditConfig(actor, target, message string) {
	if l := AuditLogger(); l != nil {
		l.Log(auditlog.Event{
			Type:     auditlog.EventConfigChanged,
			Severity: auditlog.SeverityInfo,
			Actor:    actor,
			Target:   target,
			Message:  message,
		})
	}
}

// AuditFileOp records a file-level destructive operation (delete, rename,
// trash). These are the events an admin wants to see when investigating
// "what happened to that file".
func AuditFileOp(action, actor, target, message string) {
	if l := AuditLogger(); l != nil {
		l.Log(auditlog.Event{
			Type:     auditlog.EventType("file." + action),
			Severity: auditlog.SeverityInfo,
			Actor:    actor,
			Target:   target,
			Message:  message,
		})
	}
}
