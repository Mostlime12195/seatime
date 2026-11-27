//go:build windows

package tray

import "os"

// stopSignals are how the system asks the icon to quit.
var stopSignals = []os.Signal{os.Interrupt}
