//go:build !unix

package filelock

import (
	"errors"
	"os"
)

// On a platform without flock(2) there is no cross-process exclusion to offer.
// Acquire fails rather than silently returning an unheld lock, so a caller that
// depends on serialization refuses to write instead of writing unserialized —
// the whole point of this package. CasaNova ships on Linux only; this file
// exists so a cross-compile fails loudly instead of quietly.
var (
	errWouldBlock  = errors.New("would block")
	errInterrupted = errors.New("interrupted")
)

func tryLockEx(*os.File) error {
	return errors.New("cross-process file locking is not supported on this platform")
}

func unlock(*os.File) {}
