package repository

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/model"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/pkg/ysk"
	"github.com/glebarez/sqlite"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// addSqlitePragmas appends the WAL + busy_timeout PRAGMAs to a sqlite
// DSN unless the path is the in-memory shared cache form used by tests.
// Wave 35-Q centralized this so the two open paths in NewDatabaseRepository
// always agree.
func addSqlitePragmas(path string) string {
	if strings.HasPrefix(path, "file::memory:") || strings.Contains(path, ":memory:") {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
}

type DatabaseRepository struct {
	db        *gorm.DB
	persistDB *gorm.DB
}

func (r *DatabaseRepository) GetYSKCardList() ([]ysk.YSKCard, error) {
	var cardList []ysk.YSKCard
	if err := r.persistDB.Order("updated desc, id desc").Find(&cardList).Error; err != nil {
		return nil, err
	}
	return cardList, nil
}

func (r *DatabaseRepository) UpsertYSKCard(card ysk.YSKCard) error {
	return r.persistDB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		UpdateAll: true,
	}).Create(&card).Error
}

func (r *DatabaseRepository) DeleteYSKCard(id string) error {
	if id == "" {
		return fmt.Errorf("id is required for deletion")
	}
	// Use exact match — LIKE with user input allows mass deletion via empty/partial IDs.
	return r.persistDB.Where("id = ?", id).Delete(&ysk.YSKCard{}).Error
}

func (r *DatabaseRepository) GetEventTypes() ([]model.EventType, error) {
	var eventTypes []model.EventType

	if err := r.db.Preload(model.PropertyTypeList).Find(&eventTypes).Error; err != nil {
		return nil, err
	}

	return eventTypes, nil
}

func (r *DatabaseRepository) GetSettings(key string) (*model.Settings, error) {
	var settings model.Settings
	if err := r.persistDB.Where(&model.Settings{Key: key}).First(&settings).Error; err != nil {
		return nil, err
	}
	return &settings, nil
}

func (r *DatabaseRepository) UpsertSettings(settings model.Settings) error {
	return r.persistDB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		UpdateAll: true,
	}).Create(&settings).Error
}

// RegisterEventType creates an event type, or returns the existing one
// if a row with the same (SourceID, Name) is already in the database.
//
// Wave 28: previously this used `clause.OnConflict{UpdateAll: true}`,
// which on a primary-key conflict rewrote every column on the existing
// row AND attempted to re-insert the many-to-many PropertyType rows.
// The result was that ANY caller who could register events under a
// SourceID could redefine that SourceID's schema (replacing or adding
// property types), and the join table accumulated duplicate rows on
// every re-registration. There is no per-source-id ownership check
// upstream of this method, so an attacker who could enumerate live
// SourceIDs (trivial: GetEventTypes is unauthenticated on loopback)
// could arbitrarily mutate any other producer's schema.
//
// New behaviour:
//   - If no row exists, INSERT and return.
//   - If a row exists and the inbound EventType is structurally
//     identical, return the existing record (idempotent registration —
//     producers can safely re-register their own schema on restart).
//   - If a row exists and differs, refuse with a conflict error so the
//     mutation is loud, not silent. Updating an existing schema must
//     go through an explicit, audited path (currently absent — kept
//     out of scope for this wave).
func (r *DatabaseRepository) RegisterEventType(eventType model.EventType) (*model.EventType, error) {
	var existing model.EventType
	err := r.db.Preload(model.PropertyTypeList).
		Where(&model.EventType{SourceID: eventType.SourceID, Name: eventType.Name}).
		First(&existing).Error
	if err == nil {
		if eventTypePropertyListEqual(existing.PropertyTypeList, eventType.PropertyTypeList) {
			return &existing, nil
		}
		return nil, fmt.Errorf(
			"event type %q from sourceID %q already exists with a different schema; refusing to overwrite",
			eventType.Name, eventType.SourceID,
		)
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}
	if err := r.db.Create(&eventType).Error; err != nil {
		return nil, err
	}
	return &eventType, nil
}

// eventTypePropertyListEqual returns true if two PropertyType lists
// describe the same set of properties (compared by Name; the type
// system stores name + type, but order is irrelevant for equality).
// Used to make RegisterEventType idempotent for re-registration of
// an unchanged schema.
func eventTypePropertyListEqual(a, b []model.PropertyType) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, p := range a {
		seen[p.Name] = struct{}{}
	}
	for _, p := range b {
		if _, ok := seen[p.Name]; !ok {
			return false
		}
	}
	return true
}

func (r *DatabaseRepository) GetEventTypesBySourceID(sourceID string) ([]model.EventType, error) {
	var eventTypes []model.EventType

	if err := r.db.Preload(model.PropertyTypeList).Where(&model.EventType{SourceID: sourceID}).Find(&eventTypes).Error; err != nil {
		return nil, err
	}

	return eventTypes, nil
}

func (r *DatabaseRepository) GetEventType(sourceID string, name string) (*model.EventType, error) {
	var eventType model.EventType

	if err := r.db.Preload(model.PropertyTypeList).Where(&model.EventType{SourceID: sourceID, Name: name}).First(&eventType).Error; err != nil {
		logger.Error("can't find event type", zap.String("sourceID", sourceID), zap.String("EventName", name), zap.Error(err))
		return nil, err
	}

	return &eventType, nil
}

func (r *DatabaseRepository) GetActionTypes() ([]model.ActionType, error) {
	return GetTypes[model.ActionType](r.db)
}

