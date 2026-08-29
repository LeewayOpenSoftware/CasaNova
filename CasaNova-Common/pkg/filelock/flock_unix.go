//go:build unix

package filelock

import (
	"os"
	"syscall"
)

var (
	errWouldBlock  error = syscall.EWOULDBLOCK
	errInterrupted error = syscall.EINTR
)

func tryLockEx(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
