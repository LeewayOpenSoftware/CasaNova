package sqlite

import (
	"context"
	"path/filepath"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/LeewayOpenSoftware/CasaNova-LocalStorage/service/model"
)

type ContextKey string

const (
	contextKeyGlobalDB = ContextKey("gdb")

	// GORM's lifecyle
	HookBeforeCreate = "before_create"
	HookAfterCreate  = "after_create"
	HookBeforeSave   = "before_save"
	HookAfterSave    = "after_save"
	HookBeforeUpdate = "before_update"
	HookAfterUpdate  = "after_update"
	HookBeforeDelete = "before_delete"
	HookAfterDelete  = "after_delete"
	HookAfterFind    = "after_find"
)

var (
	_gdb *gorm.DB

	// A map of hook to callbacks per GORM's lifecycle.
	// This allows additional logic to happen when a row is created, updated, deleted, or queried from the database, without changing original model structure or logic.
	// Otherwise we will have to add migration logic from version to version.
	Hooks map[string][]func(*gorm.DB, interface{}) error
)

func init() {
	Hooks = make(map[string][]func(*gorm.DB, interface{}) error)

	for _, v := range []string{
		HookBeforeCreate, HookAfterCreate,
		HookBeforeSave, HookAfterSave,
		HookBeforeUpdate, HookAfterUpdate,
		HookBeforeDelete, HookAfterDelete,
		HookAfterFind,
	} {
		Hooks[v] = make([]func(*gorm.DB, interface{}) error, 0)
	}
}

// Initialize each hook to call each registered callback function when the hook is triggered
func initializeHooks(db *gorm.DB) error {
	if err := db.Callback().Create().Before("gorm:create").Register(HookBeforeCreate, hookFunc(HookBeforeCreate)); err != nil {
		return err
	}

	if err := db.Callback().Create().After("gorm:create").Register(HookAfterCreate, hookFunc(HookAfterCreate)); err != nil {
		return err
	}

	if err := db.Callback().Update().Before("gorm:save").Register(HookBeforeSave, hookFunc(HookBeforeSave)); err != nil {
		return err
	}

	if err := db.Callback().Update().After("gorm:save").Register(HookAfterSave, hookFunc(HookAfterSave)); err != nil {
		return err
	}

	if err := db.Callback().Update().Before("gorm:update").Register(HookBeforeUpdate, hookFunc(HookBeforeUpdate)); err != nil {
		return err
	}

	if err := db.Callback().Update().After("gorm:update").Register(HookAfterUpdate, hookFunc(HookAfterUpdate)); err != nil {
		return err
	}

	if err := db.Callback().Delete().Before("gorm:delete").Register(HookBeforeDelete, hookFunc(HookBeforeDelete)); err != nil {
		return err
	}

	if err := db.Callback().Delete().After("gorm:delete").Register(HookAfterDelete, hookFunc(HookAfterDelete)); err != nil {
		return err
	}

	if err := db.Callback().Query().After("gorm:find").Register(HookAfterFind, hookFunc(HookAfterFind)); err != nil {
		return err
	}

	return nil
}

// Make sure each hook calls each registered callback function per hook name
func hookFunc(name string) func(d *gorm.DB) {
	return func(d *gorm.DB) {
		if d == nil || d.Statement == nil || d.Statement.Schema == nil || d.Statement.SkipHooks {
			return
		}

		gdb := d.Statement.Context.Value(contextKeyGlobalDB)
		if gdb, ok := gdb.(*gorm.DB); ok {
			for _, f := range Hooks[name] {
				if err := f(gdb, d.Statement.Model); err != nil {
					_ = d.AddError(err)
					return
				}
			}
		}
	}
}

func GetDBByFile(dbFile string) *gorm.DB {
	// Wave 35-Q: pass the journal mode + busy timeout in the DSN itself
	// so EVERY connection in the pool inherits them. The previous code
	// did `db.Exec("PRAGMA journal_mode=WAL")` once, but with
	// MaxOpenConns=5 the PRAGMA only sticks on whichever pooled
	// connection happened to run it — every other connection in the
	// pool starts in the default rollback-journal mode, where writers
	// serialize at the file-lock level and SQLITE_BUSY errors cascade
	// under load. The user-visible symptom was intermittent
	// "failed to save volume" 500s on a busy NAS. _busy_timeout=5000
	// makes contended writers retry for 5 s instead of failing
	// immediately.
	dsn := dbFile + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		panic(err)
	}

	c, _ := db.DB()
	// Wave 35-Q: SQLite is single-writer. With MaxOpenConns > 1 gorm
	// hands out arbitrary pooled connections and concurrent writes
	// race for the WAL lock. Cap to 1 writer + a small idle pool.
	c.SetMaxIdleConns(1)
	c.SetMaxOpenConns(1)
	c.SetConnMaxIdleTime(300 * time.Second) // 5 min idle timeout

	if err := db.AutoMigrate(&model.Merge{}, &model.Volume{}); err != nil {
		panic(err)
	}

	if err := initializeHooks(db); err != nil {
		panic(err)
	}

	ctx := context.WithValue(context.Background(), contextKeyGlobalDB, db)

	return db.WithContext(ctx)
}

func GetGlobalDB(dbPath string) *gorm.DB {
	if _gdb != nil {
		return _gdb
	}

	if err := file.IsNotExistMkDir(dbPath); err != nil {
		panic(err)
	}

	_gdb = GetDBByFile(filepath.Join(dbPath, "local-storage.db"))

	return _gdb
}
