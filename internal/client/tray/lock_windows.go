//go:build windows

package tray

import "syscall"

// tryLock opens the file with no sharing: a second open fails while the
// first handle is open.
func tryLock(path string) (release func(), ok bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, false
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, false
	}
	return func() { _ = syscall.CloseHandle(h) }, true
}
