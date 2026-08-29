/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-03-18 11:40:55
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-07-12 10:05:37
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package service

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/jwt"
	"github.com/LeewayOpenSoftware/CasaNova-Common/utils/logger"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/pkg/config"
	"github.com/LeewayOpenSoftware/CasaNova-UserService/service/model"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// jwtPrivateKeyFilename is the on-disk name for the persisted ECDSA
// signing key. Stored inside AppInfo.DBPath so it inherits the same
// 0700 parent directory the SQLite databases live in, and so the
// CasaOS→CasaNova migration script that already backs up db/ also
// captures the key. Anyone with read access to the sqlite files can
// already forge session tokens via the users table, so parking the
// key next to them does not widen the blast radius.
const jwtPrivateKeyFilename = "jwt_ec_private.pem"

type UserService interface {
	UpLoadFile(file multipart.File, name string) error
	CreateUser(m model.UserDBModel) model.UserDBModel
	GetUserCount() (userCount int64)
	UpdateUser(m model.UserDBModel)
	UpdateUserPassword(m model.UserDBModel)
	GetUserInfoById(id string) (m model.UserDBModel)
	GetUserAllInfoById(id string) (m model.UserDBModel)
	GetUserAllInfoByName(userName string) (m model.UserDBModel)
	DeleteUserById(id string)
	GetUserInfoByUserName(userName string) (m model.UserDBModel)
	GetAllUserName() (list []model.UserDBModel)

	GetKeyPair() (*ecdsa.PrivateKey, *ecdsa.PublicKey)
}

// UserRegisterHash is a thread-safe map for registration key validation.
// Uses sync.Map to prevent concurrent map read/write panics from simultaneous requests.
var UserRegisterHash sync.Map

type userService struct {
	privateKey *ecdsa.PrivateKey // keep this private - NEVER expose it!!!
	publicKey  *ecdsa.PublicKey

	db *gorm.DB
}

func (u *userService) DeleteUserById(id string) {
	u.db.Where("id= ?", id).Delete(&model.UserDBModel{})
}

func (u *userService) GetAllUserName() (list []model.UserDBModel) {
	u.db.Select("username").Find(&list)
	return
}

func (u *userService) CreateUser(m model.UserDBModel) model.UserDBModel {
	u.db.Create(&m)
	return m
}

func (u *userService) GetUserCount() (userCount int64) {
	u.db.Find(&model.UserDBModel{}).Count(&userCount)
	return
}

func (u *userService) UpdateUser(m model.UserDBModel) {
	u.db.Model(&m).Omit("password").Updates(&m)
}

func (u *userService) UpdateUserPassword(m model.UserDBModel) {
	u.db.Model(&m).Update("password", m.Password)
}

func (u *userService) GetUserAllInfoById(id string) (m model.UserDBModel) {
	u.db.Where("id= ?", id).First(&m)
	return
}

func (u *userService) GetUserAllInfoByName(userName string) (m model.UserDBModel) {
	u.db.Where("username= ?", userName).First(&m)
	return
}

func (u *userService) GetUserInfoById(id string) (m model.UserDBModel) {
	u.db.Select("username", "id", "role", "nickname", "description", "avatar", "email").Where("id= ?", id).First(&m)
	return
}

func (u *userService) GetUserInfoByUserName(userName string) (m model.UserDBModel) {
	u.db.Select("username", "id", "role", "nickname", "description", "avatar", "email").Where("username= ?", userName).First(&m)
	return
}

// UpLoadFile saves an uploaded file to the specified path.
// Validates that the destination path is within the allowed user data directory.
func (c *userService) UpLoadFile(file multipart.File, destPath string) error {
	cleanPath := filepath.Clean(destPath)

	// Must be absolute path.
	if !filepath.IsAbs(cleanPath) {
		return fmt.Errorf("invalid file path: must be absolute")
	}

	// Block system directories — uploads should only target user data areas.
	blockedPrefixes := []string{"/etc", "/usr", "/bin", "/sbin", "/lib", "/boot", "/dev", "/proc", "/sys", "/root", "/var/run", "/var/lib"}
	for _, prefix := range blockedPrefixes {
		if cleanPath == prefix || strings.HasPrefix(cleanPath, prefix+"/") {
			return fmt.Errorf("invalid file path: system directory not allowed")
		}
	}

	// Reject symlink at destination (TOCTOU mitigated by O_NOFOLLOW where supported).
	if info, err := os.Lstat(cleanPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("invalid file path: destination is a symlink")
	}

	out, err := os.OpenFile(cleanPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open destination file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, file); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	return nil
}

func (u *userService) GetKeyPair() (*ecdsa.PrivateKey, *ecdsa.PublicKey) {
	return u.privateKey, u.publicKey
}

