// Package notifications provides user-facing notifications backed by SQLite.
//
// Notifications are distinct from oplog (lifecycle operations) and auditlog
// (security events). This is what users see in the notification center:
// "Your Plex container crashed", "Backup completed", "Update available".
//
// Notifications are tied to the MessageBus event system — the backend
// publishes events, and a subscriber turns them into notifications.
package notifications

import (
	"sync"
	"time"

	"gorm.io/gorm"
)

// Level indicates the urgency of a notification.
type Level string

const (
	LevelInfo    Level = "info"    // General info
	LevelSuccess Level = "success" // Positive outcome (backup done, app installed)
	LevelWarning Level = "warning" // Attention needed (update available, disk 85% full)
	LevelError   Level = "error"   // Immediate action (container crashed, disk SMART critical)
)

// Category groups notifications for UI filtering.
type Category string

const (
	CategoryApp      Category = "app"      // App lifecycle events
	CategoryStorage  Category = "storage"  // Disk events
	CategorySystem   Category = "system"   // System-level events
	CategoryUpdate   Category = "update"   // Update availability
	CategorySecurity Category = "security" // Security alerts
	CategoryBackup   Category = "backup"   // Backup events
)

// Notification is a single notification record.
type Notification struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Level     Level     `gorm:"index;size:16" json:"level"`
	Category  Category  `gorm:"index;size:32" json:"category"`
	Title     string    `gorm:"size:128" json:"title"`
	Message   string    `gorm:"size:512" json:"message"`
	Link      string    `gorm:"size:256" json:"link,omitempty"`      // Optional deep link
	LinkLabel string    `gorm:"size:64" json:"link_label,omitempty"` // e.g. "View app"
	Read      bool      `gorm:"index" json:"read"`
	CreatedAt time.Time `gorm:"index" json:"created_at"`
}

// TableName overrides GORM's default.
func (Notification) TableName() string {
	return "notifications"
}

// Service is the notification backend.
// Safe for concurrent use.
type Service struct {
	db *gorm.DB
	mu sync.Mutex
}

// NewService creates a notification service. Auto-migrates the schema.
func NewService(db *gorm.DB) (*Service, error) {
	if err := db.AutoMigrate(&Notification{}); err != nil {
		return nil, err
	}
	return &Service{db: db}, nil
}

// Create stores a new notification.
// Use the convenience helpers (Info, Success, Warn, Err) for common cases.
func (s *Service) Create(n Notification) error {
	if n.CreatedAt.IsZero() {
		n.CreatedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Create(&n).Error
}

// Info creates an info-level notification.
func (s *Service) Info(category Category, title, message string) error {
	return s.Create(Notification{
		Level:    LevelInfo,
		Category: category,
		Title:    title,
		Message:  message,
	})
}

// Success creates a success notification.
func (s *Service) Success(category Category, title, message string) error {
	return s.Create(Notification{
		Level:    LevelSuccess,
		Category: category,
		Title:    title,
		Message:  message,
	})
}

// Warn creates a warning notification.
func (s *Service) Warn(category Category, title, message string) error {
	return s.Create(Notification{
		Level:    LevelWarning,
		Category: category,
		Title:    title,
		Message:  message,
	})
}

// Err creates an error notification (most urgent).
func (s *Service) Err(category Category, title, message string) error {
	return s.Create(Notification{
		Level:    LevelError,
		Category: category,
		Title:    title,
		Message:  message,
	})
}

// List returns notifications in reverse chronological order.
type ListOptions struct {
	UnreadOnly bool
	Category   Category // empty = all
	Level      Level    // empty = all
	Limit      int      // 0 = default 100
	Offset     int
}

func (s *Service) List(opts ListOptions) ([]Notification, error) {
	q := s.db.Model(&Notification{})

	if opts.UnreadOnly {
		q = q.Where("read = ?", false)
	}
	if opts.Category != "" {
		q = q.Where("category = ?", opts.Category)
	}
	if opts.Level != "" {
		q = q.Where("level = ?", opts.Level)
	}

	limit := opts.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	var list []Notification
	err := q.Order("created_at DESC").Limit(limit).Offset(opts.Offset).Find(&list).Error
	return list, err
}

// UnreadCount returns the count of unread notifications.
// Useful for showing a badge on the notification bell icon.
func (s *Service) UnreadCount() (int64, error) {
	var count int64
	err := s.db.Model(&Notification{}).Where("read = ?", false).Count(&count).Error
	return count, err
}

// MarkRead marks specific notifications as read.
func (s *Service) MarkRead(ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Model(&Notification{}).Where("id IN ?", ids).Update("read", true).Error
}

// MarkAllRead marks every notification as read.
func (s *Service) MarkAllRead() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Model(&Notification{}).Where("read = ?", false).Update("read", true).Error
}

// Delete removes a notification permanently.
func (s *Service) Delete(id uint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Delete(&Notification{}, id).Error
}

// Prune deletes read notifications older than maxAge.
// Unread notifications are kept regardless of age.
// Call periodically to bound database growth.
func (s *Service) Prune(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge)
	s.mu.Lock()
	defer s.mu.Unlock()

	result := s.db.Where("read = ? AND created_at < ?", true, cutoff).Delete(&Notification{})
	return result.RowsAffected, result.Error
}
