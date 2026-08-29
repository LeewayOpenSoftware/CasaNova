package random

import (
	cryptorand "crypto/rand"
	"math/big"
)

// RandomString generates a cryptographically secure random string of length n.
// Used for password reset flows — output must be crypto-grade, not math/rand.
//
// The previous implementation used math/rand seeded with time.Now().UnixNano(),
// which is predictable to anyone who knows the approximate call time.
func RandomString(n int, onlyLetter bool) string {
	var letters []rune
	if onlyLetter {
		letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	} else {
		letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	}

	b := make([]rune, n)
	max := big.NewInt(int64(len(letters)))
	for i := range b {
		idx, err := cryptorand.Int(cryptorand.Reader, max)
		if err != nil {
			// crypto/rand.Reader failure is catastrophic — no entropy source.
			panic("crypto/rand unavailable: " + err.Error())
		}
		b[i] = letters[idx.Int64()]
	}
	return string(b)
}
