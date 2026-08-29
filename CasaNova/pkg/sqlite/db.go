/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-05-13 18:15:46
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-08-31 13:39:24
 * @FilePath: /CasaOS/pkg/sqlite/db.go
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package sqlite

import (
	"fmt"
	"sync"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/utils/file"
	model2 "github.com/LeewayOpenSoftware/CasaNova/service/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

var (
	gdb     *gorm.DB
	gdbOnce sync.Once
)

// GetDb returns the shared CasaNova SQLite handle. Wave 35-Q residual:
// previously used a naked nil-check + assignment, which raced when two
// goroutines hit GetDb during startup (one from main, one from a service
// init): both saw `gdb == nil`, both opened a fresh handle, and the
// second open clobbered the first while the first was already serving
// queries on `c.SetMaxOpenConns(1)`. The migration DROPs at the bottom
// also re-ran every time the function was re-entered before `gdb` was
// stored, occasionally racing the AutoMigrate output. Use sync.Once and
// hide the legacy DROPs behind a one-time guard so they only fire on the
// very first open in the daemon's lifetime.
func GetDb(dbPath string) *gorm.DB {
	gdbOnce.Do(func() {
		file.IsNotExistMkDir(dbPath)
		// Match the WAL/busy_timeout pragmas every other CasaNova service
		// uses; the previous code set journal_mode WAL after the handle was
		// already returned, so the very first concurrent reader could hit
		// SQLITE_BUSY before the pragma took effect.
		dsn := dbPath + "/casaOS.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
		if err != nil {
			panic("sqlite connect error")
		}

		c, _ := db.DB()
		c.SetMaxIdleConns(1)
		c.SetMaxOpenConns(1)
		c.SetConnMaxIdleTime(300 * time.Second) // 5 min idle timeout

		if err := db.AutoMigrate(&model2.AppNotify{}, model2.SharesDBModel{}, model2.ConnectionsDBModel{}, model2.PeerDriveDBModel{}); err != nil {
			fmt.Println(err)
		}

		// Legacy schema cleanup. These tables were dropped in 0.4.x and
		// only need to run once per upgrade — keep them inside the Once.
		db.Exec("DROP TABLE IF EXISTS o_application")
		db.Exec("DROP TABLE IF EXISTS o_friend")
		db.Exec("DROP TABLE IF EXISTS o_person_download")
		db.Exec("DROP TABLE IF EXISTS o_person_down_record")

		gdb = db
	})
	return gdb
}
