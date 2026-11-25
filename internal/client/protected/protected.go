// Package protected keeps the background daemon out of the folders macOS
// guards with a "would like to access files in your Documents folder" prompt
// (TCC: Desktop, Documents, Downloads, iCloud Drive, removable and network
// volumes, and the media folders). A background process that opens anything
// there raises a dialog, which breaks the zero-prompt install.
//
// The rule: the daemon never opens, stats or lists a path under these folders.
// Work that needs a project directory (finding its git root, reading its
// commits) is done by the hook process instead, which runs inside the
// terminal or editor the user already granted. The guard is switched on by
// `seatime daemon` on macOS; tests switch it on everywhere.
package protected

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
)

var enabled atomic.Bool

// Enable turns the guard on for this process (the daemon calls it on macOS).
func Enable(on bool) { enabled.Store(on) }

// Enabled reports whether the guard is on.
func Enabled() bool { return enabled.Load() }

// ShouldEnable reports whether the daemon should enable the guard on this OS.
// SEATIME_PROTECTED_DIRS=1 forces it on elsewhere (for testing the macOS path).
func ShouldEnable() bool {
	return runtime.GOOS == "darwin" || os.Getenv("SEATIME_PROTECTED_DIRS") == "1"
}

// Dirs lists the guarded folders for a home directory.
func Dirs(home string) []string {
	out := []string{"/Volumes"}
	if home == "" {
		return out
	}
	for _, d := range []string{"Desktop", "Documents", "Downloads", "Library/Mobile Documents",
		"Library/CloudStorage", "Pictures", "Movies", "Music"} {
		out = append(out, filepath.Join(home, filepath.FromSlash(d)))
	}
	return out
}

// Under reports whether p is one of dirs or inside one, ignoring the guard
// switch. Comparison is case-insensitive (macOS file systems usually are).
func Under(p string, dirs []string) bool {
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	p = strings.ToLower(filepath.Clean(p))
	for _, d := range dirs {
		d = strings.ToLower(filepath.Clean(d))
		if p == d || strings.HasPrefix(p, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Blocked reports whether this process must not touch p: the guard is on and
// p is under a guarded folder of home.
func Blocked(home, p string) bool {
	return Enabled() && Under(p, Dirs(home))
}
