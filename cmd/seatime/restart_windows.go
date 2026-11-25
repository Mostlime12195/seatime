//go:build windows

package main

import "seatime/internal/client/setup"

// restartDaemon starts the binary at exe as a new detached daemon; this one
// then exits. Windows cannot exec in place, and the Startup folder and
// Scheduled Task only launch at login, so nothing else would start it.
// --service makes it wait for the port if this process is slow to let go.
func restartDaemon(exe string, _ []string) error {
	return setup.StartDetached(exe, "daemon", "--service")
}
