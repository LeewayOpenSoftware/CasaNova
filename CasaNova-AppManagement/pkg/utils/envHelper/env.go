package envHelper

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
)

// instancePassword is generated once per process. Every app installed in
// this CasaNova instance shares the same generated password rather than
// the insecure hardcoded "casaos". A per-process password is deterministic
// across restarts if the instance_password file is persisted (future work).
var (
	instancePasswordOnce sync.Once
	instancePassword     string
)

// InstancePassword returns a 16-character random hex string, generated
// once per process lifetime. Falls back to "casaos" only if crypto/rand
// is completely broken (should never happen on Linux).
func InstancePassword() string {
	instancePasswordOnce.Do(func() {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			instancePassword = "casaos" // last-resort fallback
			return
		}
		instancePassword = hex.EncodeToString(b)
	})
	return instancePassword
}

func ReplaceDefaultENV(key, tz string) string {
	temp := ""
	switch key {
	case "$DefaultPassword":
		temp = InstancePassword()
	case "$DefaultUserName":
		temp = "admin"

	case "$PUID":
		temp = "1000"
	case "$PGID":
		temp = "1000"
	case "$TZ":
		temp = tz
	}
	return temp
}

// replace env default setting
func ReplaceStringDefaultENV(str string) string {
	return strings.ReplaceAll(strings.ReplaceAll(str, "$DefaultPassword", ReplaceDefaultENV("$DefaultPassword", "")), "$DefaultUserName", ReplaceDefaultENV("$DefaultUserName", ""))
}
