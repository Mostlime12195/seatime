//go:build windows || (darwin && cgo)

package tray

func sessionOK() bool { return true }

// name does nothing here: on macOS a title would be text in the menu bar.
func name() {}

// again does nothing here: the look is set once the icon exists.
func again(func()) {}
