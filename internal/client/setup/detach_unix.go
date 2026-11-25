//go:build !windows

package setup

import (
	"os"
	"os/exec"
	"syscall"
)

func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }

func detach(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
