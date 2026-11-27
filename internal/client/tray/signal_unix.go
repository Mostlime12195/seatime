//go:build !windows

package tray

import (
	"os"
	"syscall"
)

// stopSignals are how the system and SeaTime ask the icon to quit.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
