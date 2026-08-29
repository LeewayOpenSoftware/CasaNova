package service

import (
	"sync"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/oplog"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// opLogger is the global operation logger for AppManagement.
// It's initialized on first use, and safely no-ops if the DB isn't available.
var (
	opLogger   *oplog.Logger
	opLoggerMu sync.Mutex
)

// SetOpLoggerDB initializes the operation logger with a database connection.
// Call this during service startup after the DB is ready.
func SetOpLoggerDB(db *gorm.DB) {
	opLoggerMu.Lock()
	defer opLoggerMu.Unlock()

	if db == nil {
		return
	}

	l, err := oplog.NewLogger(db)
	if err != nil {
		logger.Error("failed to initialize operation logger", zap.Error(err))
		return
	}
	opLogger = l
	logger.Info("operation logger initialized")
}

// OpLogger returns the global operation logger, or nil if not initialized.
// Callers should check for nil before using.
func OpLogger() *oplog.Logger {
	opLoggerMu.Lock()
	defer opLoggerMu.Unlock()
	return opLogger
}

// StartOp is a convenience wrapper that safely starts an operation.
// If the logger isn't initialized, returns nil (caller should check).
// Usage:
//
//	op := service.StartOp("app.install", "nextcloud")
//	defer func() {
//	    if op != nil {
//	        if err != nil { op.Fail(err) } else { op.Complete("installed") }
//	    }
//	}()
func StartOp(action, target string) *oplog.Operation {
	l := OpLogger()
	if l == nil {
		return nil
	}
	return l.Start(action, target)
}
