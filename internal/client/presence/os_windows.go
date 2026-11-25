package presence

import (
	"context"
	"fmt"
	"syscall"
	"unsafe"
)

// No terminal reading on Windows: its consoles have no access time.
func terminal() Terminal { return nil }

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
)

// winWindows asks user32 for the foreground window's process, and the
// process snapshot for parents. No permission is needed.
type winWindows struct{}

func windows(func(string) string) Windows { return winWindows{} }

func (winWindows) FrontPID(context.Context) (int, error) {
	h, _, _ := procGetForegroundWindow.Call()
	if h == 0 {
		return 0, fmt.Errorf("no foreground window")
	}
	var pid uint32
	procGetWindowThreadProcessID.Call(h, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return 0, fmt.Errorf("no process for the foreground window")
	}
	return int(pid), nil
}

func (winWindows) Parent(pid int) (int, error) {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(snap)
	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		if int(e.ProcessID) == pid {
			return int(e.ParentProcessID), nil
		}
	}
	return 0, fmt.Errorf("process %d not found", pid)
}