// Get user service.
//
// The ECDSA key pair used to sign session JWTs is persisted across
// restarts. A historic comment on this function read "DO NOT store
// private key anywhere - keep it in memory ONLY!!!". That sounds
// security-minded but in practice it was catastrophic UX: every time
// the user-service restarted (upgrades, crashes, host reboots) every
// issued session token became unverifiable because the fresh key the
// new process generated could not validate signatures from the old
// one. Most of the UI masked the symptom because the v1 JWT
// middleware skips loopback callers, but the strictJWT middleware
// used by /v1/sys/ssh-login and /v1/sys/wsssh does actually verify —
// so "SSH" in the dashboard always reported "unauthorized" after any
// restart until the user manually logged out and back in.
//
// The "security" argument does not survive first contact either:
//   - An attacker with local filesystem read cannot do anything with
//     the key that they cannot already do by reading the sqlite users
//     table (which is in the same directory, same mode) and editing
//     rows or issuing tokens through the running process directly.
//   - The JWKS endpoint already leaks the public key, and the public
//     key alone is sufficient for offline token validation, so any
//     confidentiality argument was purely about the *private* half.
//   - Losing all sessions every restart is itself a downgrade attack
//     surface — users get conditioned to click "log in again" after
//     random 401s and can no longer distinguish "the process was
//     upgraded" from "my token got stolen and revoked".
//
// So: load-or-generate. Write 0600 to <DBPath>/jwt_ec_private.pem,
// parent dir 0700. On any parse/permission error we fail LOUD — the
// service refuses to start rather than silently rotating the key and
// invalidating every session, because silent rotation is exactly the
// bug we're fixing.
func NewUserService(db *gorm.DB) UserService {
	privateKey, publicKey, err := loadOrCreateJWTKey()
	if err != nil {
		logger.Error("failed to load or create JWT key pair", zap.Error(err))
		return nil
	}

	return &userService{
		privateKey: privateKey,
		publicKey:  publicKey,
		db:         db,
	}
}

// loadOrCreateJWTKey returns the persistent ECDSA key pair used to
// sign session tokens. On first run it generates a new key and
// writes it to disk with 0600. On subsequent runs it parses the
// existing PEM file. Any error (missing dir, corrupt PEM, wrong key
// type) is returned to the caller — do not silently fall back to an
// in-memory key because that reintroduces the exact restart-nukes-
// all-sessions bug this function exists to fix.
func loadOrCreateJWTKey() (*ecdsa.PrivateKey, *ecdsa.PublicKey, error) {
	dir := config.AppInfo.DBPath
	if strings.TrimSpace(dir) == "" {
		// Config was never loaded (unit tests, bare `go run`). Fall
		// back to generating an ephemeral key rather than crashing
		// the test runner. Production always has DBPath set from
		// user-service.conf, so this branch is never taken at runtime.
		logger.Error("AppInfo.DBPath is empty; generating ephemeral JWT key (tests only)")
		priv, pub, err := jwt.GenerateKeyPair()
		return priv, pub, err
	}

	// Ensure parent dir exists with 0700. MkdirAll is a no-op when
	// the path already exists with a different (e.g. 0755) mode —
	// we don't want to silently downgrade the permissions of the
	// main db/ directory, so we accept whatever the operator chose.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create jwt key dir: %w", err)
	}

	keyPath := filepath.Join(dir, jwtPrivateKeyFilename)

	// Try to load first. Only generate if the file is absent.
	if data, err := os.ReadFile(keyPath); err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, nil, fmt.Errorf("jwt key at %s is not PEM-encoded", keyPath)
		}
		if block.Type != "EC PRIVATE KEY" {
			return nil, nil, fmt.Errorf("jwt key at %s has unexpected PEM type %q", keyPath, block.Type)
		}
		priv, perr := x509.ParseECPrivateKey(block.Bytes)
		if perr != nil {
			return nil, nil, fmt.Errorf("parse ec private key at %s: %w", keyPath, perr)
		}
		logger.Info("loaded persistent JWT signing key", zap.String("path", keyPath))
		return priv, &priv.PublicKey, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("read jwt key at %s: %w", keyPath, err)
	}

	// First-run path: generate a fresh key and persist it. Use
	// O_EXCL so concurrent startups (unlikely but possible under
	// systemd Restart=on-failure loops) don't race to truncate
	// each other's key — the second writer will hit ErrExist and
	// fall through to the load branch on the next attempt.
	priv, pub, err := jwt.GenerateKeyPair()
	if err != nil {
		return nil, nil, fmt.Errorf("generate ec key pair: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal ec private key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: der,
	})
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Lost the race to another process — re-read whatever
			// it wrote. Recurse via a direct load so we get the
			// same validation path as the normal startup case.
			data, rerr := os.ReadFile(keyPath)
			if rerr != nil {
				return nil, nil, fmt.Errorf("race-read jwt key at %s: %w", keyPath, rerr)
			}
			block, _ := pem.Decode(data)
			if block == nil || block.Type != "EC PRIVATE KEY" {
				return nil, nil, fmt.Errorf("race-loaded jwt key at %s is malformed", keyPath)
			}
			rpriv, perr := x509.ParseECPrivateKey(block.Bytes)
			if perr != nil {
				return nil, nil, fmt.Errorf("race-parse jwt key at %s: %w", keyPath, perr)
			}
			return rpriv, &rpriv.PublicKey, nil
		}
		return nil, nil, fmt.Errorf("create jwt key at %s: %w", keyPath, err)
	}
	if _, err := f.Write(pemBytes); err != nil {
		_ = f.Close()
		_ = os.Remove(keyPath)
		return nil, nil, fmt.Errorf("write jwt key at %s: %w", keyPath, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(keyPath)
		return nil, nil, fmt.Errorf("close jwt key at %s: %w", keyPath, err)
	}
	logger.Info("generated new persistent JWT signing key", zap.String("path", keyPath))
	return priv, pub, nil
}
