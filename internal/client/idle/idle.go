// Package idle reads the operating system's idle timer: seconds since the
// last keyboard or mouse input, and nothing else (no key content, no window,
// no permission prompt). The daemon samples it once a minute and records
// the stretches without input (Recorder); the scoring rule may only REMOVE
// time with it, and when no idle timer is available hours stay and carry the
// "no idle signal" label.
//
// Under rule heartbeat/1 the idle timer never changes hours: what it
// records is shown with the records.
package idle

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrUnavailable means this machine has no idle timer we can read.
var ErrUnavailable = errors.New("idle timer unavailable")

// Source reads the time since the last user input.
type Source interface {
	Name() string
	Idle(ctx context.Context) (time.Duration, error)
}

// None is the source used when no idle timer exists; Idle always fails.
type None struct{ Reason string }

func (None) Name() string { return "none" }
func (n None) Idle(context.Context) (time.Duration, error) {
	if n.Reason == "" {
		return 0, ErrUnavailable
	}
	return 0, fmt.Errorf("%w: %s", ErrUnavailable, n.Reason)
}

// Func adapts a function (tests, fakes).
type Func struct {
	N string
	F func(ctx context.Context) (time.Duration, error)
}

func (f Func) Name() string                                    { return f.N }
func (f Func) Idle(ctx context.Context) (time.Duration, error) { return f.F(ctx) }

// Default returns the idle source for this OS (see the per-OS files).
func Default() Source { return defaultSource() }

// runner runs a command with a short timeout and returns its stdout.
type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// ---- macOS: ioreg -c IOHIDSystem, "HIDIdleTime" in nanoseconds ----

// IOReg reads HIDIdleTime from ioreg. No cgo, no permission.
type IOReg struct{ run runner }

func (IOReg) Name() string { return "ioreg" }

func (s IOReg) Idle(ctx context.Context) (time.Duration, error) {
	run := s.run
	if run == nil {
		run = execRun
	}
	out, err := run(ctx, "/usr/sbin/ioreg", "-c", "IOHIDSystem", "-d", "4")
	if err != nil {
		return 0, fmt.Errorf("ioreg: %w", err)
	}
	return ParseIOReg(out)
}

// ParseIOReg extracts the first HIDIdleTime (nanoseconds) from ioreg output.
func ParseIOReg(out []byte) (time.Duration, error) {
	for _, line := range strings.Split(string(out), "\n") {
		i := strings.Index(line, `"HIDIdleTime"`)
		if i < 0 {
			continue
		}
		rest := strings.TrimSpace(line[i+len(`"HIDIdleTime"`):])
		rest = strings.TrimSpace(strings.TrimPrefix(rest, "="))
		if j := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' }); j >= 0 {
			rest = rest[:j]
		}
		ns, err := strconv.ParseUint(rest, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("ioreg: bad HIDIdleTime %q", rest)
		}
		return time.Duration(ns), nil
	}
	return 0, errors.New("ioreg: no HIDIdleTime in output")
}

// ---- Linux: GNOME Mutter over D-Bus, else xprintidle on X11 ----

// GnomeMutter asks GNOME's idle monitor (works on Wayland and X11).
type GnomeMutter struct{ run runner }

func (GnomeMutter) Name() string { return "gnome" }

func (s GnomeMutter) Idle(ctx context.Context) (time.Duration, error) {
	run := s.run
	if run == nil {
		run = execRun
	}
	out, err := run(ctx, "gdbus", "call", "--session", "--dest", "org.gnome.Mutter.IdleMonitor",
		"--object-path", "/org/gnome/Mutter/IdleMonitor/Core", "--method", "org.gnome.Mutter.IdleMonitor.GetIdletime")
	if err != nil {
		return 0, fmt.Errorf("gdbus: %w", err)
	}
	// "(uint64 1234,)"
	f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "(),"))
	if len(f) == 0 {
		return 0, fmt.Errorf("gdbus: unexpected %q", out)
	}
	ms, err := strconv.ParseUint(strings.TrimSuffix(f[len(f)-1], ","), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("gdbus: unexpected %q", out)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// XPrintIdle runs xprintidle (milliseconds) on X11.
type XPrintIdle struct{ run runner }

func (XPrintIdle) Name() string { return "xprintidle" }

func (s XPrintIdle) Idle(ctx context.Context) (time.Duration, error) {
	run := s.run
	if run == nil {
		run = execRun
	}
	out, err := run(ctx, "xprintidle")
	if err != nil {
		return 0, fmt.Errorf("xprintidle: %w", err)
	}
	ms, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("xprintidle: unexpected %q", out)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// Wayland reports whether the session is a Wayland one (XDG_SESSION_TYPE,
// or WAYLAND_DISPLAY set).
func Wayland(getenv func(string) string) bool {
	return getenv("XDG_SESSION_TYPE") == "wayland" || getenv("WAYLAND_DISPLAY") != ""
}

// LinuxSources are the idle timers that can be right on this Linux session,
// in order, or why there are none. A source that can be wrong is not used:
// on Wayland the X11 counter (xprintidle, through
// XWayland's DISPLAY) sees only X11 windows, so it is never used there.
// GNOME's idle monitor answers on Wayland and X11. KDE, Sway and Hyprland
// share a Wayland idle protocol that SeaTime does not speak, and it stops
// counting while an app holds the screen awake unless asked for its
// input-only form: those sessions are "not measured".
func LinuxSources(getenv func(string) string, has func(bin string) bool) ([]Source, string) {
	wayland := Wayland(getenv)
	x11 := !wayland && (getenv("XDG_SESSION_TYPE") == "x11" || getenv("DISPLAY") != "")
	if !wayland && !x11 && getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return nil, "no graphical session"
	}
	var srcs []Source
	if has("gdbus") {
		srcs = append(srcs, GnomeMutter{})
	}
	if x11 && has("xprintidle") {
		srcs = append(srcs, XPrintIdle{})
	}
	if len(srcs) > 0 {
		return srcs, ""
	}
	if wayland {
		return nil, "a Wayland desktop without GNOME's idle monitor: not measured"
	}
	return nil, "no supported idle interface (GNOME D-Bus or xprintidle)"
}

// First returns the first source that answers now, or None listing why each
// failed. Used where several mechanisms may exist (Linux desktops).
func First(ctx context.Context, srcs ...Source) Source {
	var why []string
	for _, s := range srcs {
		if _, err := s.Idle(ctx); err == nil {
			return s
		} else {
			why = append(why, err.Error())
		}
	}
	return None{Reason: strings.Join(why, "; ")}
}
