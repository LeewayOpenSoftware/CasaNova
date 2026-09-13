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
// now each domain's events would live with the service that emits them:
//   - UserService: auth events
//   - CasaNova:    file operations, system config, trash, shield bans
//   - AppManagement: container lifecycle
//
// NONE OF THAT IS WIRED YET, and the list above used to say "(already wired)"
// next to UserService, which is false: UserService's SetAuditLoggerDB has zero
// callers, so every one of its audit call sites no-ops. THIS process has no
// audit writers at all -- the only AuditLogger() use under CasaNova/ is the
// read handler in route/v1/events.go. So this process's security_audit_log
// table is empty BY CONSTRUCTION, not because nothing has happened yet.
var (
	auditLogger     *auditlog.Logger
	notificationSvc *notifications.Service
	instanceMu      sync.Mutex

	// auditRecordingWired says whether anything in this process actually
	// RECORDS into the audit store. It is deliberately separate from
	// auditLogger being non-nil, because those answer different questions:
	// the logger exists so the store can be READ, and reading is wired
	// (main.go calls InitAuditAndNotifications). Recording is not.
	//
	// Nothing sets this today, and false is the honest answer. It exists as
	// the single switch a future writer activation flips, so the read surface
	// starts telling the truth the moment recording is real, instead of
	// needing to be found and updated separately.
	auditRecordingWired bool
)

// MarkAuditRecordingWired declares that this process records audit events.
//
// Call it from whatever activates the writers. It is NOT called today: the
// audit writers live in CasaNova-UserService, whose SetAuditLoggerDB has no
// caller, and they write to that service's own database rather than this one
// -- so activating them is a cross-service change (shared or queried store),
// not a line here. Until then the read surface must not imply otherwise.
func MarkAuditRecordingWired() {
	instanceMu.Lock()
	defer instanceMu.Unlock()
	auditRecordingWired = true
}

// AuditRecordingWired reports whether audit events are being recorded into
// this process's store. When false, an EMPTY query result means "nothing was
// ever written", not "nothing happened" -- a distinction the read endpoint has
// to pass on to its caller.
func AuditRecordingWired() bool {
	instanceMu.Lock()
	defer instanceMu.Unlock()
	return auditRecordingWired
}

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
