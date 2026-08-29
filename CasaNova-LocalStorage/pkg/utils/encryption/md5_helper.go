/*@Author: LinkLeong dev@lucentopen.org
 *@Date: 2022-06-14 14:33:25
 *@LastEditors: LinkLeong
 *@LastEditTime: 2022-06-14 14:33:49
 *@FilePath: /CasaOS/pkg/utils/encryption/md5_helper.go
 *@Description:
 *@Website: https://casanova.dev
 *Copyright (c) 2022-2026 Leeway Open Software.
 */
package encryption

import (
	"crypto/md5" // nolint: gosec
	"encoding/hex"
)

func GetMD5ByStr(str string) string {
	h := md5.New() // nolint: gosec
	h.Write([]byte(str))
	return hex.EncodeToString(h.Sum(nil))
}
