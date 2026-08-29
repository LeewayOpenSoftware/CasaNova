// Package auditlog provides structured security event logging for CasaNova.
//
// Distinct from oplog (which tracks lifecycle operations), auditlog captures
// security-relevant events: authentication attempts, permission changes,
// config modifications, rate-limit bans, etc.
//
// Events are stored in SQLite with indexes on event type and timestamp so
// admins can quickly answer "who tried to log in as admin last Tuesday?"
package auditlog

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// EventType enumerates security event categories.
// Using strings (not iota) so they're human-readable in the database.
type EventType string

const (
	// Authentication events
	EventLoginSuccess   EventType = "auth.login.success"
	EventLoginFailure   EventType = "auth.login.failure"
	EventLogout         EventType = "auth.logout"
	EventPasswordReset  EventType = "auth.password.reset"
	EventPasswordChange EventType = "auth.password.change"
	Event2FAEnabled     EventType = "auth.2fa.enabled"
	Event2FADisabled    EventType = "auth.2fa.disabled"

	// Authorization events
	EventPermissionDenied EventType = "authz.denied"
	EventPermissionGrant  EventType = "authz.grant"

	// Configuration events
	EventConfigChanged  EventType = "config.changed"
	EventUserCreated    EventType = "user.created"
	EventUserDeleted    EventType = "user.deleted"
	EventUserRoleChange EventType = "user.role.changed"

	// Network / rate limiting events
	EventShieldThrottle EventType = "shield.throttle"
	EventShieldStrike   EventType = "shield.strike"
	EventShieldTempBan  EventType = "shield.ban.temp"
	EventShieldPermBan  EventType = "shield.ban.perm"
	EventIPBlocked      EventType = "net.ip.blocked"

	// App lifecycle security events
	EventAppInstalled   EventType = "app.installed"
	EventAppUninstalled EventType = "app.uninstalled"
	EventAppUpdated     EventType = "app.updated"
)

// Severity indicates the importance of an event.
// Used to filter noise in the audit log UI.
type Severity string

const (
	SeverityInfo     Severity = "info"     // Normal activity
	SeverityWarning  Severity = "warning"  // Worth noting
	SeverityCritical Severity = "critical" // Immediate attention
)

// Event is a single audit log record.
type Event struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Type      EventType `gorm:"index;size:64" json:"type"`
	Severity  Severity  `gorm:"index;size:16" json:"severity"`
	Actor     string    `gorm:"index;size:64" json:"actor,omitempty"`    // User/system that did it
	ActorIP   string    `gorm:"index;size:45" json:"actor_ip,omitempty"` // IPv4 or IPv6
	Target    string    `gorm:"size:128" json:"target,omitempty"`        // What was acted on
	Message   string    `gorm:"size:512" json:"message"`                 // Human-readable summary
	Metadata  string    `gorm:"size:1024" json:"metadata,omitempty"`     // JSON blob of extra context
	Timestamp time.Time `gorm:"index" json:"timestamp"`
}

// TableName overrides GORM's default naming.
func (Event) TableName() string {
	return "security_audit_log"
}

// logSemaphore bounds the number of concurrent goroutines spawned by Log().
// Without this, a burst of events (e.g. during a brute-force attack) could
// exhaust memory with unbounded goroutines.
var logSemaphore = make(chan struct{}, 100)

// droppedAuditEvents counts events the semaphore had to drop. We can't
// log every drop synchronously (the logger.Warn call hits the same disk
// the gorm write is contending for, defeating the point of dropping)
// so we count them and emit a single warn line every 100 drops.
//
// Sysadmins need to know when audit data is being thrown away — silent
// drops are exactly the failure mode that lets a slow brute-force
// attack hide in the noise. Before this fix, the `default` branch
// dropped events with zero feedback.
var droppedAuditEvents atomic.Uint64

// Logger is the audit event logger.
// Safe for concurrent use.
type Logger struct {
	db *gorm.DB
	mu sync.Mutex
}

