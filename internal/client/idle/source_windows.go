package idle

import (
	"context"
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	getLastInputInfo = user32.NewProc("GetLastInputInfo")
	getTickCount     = kernel32.NewProc("GetTickCount")
)

// Win32 reads GetLastInputInfo: no permission, no hook.
type Win32 struct{}

func (Win32) Name() string { return "win32" }

func (Win32) Idle(context.Context) (time.Duration, error) {
	var lii struct {
		cbSize uint32
		dwTime uint32
	}
	lii.cbSize = uint32(unsafe.Sizeof(lii))
	if r, _, err := getLastInputInfo.Call(uintptr(unsafe.Pointer(&lii))); r == 0 {
		return 0, fmt.Errorf("GetLastInputInfo: %v", err)
	}
	now, _, _ := getTickCount.Call()
	// both are 32-bit millisecond tick counts; unsigned subtraction survives the 49.7-day wrap
	return time.Duration(uint32(now)-lii.dwTime) * time.Millisecond, nil
}

func defaultSource() Source { return Win32{} }
