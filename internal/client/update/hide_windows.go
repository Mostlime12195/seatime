//go:build windows

package update

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps the self-test from flashing a console window when the
// daemon (which has none) runs it.
func hideWindow(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
