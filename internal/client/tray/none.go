//go:build !(windows || linux || (darwin && cgo))

package tray

// Supported reports whether this build can show an icon. The macOS icon
// needs cgo (Cocoa): a macOS binary cross-built without it has none, and
// everything else works as before.
const Supported = false

// Run reports that this build has no icon.
func Run(Options) error { return ErrUnsupported }

// ReleaseMainThread does nothing in a build without an icon.
func ReleaseMainThread() {}
