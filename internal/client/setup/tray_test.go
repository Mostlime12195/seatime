package setup

import (
	"encoding/xml"
	"io"
	"os"
	"strings"
	"testing"

	"seatime/internal/client/tray"
)

func trayOpts(m *mock, goos string, headless, running bool) Options {
	o := opts(m, goos, headless)
	o.trayUp = func() bool { return running }
	return o
}

// macOS: the icon is its own launchd job. It is loaded once; after that a
// pass does nothing while the icon runs, and starts the job when it does
// not.
func TestTrayMac(t *testing.T) {
	if !tray.Supported {
		t.Skip("this build has no icon")
	}
	sandbox(t)
	m := &mock{}
	if _, err := EnsureTray(trayOpts(m, "darwin", false, false), "/Users/u/.local/bin/seatime", true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(trayPlistPath())
	if err != nil {
		t.Fatal("no plist")
	}
	plist := string(b)
	for _, want := range []string{"<string>" + TrayLabel + "</string>", "<string>/Users/u/.local/bin/seatime</string><string>tray</string>",
		"<key>RunAtLoad</key><true/>", "<key>SuccessfulExit</key><false/>", "<string>Interactive</string>", "<string>Aqua</string>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %s", want)
		}
	}
	d := xml.NewDecoder(strings.NewReader(plist))
	for {
		if _, err := d.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("plist is not XML: %v", err)
		}
	}
	calls := strings.Join(m.calls, "\n")
	if !strings.Contains(calls, "launchctl bootstrap gui/") || m.started != 0 {
		t.Fatalf("first pass: %q, started %d", calls, m.started)
	}

	// running: nothing to do
	m = &mock{}
	if _, err := EnsureTray(trayOpts(m, "darwin", false, true), "/Users/u/.local/bin/seatime", true); err != nil || len(m.calls) != 0 {
		t.Fatalf("running: %v %q", err, m.calls)
	}
	// not running: launchd starts its job
	if _, err := EnsureTray(trayOpts(m, "darwin", false, false), "/Users/u/.local/bin/seatime", true); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 1 || !strings.HasPrefix(m.calls[0], "launchctl kickstart gui/") || !strings.HasSuffix(m.calls[0], "/"+TrayLabel) {
		t.Fatalf("not running: %q", m.calls)
	}
	// hidden by the person: left alone until the next login
	tray.Hide()
	m = &mock{}
	if _, err := EnsureTray(trayOpts(m, "darwin", false, false), "/Users/u/.local/bin/seatime", true); err != nil || len(m.calls) != 0 || m.started != 0 {
		t.Fatalf("hidden: %v %q", err, m.calls)
	}
	tray.Unhide()
	// the binary moved: the job is loaded again from the new plist
	m = &mock{}
	if _, err := EnsureTray(trayOpts(m, "darwin", false, true), "/new/seatime", true); err != nil {
		t.Fatal(err)
	}
	if calls := strings.Join(m.calls, "\n"); !strings.Contains(calls, "launchctl bootout") || !strings.Contains(calls, "launchctl bootstrap") {
		t.Fatalf("moved: %q", calls)
	}
	// launchctl refuses: the icon is started directly, the plist stays for the next login
	os.Remove(trayPlistPath())
	m = &mock{fail: map[string]string{"launchctl": "Bootstrap failed: 5: Input/output error"}}
	if _, err := EnsureTray(trayOpts(m, "darwin", false, false), "/g", true); err != nil || m.started != 1 {
		t.Fatalf("refused: %v, started %d", err, m.started)
	}
	if !exists(trayPlistPath()) {
		t.Fatal("refused: no plist for the next login")
	}

	RemoveTray(Options{Runner: m, goos: "darwin"})
	if exists(trayPlistPath()) {
		t.Fatal("plist left after removal")
	}
}

// Windows: no file; whoever calls starts the icon when none runs.
func TestTrayWindows(t *testing.T) {
	if !tray.Supported {
		t.Skip("this build has no icon")
	}
	home := sandbox(t)
	before := snapshot(t, home)
	m := &mock{}
	if _, err := EnsureTray(trayOpts(m, "windows", false, false), `C:\seatime.exe`, true); err != nil || m.started != 1 || len(m.calls) != 0 {
		t.Fatalf("%v started %d %q", err, m.started, m.calls)
	}
	if _, err := EnsureTray(trayOpts(m, "windows", false, true), `C:\seatime.exe`, true); err != nil || m.started != 1 {
		t.Fatalf("running: started %d", m.started)
	}
	if after := snapshot(t, home); len(after) != len(before) {
		t.Fatalf("files written: %v", after)
	}
}

// Linux: an autostart entry for the desktop session. It is started now
// only from inside a session, and never by a daemon outside one.
func TestTrayLinux(t *testing.T) {
	if !tray.Supported {
		t.Skip("this build has no icon")
	}
	sandbox(t)
	m := &mock{}
	if _, err := EnsureTray(trayOpts(m, "linux", true, false), "/home/u/.local/bin/seatime", true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(trayDesktopPath())
	if !strings.Contains(string(b), "Exec=\"/home/u/.local/bin/seatime\" tray\n") || m.started != 0 {
		t.Fatalf("no session: %q, started %d", b, m.started)
	}
	if _, err := EnsureTray(trayOpts(m, "linux", false, false), "/home/u/.local/bin/seatime", false); err != nil || m.started != 0 {
		t.Fatalf("a daemon outside the session started the icon (%v)", err)
	}
	if _, err := EnsureTray(trayOpts(m, "linux", false, false), "/home/u/.local/bin/seatime", true); err != nil || m.started != 1 {
		t.Fatalf("in a session: %v, started %d", err, m.started)
	}
	RemoveTray(Options{Runner: m, goos: "linux"})
	if exists(trayDesktopPath()) {
		t.Fatal("autostart entry left after removal")
	}
}

func TestTrayDryRunTouchesNothing(t *testing.T) {
	home := sandbox(t)
	before := snapshot(t, home)
	for _, goos := range []string{"darwin", "linux", "windows"} {
		o := trayOpts(&mock{}, goos, false, false)
		o.DryRun, o.Runner = true, nil
		if _, err := EnsureTray(o, "/g", true); err != nil {
			t.Fatal(err)
		}
	}
	if after := snapshot(t, home); len(after) != len(before) {
		t.Fatalf("dry run wrote: %v", after)
	}
}