// NewLogger creates an audit logger backed by the given DB.
// Auto-migrates the schema.
func NewLogger(db *gorm.DB) (*Logger, error) {
	if err := db.AutoMigrate(&Event{}); err != nil {
		return nil, err
	}
	return &Logger{db: db}, nil
}

// Log records a security event. Never blocks: if the DB is slow,
// the caller continues and the log write happens in a goroutine.
// This is the right tradeoff for security logging — we don't want
// to slow down login requests waiting for a disk write.
// A semaphore limits concurrent goroutines to prevent memory exhaustion
// under burst load; excess events are silently dropped.
func (l *Logger) Log(event Event) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if event.Severity == "" {
		event.Severity = SeverityInfo
	}

	select {
	case logSemaphore <- struct{}{}:
		go func(e Event) {
			defer func() { <-logSemaphore }()
			l.mu.Lock()
			defer l.mu.Unlock()
			l.db.Create(&e)
		}(event)
	default:
		// Drop the log rather than exhaust memory, but make the
		// drop visible. We rate-limit the warn output to once per
		// 100 drops so the warn channel itself never becomes the
		// bottleneck during a real flood.
		dropped := droppedAuditEvents.Add(1)
		if dropped%100 == 1 {
			// logger.Warn doesn't exist in this package; Error is the
			// only severity above Info, which is appropriate — dropped
			// audit events ARE a security signal.
			logger.Error("audit log buffer full — dropping events",
				zap.Uint64("total_dropped_since_start", dropped),
				zap.String("last_event_type", string(event.Type)),
				zap.String("last_actor", event.Actor),
				zap.String("last_actor_ip", event.ActorIP))
		}
	}
}

// LogAuth is a convenience method for authentication events.
func (l *Logger) LogAuth(eventType EventType, actor, ip, message string, success bool) {
	sev := SeverityInfo
	if !success {
		sev = SeverityWarning
	}
	l.Log(Event{
		Type:     eventType,
		Severity: sev,
		Actor:    actor,
		ActorIP:  ip,
		Message:  message,
	})
}

// LogShield is a convenience for rate-limit / ban events.
func (l *Logger) LogShield(eventType EventType, ip, message string) {
	sev := SeverityWarning
	if eventType == EventShieldPermBan {
		sev = SeverityCritical
	}
	l.Log(Event{
		Type:     eventType,
		Severity: sev,
		ActorIP:  ip,
		Message:  message,
	})
}

// Query returns events matching the given filters, newest first.
// All filters are optional; pass zero values to skip them.
type QueryFilter struct {
	Types    []EventType
	Severity Severity
	Actor    string
	ActorIP  string
	Since    time.Time
	Until    time.Time
	Limit    int
}

func (l *Logger) Query(filter QueryFilter) ([]Event, error) {
	q := l.db.Model(&Event{})

	if len(filter.Types) > 0 {
		q = q.Where("type IN ?", filter.Types)
	}
	if filter.Severity != "" {
		q = q.Where("severity = ?", filter.Severity)
	}
	if filter.Actor != "" {
		q = q.Where("actor = ?", filter.Actor)
	}
	if filter.ActorIP != "" {
		q = q.Where("actor_ip = ?", filter.ActorIP)
	}
	if !filter.Since.IsZero() {
		q = q.Where("timestamp >= ?", filter.Since)
	}
	if !filter.Until.IsZero() {
		q = q.Where("timestamp <= ?", filter.Until)
	}

	limit := filter.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	var events []Event
	err := q.Order("timestamp DESC").Limit(limit).Find(&events).Error
	return events, err
}

// Prune deletes events older than maxAge.
// Call periodically (e.g. weekly) to bound log size.
// Default retention: 90 days.
func (l *Logger) Prune(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge)
	result := l.db.Where("timestamp < ?", cutoff).Delete(&Event{})
	return result.RowsAffected, result.Error
}
