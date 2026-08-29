package service

import (
	"sync"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/auditlog"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// auditLogger is the global audit event logger for UserService.
// Safely no-ops if not initialized (so code that uses it doesn't need nil checks
// beyond the initial sanity check via AuditLogger()).
var (
	auditLogger   *auditlog.Logger
	auditLoggerMu sync.Mutex
)

// SetAuditLoggerDB initializes the audit logger.
// Call this during service startup after the DB is ready.
func SetAuditLoggerDB(db *gorm.DB) {
	auditLoggerMu.Lock()
	defer auditLoggerMu.Unlock()

	if db == nil {
		return
	}
	l, err := auditlog.NewLogger(db)
	if err != nil {
		logger.Error("failed to initialize audit logger", zap.Error(err))
		return
	}
	auditLogger = l
	logger.Info("audit logger initialized")
}

// AuditLogger returns the global audit logger, or nil if not initialized.
func AuditLogger() *auditlog.Logger {
	auditLoggerMu.Lock()
	defer auditLoggerMu.Unlock()
	return auditLogger
}

// AuditAuth is a convenience wrapper for logging authentication events.
// Safely no-ops if the audit logger isn't initialized yet.
func AuditAuth(eventType auditlog.EventType, actor, ip, message string, success bool) {
	l := AuditLogger()
	if l == nil {
		return
	}
	l.LogAuth(eventType, actor, ip, message, success)
}
