// Package encryption provides password hashing using bcrypt.
//
// CasaOS originally used raw MD5 (no salt, fast, broken).
// CasaNova uses bcrypt with cost-14 (~76ms per hash).
//
// Migration strategy: on login, check bcrypt first, then MD5 fallback.
// If MD5 matches, re-hash with bcrypt and update the database.
// This transparently upgrades all users on their next login.
package encryption

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const (
	// BcryptCost is the work factor for bcrypt hashing.
	// Cost 14 = ~76ms per hash on modern hardware.
	// This makes brute force attacks impractical (~13 hashes/sec vs billions for MD5).
	BcryptCost = 14

	// BcryptPrefix identifies bcrypt hashes (they start with "$2a$" or "$2b$")
	BcryptPrefix = "$2"
)

// HashPassword creates a bcrypt hash of the given password.
// Returns the hash string suitable for database storage.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CheckPassword verifies a password against a stored hash.
// Supports both bcrypt (new) and MD5 (legacy) hashes.
// Returns (matches bool, needsUpgrade bool).
//
// If needsUpgrade is true, the caller should re-hash with HashPassword()
// and update the database. This transparently migrates MD5 users to bcrypt.
func CheckPassword(password, storedHash string) (matches bool, needsUpgrade bool) {
	if IsBcryptHash(storedHash) {
		// Modern bcrypt hash
		err := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(password))
		return err == nil, false
	}

	// Legacy MD5 hash (no salt, fast, insecure — but needed for migration).
	// Use constant-time comparison to prevent timing attacks on the hash.
	md5Hash := legacyMD5(password)
	if subtle.ConstantTimeCompare([]byte(md5Hash), []byte(storedHash)) == 1 {
		return true, true // Match, but needs upgrade to bcrypt
	}

	return false, false
}

// IsBcryptHash checks if a hash string is a bcrypt hash.
func IsBcryptHash(hash string) bool {
	return strings.HasPrefix(hash, BcryptPrefix)
}

// legacyMD5 computes the same MD5 hash that CasaOS used.
// Only used for backward compatibility during migration.
func legacyMD5(str string) string {
	h := md5.New()
	h.Write([]byte(str))
	return hex.EncodeToString(h.Sum(nil))
}
