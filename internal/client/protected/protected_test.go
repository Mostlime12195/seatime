package protected

import (
	"path/filepath"
	"testing"
)

func TestUnder(t *testing.T) {
	home := "/Users/kid"
	dirs := Dirs(home)
	for p, want := range map[string]bool{
		"/Users/kid/Documents":                           true,
		"/Users/kid/Documents/app/main.go":               true,
		"/Users/kid/documents/app":                       true, // APFS is case-insensitive by default
		"/Users/kid/Desktop/x":                           true,
		"/Users/kid/Downloads":                           true,
		"/Users/kid/Library/Mobile Documents/a":          true,
		"/Users/kid/Library/CloudStorage/Dropbox/a":      true,
		"/Volumes/USB/code":                              true,
		"/Users/kid/DocumentsOld/app":                    false,
		"/Users/kid/code/app":                            false,
		"/Users/kid/Library/Application Support/seatime": false,
		"/Users/kid":                                     false,
		"relative/Documents":                             false,
		"":                                               false,
	} {
		if got := Under(filepath.FromSlash(p), dirs); got != want {
			t.Errorf("Under(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestBlockedFollowsSwitch(t *testing.T) {
	defer Enable(false)
	Enable(false)
	if Blocked("/Users/kid", "/Users/kid/Documents/a") {
		t.Fatal("guard off: nothing blocked")
	}
	Enable(true)
	if !Blocked("/Users/kid", "/Users/kid/Documents/a") || Blocked("/Users/kid", "/Users/kid/code/a") {
		t.Fatal("guard on")
	}
}
