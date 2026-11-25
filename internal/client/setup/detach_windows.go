//go:build windows

package setup

import (
	"os"
	"os/exec"
	"syscall"
)

func terminate(p *os.Process) error { return p.Kill() }

func detach(c *exec.Cmd) {
	// DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP, no console window.
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200, HideWindow: true}
}