// RegisterActionType mirrors Wave 28's RegisterEventType hardening:
// re-registering an existing (sourceID, name) pair with a *different*
// schema must be refused, not silently overwritten. The previous
// implementation delegated to RegisterType which uses
// `clause.OnConflict{UpdateAll: true}` — i.e. UPSERT-and-replace —
// so any caller could enumerate live SourceIDs (GetActionTypes is
// reachable on the loopback bus) and stomp another producer's
// schema. The fact that the same author rationale lives 60 lines
// above on RegisterEventType is what makes this an oversight rather
// than an intentional design choice.
//
// Behaviour now matches RegisterEventType:
//   - No row exists → INSERT and return.
//   - Row exists with structurally identical PropertyTypeList → return
//     the existing record (idempotent re-registration on producer
//     restart is still allowed).
//   - Row exists with a different schema → refuse with a conflict
//     error so the mutation is loud, not silent.
func (r *DatabaseRepository) RegisterActionType(actionType model.ActionType) (*model.ActionType, error) {
	var existing model.ActionType
	err := r.db.Preload(model.PropertyTypeList).
		Where(&model.ActionType{SourceID: actionType.SourceID, Name: actionType.Name}).
		First(&existing).Error
	if err == nil {
		if eventTypePropertyListEqual(existing.PropertyTypeList, actionType.PropertyTypeList) {
			return &existing, nil
		}
		return nil, fmt.Errorf(
			"action type %q from sourceID %q already exists with a different schema; refusing to overwrite",
			actionType.Name, actionType.SourceID,
		)
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}
	if err := r.db.Create(&actionType).Error; err != nil {
		return nil, err
	}
	return &actionType, nil
}

func (r *DatabaseRepository) GetActionTypesBySourceID(sourceID string) ([]model.ActionType, error) {
	return GetTypesBySourceID[model.ActionType](r.db, sourceID)
}

func (r *DatabaseRepository) GetActionType(sourceID string, name string) (*model.ActionType, error) {
	return GetType[model.ActionType](r.db, sourceID, name)
}

func (r *DatabaseRepository) Close() {
	for _, db := range []*gorm.DB{r.db, r.persistDB} {
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	}
}

func GetTypes[T any](db *gorm.DB) ([]T, error) {
	var types []T

	if err := db.Preload(model.PropertyTypeList).Find(&types).Error; err != nil {
		return nil, err
	}

	return types, nil
}

func RegisterType[T any](db *gorm.DB, t T) (*T, error) {
	// upsert
	if err := db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&t).Error; err != nil {
		return nil, err
	}

	return &t, nil
}

func GetTypesBySourceID[T any](db *gorm.DB, sourceID string) ([]T, error) {
	var types []T

	if err := db.Preload(model.PropertyTypeList).Where(&model.GenericType{SourceID: sourceID}).Find(&types).Error; err != nil {
		return nil, err
	}

	return types, nil
}

func GetType[T any](db *gorm.DB, sourceID string, name string) (*T, error) {
	var t T

	if err := db.Preload(model.PropertyTypeList).Where(&model.GenericType{SourceID: sourceID, Name: name}).First(&t).Error; err != nil {
		return nil, err
	}

	return &t, nil
}

func NewDatabaseRepositoryInMemory() (Repository, error) {
	return NewDatabaseRepository("file::memory:?cache=shared", "file::memory:?cache=shared")
}

func NewDatabaseRepository(databaseFilePath string, persistDatabaseFilePath string) (Repository, error) {
	// Create database directory with restricted permissions (owner + group only)
	if err := os.MkdirAll(filepath.Dir(databaseFilePath), 0o750); err != nil {
		return nil, err
	}
	// Wave 35-Q: glebarez/sqlite defaults to rollback-journal +
	// synchronous=FULL, which serializes every writer at the file-lock
	// level. The persistDB at /var/lib/casaos/db/message-bus.db is hit
	// by UpsertYSKCard on every notification — under heavy load
	// (backup running, downloads finishing) the writes serialize and
	// concurrent GetYSKCardList readers see SQLITE_BUSY, blanking the
	// dashboard. Force WAL + a 5 s busy timeout via the DSN so every
	// new connection inherits the pragmas. In-memory tests use
	// "file::memory:?cache=shared" — only append the WAL flags when
	// the path is a real file path so we do not break the test fixture.
	db, err := gorm.Open(sqlite.Open(addSqlitePragmas(databaseFilePath)))
	if err != nil {
		return nil, err
	}
	persistDB, err := gorm.Open(sqlite.Open(addSqlitePragmas(persistDatabaseFilePath)))
	if err != nil {
		return nil, err
	}

	for _, db := range []*gorm.DB{db, persistDB} {
		if c, err := db.DB(); err == nil {
			c.SetMaxIdleConns(1)
			c.SetMaxOpenConns(1)
			c.SetConnMaxIdleTime(300 * time.Second) // 5 min idle timeout
		}
	}

	if err := db.AutoMigrate(
		&model.EventType{}, &model.ActionType{}, &model.PropertyType{},
	); err != nil {
		return nil, err
	}

	if err := persistDB.AutoMigrate(
		&ysk.YSKCard{},
		&model.Settings{},
	); err != nil {
		return nil, err
	}

	return &DatabaseRepository{
		db:        db,
		persistDB: persistDB,
	}, nil
}
