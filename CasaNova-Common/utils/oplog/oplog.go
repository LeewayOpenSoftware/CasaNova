// Package oplog provides structured operation logging for CasaNova.
//
// Inspired by YunoHost's operation log: every significant action creates
// a record with start time, end time, success/failure, and message.
// When something goes wrong, users can see exactly what happened and when.
//
// Records are stored in SQLite via GORM. The table is auto-created on
// first use. Old records are pruned by age (default: 90 days).
package oplog

import (
	"sync"
	"time"

	"gorm.io/gorm"
)

// Status represents the current state of an operation.
type Status string

const (
	StatusRunning   Status = "running"
	StatusSuccess   Status = "success"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Entry is a single operation log record.
type Entry struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	Action     string    `gorm:"index" json:"action"` // e.g. "app.install", "disk.mount"
	Target     string    `gorm:"index" json:"target"` // e.g. "nextcloud", "/dev/sda1"
	Actor      string    `json:"actor,omitempty"`     // User who initiated (if known)
	Status     Status    `gorm:"index" json:"status"`
	Message    string    `json:"message,omitempty"` // Human-readable summary
	ErrorText  string    `json:"error,omitempty"`   // Error details if failed
	StartedAt  time.Time `gorm:"index" json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitempty"`
	DurationMs int64     `json:"duration_ms"` // Convenience field
}

// TableName overrides GORM's default table naming convention.
func (Entry) TableName() string {
	return "operation_log"
}

// Logger is the operation logger.
// Thread-safe: can be used from multiple goroutines.
type Logger struct {
	db *gorm.DB
	mu sync.Mutex
}

// NewLogger creates a new operation logger backed by the given database.
// Auto-migrates the schema on first use.
func NewLogger(db *gorm.DB) (*Logger, error) {
	if err := db.AutoMigrate(&Entry{}); err != nil {
		return nil, err
	}
	return &Logger{db: db}, nil
}

// Operation is a handle to an in-progress operation.
// Call Complete() or Fail() when the operation finishes.
type Operation struct {
	entry  *Entry
	logger *Logger
}

// Start begins a new operation and returns a handle.
// The operation is immediately persisted with Status=running.
func (l *Logger) Start(action, target string) *Operation {
	entry := &Entry{
		Action:    action,
		Target:    target,
		Status:    StatusRunning,
		StartedAt: time.Now(),
	}

	l.mu.Lock()
	l.db.Create(entry)
	l.mu.Unlock()

	return &Operation{entry: entry, logger: l}
}

// StartWithActor begins a new operation with a known actor (user).
func (l *Logger) StartWithActor(action, target, actor string) *Operation {
	op := l.Start(action, target)
	op.entry.Actor = actor
	return op
}

// Complete marks the operation as successful.
func (op *Operation) Complete(message string) {
	op.finish(StatusSuccess, message, "")
}

// Fail marks the operation as failed with an error.
func (op *Operation) Fail(err error) {
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	op.finish(StatusFailed, "", errText)
}

// Cancel marks the operation as cancelled (user aborted or timed out).
func (op *Operation) Cancel(reason string) {
	op.finish(StatusCancelled, reason, "")
}

func (op *Operation) finish(status Status, message, errText string) {
	op.entry.Status = status
	op.entry.Message = message
	op.entry.ErrorText = errText
	op.entry.EndedAt = time.Now()
	op.entry.DurationMs = op.entry.EndedAt.Sub(op.entry.StartedAt).Milliseconds()

	op.logger.mu.Lock()
	op.logger.db.Save(op.entry)
	op.logger.mu.Unlock()
}

// ID returns the database ID of the operation.
func (op *Operation) ID() uint {
	return op.entry.ID
}

// ListRecent returns the most recent N operation log entries.
func (l *Logger) ListRecent(limit int) ([]Entry, error) {
	var entries []Entry
	err := l.db.Order("started_at DESC").Limit(limit).Find(&entries).Error
	return entries, err
}

// ListByAction returns recent operations matching a specific action.
func (l *Logger) ListByAction(action string, limit int) ([]Entry, error) {
	var entries []Entry
	err := l.db.Where("action = ?", action).Order("started_at DESC").Limit(limit).Find(&entries).Error
	return entries, err
}

// ListFailed returns recent failed operations.
func (l *Logger) ListFailed(limit int) ([]Entry, error) {
	var entries []Entry
	err := l.db.Where("status = ?", StatusFailed).Order("started_at DESC").Limit(limit).Find(&entries).Error
	return entries, err
}

// Prune deletes operation log entries older than maxAge.
// Should be called periodically (e.g. daily) to prevent unbounded growth.
func (l *Logger) Prune(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge)
	result := l.db.Where("started_at < ?", cutoff).Delete(&Entry{})
	return result.RowsAffected, result.Error
}
