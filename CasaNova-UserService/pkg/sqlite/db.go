/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-05-13 18:15:46
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-07-11 18:10:53
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package sqlite

import (
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/file"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/model"
	model2 "github.com/LeewayOpenSoftware/CasaNova-UserService/service/model"
	"github.com/glebarez/sqlite"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

var (
	gdb     *gorm.DB
	gdbOnce sync.Once
)

func GetDb(dbPath string) *gorm.DB {
	gdbOnce.Do(func() {
		file.IsNotExistMkDir(dbPath)
		// Wave 35-Q: pass the journal_mode + busy_timeout PRAGMAs in the
		// DSN so EVERY pooled connection inherits them. The previous
		// `db.Exec("PRAGMA journal_mode=WAL")` only stuck on whichever
		// pooled connection happened to run it, leaving the rest in
		// rollback-journal mode and causing intermittent
		// `database is locked` 500s on concurrent password updates.
		dsn := dbPath + "/user.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
		if err != nil {
			panic(err)
		}

		c, _ := db.DB()
		// Wave 35-Q: SQLite is single-writer; gorm pool > 1 races for the
		// WAL lock. Cap at 1 writer to enforce the rule in-process.
		c.SetMaxIdleConns(1)
		c.SetMaxOpenConns(1)
		c.SetConnMaxIdleTime(300 * time.Second) // 5 min idle timeout

		gdb = db

		err = db.AutoMigrate(model2.UserDBModel{}, model.EventModel{})
		if err != nil {
			logger.Error("check or create db error", zap.Any("error", err))
		}

		// One-time backfill: legacy users had no Role column, so existing
		// rows hold "" which used to be normalized to admin. Now Normalize
		// fails closed to guest, so we must promote those rows explicitly
		// before any RBAC check fires. Idempotent — only touches empty.
		if res := db.Model(&model2.UserDBModel{}).Where("role = ? OR role IS NULL", "").Update("role", "admin"); res.Error != nil {
			logger.Error("legacy role backfill failed", zap.Any("error", res.Error))
		} else if res.RowsAffected > 0 {
			logger.Info("legacy role backfill applied", zap.Int64("rows", res.RowsAffected))
		}
	})
	return gdb
}
