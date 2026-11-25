package presence

import "testing"

// On Wayland the front window is not asked at all (most compositors do not
// say which window is in front, and the X server sees only X11 windows).
func TestNoFrontWindowOnWayland(t *testing.T) {
	if w := windows(env(map[string]string{"WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0"})); w != nil {
		t.Fatalf("front window on Wayland: %#v", w)
	}
	if w := windows(env(map[string]string{"DISPLAY": ":0"})); w == nil {
		t.Fatal("no front window on X11")
	}
}
