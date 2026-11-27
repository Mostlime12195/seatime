//go:build !windows

package tray

import (
	"os"
	"syscall"
)

func tryLock(path string) (release func(), ok bool) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	return func() { f.Close() }, true
}
