package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"strconv"
	"strings"
	"time"
)

type HMACSign struct {
	SecretKey []byte
}

func (s HMACSign) Sign(data string, expire int64) string {
	h := hmac.New(sha256.New, s.SecretKey)
	expireTimeStamp := strconv.FormatInt(expire, 10)
	_, err := io.WriteString(h, data+":"+expireTimeStamp)
	if err != nil {
		return ""
	}

	return base64.URLEncoding.EncodeToString(h.Sum(nil)) + ":" + expireTimeStamp
}

func (s HMACSign) Verify(data, sign string) error {
	signSlice := strings.Split(sign, ":")
	// check whether contains expire time
	if signSlice[len(signSlice)-1] == "" {
		return ErrExpireMissing
	}
	// check whether expire time is expired
	expires, err := strconv.ParseInt(signSlice[len(signSlice)-1], 10, 64)
	if err != nil {
		return ErrExpireInvalid
	}
	// if expire time is expired, return error
	if expires < time.Now().Unix() && expires != 0 {
		return ErrSignExpired
	}
	// Constant-time comparison: a plain `!=` on strings leaks timing
	// information byte-by-byte, letting a remote attacker brute-force a
	// valid HMAC signature character by character over enough requests.
	expected := s.Sign(data, expires)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(sign)) != 1 {
		return ErrSignInvalid
	}
	return nil
}

func NewHMACSign(secret []byte) Sign {
	return HMACSign{SecretKey: secret}
}
