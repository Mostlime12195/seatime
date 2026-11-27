package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/tray"
)

// The menu bar icon (package tray, `seatime tray`) is a second process.
// EnsureTray makes it start at login and starts it now; the daemon calls
// it too, so a computer that updates from a version without an icon gets
// one with no command.
//
//	macOS    its own LaunchAgent: launchd starts it at login, as an
//	         interactive job (the daemon's job is a background one, and
//	         a menu drawn from there would be slow), and again if it
//	         crashes
//	Windows  the daemon starts it when the daemon starts, which is at login
//	Linux    an XDG autostart entry starts it with the desktop session;
//	         a daemon that systemd runs outside that session cannot

// TrayLabel is the icon's launchd job.
const TrayLabel = "dev.hackclub.seatime.tray"

func trayPlistPath() string {
	return filepath.Join(config.Home(), "Library", "LaunchAgents", TrayLabel+".plist")
}
func trayDesktopPath() string { return filepath.Join(xdgConfig(), "autostart", "seatime-tray.desktop") }

// trayPlist starts the icon at login, in a graphical session only. It is
// started again only when it exits with an error: an icon that was hidden
// or turned off exits cleanly and stays away, and one that has to start
// again on a new version exits with an error on purpose.
func trayPlist(exe string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>tray</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>ThrottleInterval</key><integer>30</integer>
<key>ProcessType</key><string>Interactive</string>
<key>LimitLoadToSessionType</key><string>Aqua</string>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string>%s</dict>
<key>StandardOutPath</key><string>/dev/null</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, TrayLabel, xmlText(exe), launchdPATH, sandboxEnv(), xmlText(filepath.Join(config.DataDir(), "tray.err")))
}

func trayDesktopEntry(exe string) string {
	return fmt.Sprintf("[Desktop Entry]\nType=Application\nName=SeaTime\nComment=SeaTime status icon\nExec=\"%s\" tray\nTerminal=false\nNoDisplay=true\nX-GNOME-Autostart-enabled=true\n", exe)
}

func (o Options) trayRunning() bool {
	if o.trayUp != nil {
		return o.trayUp()
	}
	return tray.Running()
}

// EnsureTray makes the icon start at login and, when none is running and
// the person has not hidden it, starts it now; started says that it did.
// spawn says whether this process may start it itself where the system
// does not (false for a daemon outside the desktop session). A build
// without an icon does nothing.
func EnsureTray(o Options, exe string, spawn bool) (started bool, err error) {
	if !tray.Supported {
		return false, nil
	}
	s := newSvc(o, &config.Config{}, exe)
	genv := os.Getenv
	if o.getenv != nil {
		genv = o.getenv
	}
	wanted := !o.trayRunning() && !tray.Hidden()
	write := func(path, content string) error {
		o.say("tray: writing %s", path)
		if o.DryRun {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(content), 0o644)
	}
	start := func() (bool, error) {
		if !wanted || !spawn {
			return false, nil
		}
		o.say("tray: starting the icon")
		if o.DryRun {
			return false, nil
		}
		return true, s.r.Start(exe, "tray")
	}
	switch s.goos {
	case "darwin":
		if _, err := s.r.LookPath("launchctl"); err != nil {
			return start()
		}
		p, want := trayPlistPath(), trayPlist(exe)
		domain := "gui/" + strconv.Itoa(os.Getuid())
		if b, err := os.ReadFile(p); err == nil && string(b) == want {
			if !wanted {
				return false, nil
			}
			// loaded at login but not running: it crashed and launchd gave
			// up, or it was started before this plist existed
			if _, err := s.run("", "launchctl", "kickstart", domain+"/"+TrayLabel); err == nil {
				return true, nil
			}
		} else {
			if !o.DryRun {
				if err := os.MkdirAll(config.DataDir(), 0o700); err != nil { // for StandardErrorPath
					return false, err
				}
			}
			if err := write(p, want); err != nil {
				return false, err
			}
			_, _ = s.run("", "launchctl", "bootout", domain+"/"+TrayLabel)
		}
		out, err := s.run("", "launchctl", "bootstrap", domain, p) // RunAtLoad starts it
		if err != nil && !s.o.noWait {
			time.Sleep(time.Second) // bootout finishes asynchronously
			out, err = s.run("", "launchctl", "bootstrap", domain, p)
		}
		if err != nil {
			o.say("tray: launchctl did not load it now (%s); it loads at the next login", errText(out, err))
			return start()
		}
		return !o.DryRun, nil
	case "windows":
		return start()
	}
	p, want := trayDesktopPath(), trayDesktopEntry(exe)
	if b, err := os.ReadFile(p); err != nil || string(b) != want {
		if err := write(p, want); err != nil {
			return false, err
		}
	}
	if genv("DISPLAY") == "" && genv("WAYLAND_DISPLAY") == "" {
		return false, nil // no desktop session here: it starts with the next one
	}
	return start()
}

// RemoveTray takes back what EnsureTray added and stops a running icon.
func RemoveTray(o Options) {
	s := newSvc(o, &config.Config{}, "")
	if p := trayPlistPath(); exists(p) {
		o.say("tray: removing %s", p)
		_, _ = s.run("", "launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+TrayLabel)
		if !o.DryRun {
			_ = os.Remove(p)
		}
	}
	if p := trayDesktopPath(); exists(p) {
		o.say("tray: removing %s", p)
		if !o.DryRun {
			_ = os.Remove(p)
		}
	}
	if pid := tray.PID(); pid > 0 && !o.DryRun {
		o.say("tray: stopping the icon (pid %d)", pid)
		_ = s.r.Kill(pid)
		for i := 0; i < 20 && tray.Running(); i++ { // it holds a file in the data dir
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
