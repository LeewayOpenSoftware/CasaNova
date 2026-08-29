/*
 * @Author: LinkLeong dev@lucentopen.org
 * @Date: 2022-06-14 14:33:25
 * @LastEditors: LinkLeong
 * @LastEditTime: 2022-06-14 14:33:49
 * @Description:
 * @Website: https://casanova.dev
 * Copyright (c) 2022-2026 Leeway Open Software.
 */
package encryption

import (
	"crypto/md5"
	"encoding/hex"
)

// Deprecated: GetMD5ByStr computes an unsalted MD5 hash. Do NOT use for
// passwords or security-sensitive data. Kept only for non-security uses
// like MAC address fingerprinting. Use bcrypt or argon2 for credentials.
func GetMD5ByStr(str string) string {
	h := md5.New()
	h.Write([]byte(str))
	return hex.EncodeToString(h.Sum(nil))
}
