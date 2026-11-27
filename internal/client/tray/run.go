package tray

import "errors"

// Options are what `seatime tray` passes in.
type Options struct {
	Version string // this binary's version
	Port    int    // the daemon's local port
	// OpenURL opens a page in the person's browser.
	OpenURL func(url string) bool
	// SignIn starts a sign-in when none waits for approval yet.
	SignIn func()
	// Check shows the icon and its menu for a moment and quits: a test
	// that this build can draw one here (CI, scripts/mac-smoke.sh).
	Check bool
}

// ErrRestart is what Run returns after an update: the icon should start
// again on the binary at its path.
var ErrRestart = errors.New("tray: start again on the new version")

// ErrUnsupported: this build has no icon (macOS built without cgo), or
// there is no desktop session to show one in.
var ErrUnsupported = errors.New("this build of SeaTime has no menu bar icon")
