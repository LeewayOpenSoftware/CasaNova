package route

import (
	"testing"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/oplog"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestOperationLogPruneRemovesOldAndKeepsNewAndUnknown(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	logger, err := oplog.NewLogger(db)
	if err != nil {
		t.Fatal(err)
	}
	rows := []*oplog.Entry{
		{Action: "old", Status: oplog.StatusSuccess, StartedAt: time.Now().Add(-91 * 24 * time.Hour)},
		{Action: "new", Status: oplog.StatusSuccess, StartedAt: time.Now()},
		{Action: "unknown", Status: oplog.StatusSuccess},
	}
	for _, row := range rows {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	removed, err := logger.Prune(oplog.DefaultRetention)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("expected one old row removed, got %d", removed)
	}
	var remaining []oplog.Entry
	if err := db.Order("action").Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 || remaining[0].Action != "new" || remaining[1].Action != "unknown" {
		t.Fatalf("expected new and unknown rows to survive, got %#v", remaining)
	}
}

func TestOperationLogRetentionUsesSafeDefaultForInvalidConfiguration(t *testing.T) {
	t.Setenv("CASANOVA_OPLOG_RETENTION_DAYS", "not-a-date")
	if got := oplog.Retention(); got != oplog.DefaultRetention {
		t.Fatalf("invalid retention should use default, got %s", got)
	}
	t.Setenv("CASANOVA_OPLOG_RETENTION_DAYS", "30")
	if got := oplog.Retention(); got != 30*24*time.Hour {
		t.Fatalf("configured retention mismatch: %s", got)
	}
}
