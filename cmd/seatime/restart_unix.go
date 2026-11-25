//go:build !windows

package main

import (
	"os"
	"syscall"
)

// restartDaemon replaces this process with the binary at exe, keeping the
// PID, arguments and environment, so launchd, systemd, cron and a detached
// start all carry on with the new version without noticing.
func restartDaemon(exe string, args []string) error {
	return syscall.Exec(exe, append([]string{exe, "daemon"}, args...), os.Environ())
}
