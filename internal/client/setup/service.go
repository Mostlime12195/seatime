package setup

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"seatime/internal/client/config"
)

// Service mechanisms, in the vocabulary of config.ServiceState.Method.
const (
	mSystemd  = "systemd"
	mXDG      = "xdg-autostart"
	mCron     = "cron"
	mLaunchd  = "launchd"
	mStartup  = "startup-folder"
	mSchtasks = "schtasks"
	mDetached = "detached"
)

const cronMarker = "# seatime-daemon"
const launchdLabel = "dev.hackclub.seatime"
const schtaskName = "seatime"

// ServiceResult says how the daemon is kept running after InstallService.
type ServiceResult struct {
	Method  string   // mechanism chosen
	Running bool     // daemon is up right now (as far as we could tell)
	Notes   []string // human-readable: what was tried, what failed, caveats
}

// Probe is a snapshot of a running daemon (zero value = not running).
type Probe struct {
	Alive   bool
	PID     int
	Version string
}

// ---------- paths ----------

func systemdUnitPath() string {
	return filepath.Join(xdgConfig(), "systemd", "user", "seatime.service")
}
func xdgDesktopPath() string { return filepath.Join(xdgConfig(), "autostart", "seatime.desktop") }
func launchdPath() string {
	return filepath.Join(config.Home(), "Library", "LaunchAgents", launchdLabel+".plist")
}
func startupVBSPath() string {
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		appdata = filepath.Join(config.Home(), "AppData", "Roaming")
	}
	return filepath.Join(appdata, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "seatime.vbs")
}

func xdgConfig() string {
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" || os.Getenv("SEATIME_FAKE_HOME") != "" {
		cfg = filepath.Join(config.Home(), ".config")
	}
	return cfg
}

// ---------- plan (pure) ----------

// servicePlan is the ordered list of mechanisms to try for an OS. headless
// means no graphical session (so XDG autostart would never fire).
func servicePlan(goos string, headless bool) []string {
	switch goos {
	case "darwin":
		return []string{mLaunchd, mDetached}
	case "windows":
		return []string{mStartup, mSchtasks, mDetached}
	}
	if headless {
		return []string{mSystemd, mCron, mXDG, mDetached}
	}
	return []string{mSystemd, mXDG, mCron, mDetached}
}

func isHeadless(getenv func(string) string) bool {
	return getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == "" && getenv("XDG_CURRENT_DESKTOP") == ""
}

// ---------- file contents ----------

func systemdUnit(exe string) string {
	return fmt.Sprintf(`[Unit]
Description=seatime background daemon (productive-struggle tracking)

[Service]
ExecStart="%s" daemon --service
Restart=on-failure
RestartSec=30
MemoryMax=64M
Nice=10

[Install]
WantedBy=default.target
`, exe)
}

func desktopEntry(exe string) string {
	return fmt.Sprintf("[Desktop Entry]\nType=Application\nName=seatime\nComment=seatime background daemon\nExec=\"%s\" daemon\nTerminal=false\nNoDisplay=true\nX-GNOME-Autostart-enabled=true\n", exe)
}

// launchdPATH is the daemon's PATH under launchd (whose default is only
// /usr/bin:/bin:/usr/sbin:/sbin): Homebrew on both CPU types, then the system.
const launchdPATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// sandboxEnv carries seatime's location overrides into the agent, so a test
// install into a fake home (CI, scripts/mac-smoke.sh) stays inside it.
func sandboxEnv() string {
	var b strings.Builder
	for _, k := range []string{"SEATIME_HOME", "SEATIME_FAKE_HOME", "SEATIME_SERVER"} {
		if v := os.Getenv(k); v != "" {
			fmt.Fprintf(&b, "<key>%s</key><string>%s</string>", k, xmlText(v))
		}
	}
	return b.String()
}

// launchdPlist is a per-user LaunchAgent: started at login, restarted if it
// exits (at most every 30 s), low CPU and I/O priority. `--service` makes a
// second copy wait for the first instead of exiting (and being respawned).
func launchdPlist(exe string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>daemon</string><string>--service</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>ThrottleInterval</key><integer>30</integer>
<key>ProcessType</key><string>Background</string>
<key>LowPriorityIO</key><true/>
<key>Nice</key><integer>10</integer>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string>%s</dict>
<key>StandardOutPath</key><string>/dev/null</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, launchdLabel, xmlText(exe), launchdPATH, sandboxEnv(), xmlText(filepath.Join(config.DataDir(), "daemon.err")))
}

func startupVBS(exe string) string {
	// wscript runs it with no console window.
	return fmt.Sprintf("Set sh = CreateObject(\"WScript.Shell\")\r\nsh.Run \"\"\"%s\"\" daemon\", 0, False\r\n", exe)
}

func cronLine(exe string) string {
	return "@reboot '" + strings.ReplaceAll(exe, "'", `'\''`) + "' daemon >/dev/null 2>&1 " + cronMarker
}

// cronAdd returns crontab text with exactly one SeaTime @reboot line.
func cronAdd(existing, line string) string {
	rest, _ := cronRemove(existing)
	if rest != "" && !strings.HasSuffix(rest, "\n") {
		rest += "\n"
	}
	return rest + line + "\n"
}

// cronRemove strips seatime's line(s); reports whether any were present.
func cronRemove(existing string) (string, bool) {
	var keep []string
	found := false
	for _, l := range strings.Split(strings.TrimRight(existing, "\n"), "\n") {
		if strings.Contains(l, cronMarker) {
			found = true
			continue
		}
		keep = append(keep, l)
	}
	out := strings.Join(keep, "\n")
	if strings.TrimSpace(out) == "" {
		return "", found
	}
	return out + "\n", found
}

// ---------- installer ----------

type svc struct {
	o    Options
	r    Runner
	exe  string
	cfg  *config.Config
	goos string
}

func newSvc(o Options, cfg *config.Config, exe string) *svc {
	s := &svc{o: o, r: o.runner(), exe: exe, cfg: cfg, goos: runtime.GOOS}
	if o.goos != "" {
		s.goos = o.goos
	}
	return s
}

func (s *svc) run(stdin, name string, args ...string) (string, error) {
	if s.o.Verbose || s.o.DryRun {
		s.o.say("  $ %s %s", name, strings.Join(args, " "))
	}
	return s.r.Run(stdin, name, args...)
}

func (s *svc) write(path, content string) error {
	s.o.say("service: writing %s", path)
	if s.o.DryRun {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func (s *svc) probe() Probe {
	if s.o.Probe == nil {
		return Probe{}
	}
	return s.o.Probe(*s.cfg)
}

// waitAlive polls for the daemon to come up (a few seconds at most).
func (s *svc) waitAlive() bool {
	if s.o.Probe == nil {
		return true // can't tell; assume the mechanism worked
	}
	for i := 0; i < 20; i++ {
		if s.probe().Alive {
			return true
		}
		if s.o.noWait {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// startDetached launches the daemon as an independent background process.
func (s *svc) startDetached() error {
	s.o.say("service: starting the daemon now (detached)")
	if s.o.DryRun {
		return nil
	}
	return s.r.Start(s.exe, "daemon")
}

// InstallService keeps the daemon running across logins/reboots using the first
// mechanism that works, and starts it now. It never returns an error just
// because the preferred mechanism is unavailable; Notes say what happened.
func InstallService(o Options, cfg *config.Config, exe string) (ServiceResult, error) {
	s := newSvc(o, cfg, exe)
	var res ServiceResult
	pr := s.probe()
	stale := pr.Alive && o.Version != "" && pr.Version != "" && pr.Version != o.Version
	genv := os.Getenv
	if o.getenv != nil {
		genv = o.getenv
	}
	plan := servicePlan(s.goos, isHeadless(genv))
	var failures []string
	for _, m := range plan {
		note, err := s.try(m, stale)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s unavailable: %s", m, err))
			s.o.say("service: %s didn't work: %v", m, err)
			continue
		}
		res.Method = m
		res.Notes = append(res.Notes, failures...)
		if note != "" {
			res.Notes = append(res.Notes, note)
		}
		break
	}
	if res.Method == "" {
		return res, fmt.Errorf("could not start the background daemon: %s", strings.Join(failures, "; "))
	}
	s.removeOthers(res.Method)
	cfg.Service = config.ServiceState{Method: res.Method, Note: strings.Join(res.Notes, "; ")}

	// Make sure it is running right now, whatever the mechanism.
	res.Running = s.waitAlive()
	if !res.Running && res.Method != mDetached {
		res.Notes = append(res.Notes, "the service did not bring the daemon up, started it directly instead")
		if err := s.startDetached(); err != nil {
			res.Notes = append(res.Notes, "could not start the daemon: "+err.Error())
		}
		res.Running = s.waitAlive()
	}
	return res, nil
}

// try installs mechanism m. stale means a daemon of an older version is
// running and should be replaced.
func (s *svc) try(m string, stale bool) (string, error) {
	killStale := func() {
		if stale && !s.o.DryRun {
			if pr := s.probe(); pr.PID > 0 {
				_ = s.r.Kill(pr.PID)
				time.Sleep(500 * time.Millisecond)
			}
		}
	}
	startIfDown := func() {
		if s.probe().Alive && !stale {
			return
		}
		killStale()
		if err := s.startDetached(); err != nil {
			s.o.say("service: could not start the daemon now: %v", err)
		}
	}
	switch m {
	case mSystemd:
		if _, err := s.r.LookPath("systemctl"); err != nil {
			return "", fmt.Errorf("systemctl not found")
		}
		p := systemdUnitPath()
		if err := s.write(p, systemdUnit(s.exe)); err != nil {
			return "", err
		}
		undo := func() {
			if !s.o.DryRun {
				_ = os.Remove(p)
			}
		}
		if out, err := s.run("", "systemctl", "--user", "daemon-reload"); err != nil {
			undo()
			return "", fmt.Errorf("%s", errText(out, err))
		}
		verb := "enable"
		if out, err := s.run("", "systemctl", "--user", verb, "--now", "seatime.service"); err != nil {
			undo()
			return "", fmt.Errorf("%s", errText(out, err))
		}
		if stale {
			_, _ = s.run("", "systemctl", "--user", "restart", "seatime.service")
		}
		return "", nil
	case mXDG:
		if err := s.write(xdgDesktopPath(), desktopEntry(s.exe)); err != nil {
			return "", err
		}
		startIfDown()
		return "starts when you log in to your desktop session", nil
	case mCron:
		if _, err := s.r.LookPath("crontab"); err != nil {
			return "", fmt.Errorf("crontab not found")
		}
		out, err := s.run("", "crontab", "-l")
		if err != nil && !strings.Contains(strings.ToLower(out), "no crontab") {
			return "", fmt.Errorf("crontab -l: %s", errText(out, err))
		}
		if err != nil {
			out = ""
		}
		if out, err := s.run(cronAdd(out, cronLine(s.exe)), "crontab", "-"); err != nil {
			return "", fmt.Errorf("crontab -: %s", errText(out, err))
		}
		s.o.say("service: added an @reboot line to your crontab")
		startIfDown()
		return "starts at boot via a crontab @reboot entry", nil
	case mLaunchd:
		if _, err := s.r.LookPath("launchctl"); err != nil {
			return "", fmt.Errorf("launchctl not found")
		}
		p := launchdPath()
		if !s.o.DryRun {
			// launchd will not start a job whose StandardErrorPath folder is missing
			if err := os.MkdirAll(config.DataDir(), 0o700); err != nil {
				return "", err
			}
		}
		if err := s.write(p, launchdPlist(s.exe)); err != nil {
			return "", err
		}
		uid := strconv.Itoa(os.Getuid())
		_, _ = s.run("", "launchctl", "bootout", "gui/"+uid, p)
		killStale() // a detached daemon of an older version would hold the port
		out, err := s.run("", "launchctl", "bootstrap", "gui/"+uid, p)
		if err != nil && !s.o.noWait {
			// bootout finishes asynchronously; "Bootstrap failed: 5" right
			// after it usually clears within a second
			time.Sleep(time.Second)
			out, err = s.run("", "launchctl", "bootstrap", "gui/"+uid, p)
		}
		if err != nil {
			if out2, err2 := s.run("", "launchctl", "load", "-w", p); err2 != nil {
				// The LaunchAgent plist is still loaded at the next login.
				startIfDown()
				return "launchctl refused to load it now (" + errText(out2, err2) + "; " + errText(out, err) + "); it will load at your next login", nil
			}
		}
		return "", nil
	case mStartup:
		p := startupVBSPath()
		if err := s.write(p, startupVBS(s.exe)); err != nil {
			return "", err
		}
		startIfDown()
		return "starts when you log in to Windows (Startup folder)", nil
	case mSchtasks:
		tr := `"` + s.exe + `" daemon`
		if out, err := s.run("", "schtasks", "/Create", "/TN", schtaskName, "/TR", tr, "/SC", "ONLOGON", "/F"); err != nil {
			return "", fmt.Errorf("schtasks: %s", errText(out, err))
		}
		startIfDown()
		return "starts when you log in to Windows (Scheduled Task)", nil
	case mDetached:
		startIfDown()
		return "NOT set to start automatically (no service mechanism worked): run `seatime setup` again after a reboot or log in, or start `seatime daemon` yourself", nil
	}
	return "", fmt.Errorf("unknown mechanism %q", m)
}

// removeOthers cleans up artifacts of mechanisms other than keep so only one
// thing launches the daemon.
func (s *svc) removeOthers(keep string) {
	for _, m := range []string{mSystemd, mXDG, mCron, mLaunchd, mStartup, mSchtasks} {
		if m != keep {
			s.remove(m, true)
		}
	}
}

// remove deletes one mechanism's artifacts (quiet: only if present).
func (s *svc) remove(m string, quiet bool) {
	say := func(f string, a ...any) {
		if !quiet {
			s.o.say(f, a...)
		}
	}
	rm := func(p string) bool {
		if _, err := os.Stat(p); err != nil {
			return false
		}
		say("service: removing %s", p)
		if !s.o.DryRun {
			_ = os.Remove(p)
		}
		return true
	}
	switch m {
	case mSystemd:
		p := systemdUnitPath()
		if _, err := os.Stat(p); err == nil {
			_, _ = s.run("", "systemctl", "--user", "disable", "--now", "seatime.service")
			rm(p)
			_, _ = s.run("", "systemctl", "--user", "daemon-reload")
		}
	case mXDG:
		rm(xdgDesktopPath())
	case mCron:
		if s.cfg.Service.Method != mCron { // never shell out to crontab on machines that never used it
			return
		}
		out, err := s.run("", "crontab", "-l")
		if err != nil {
			return
		}
		rest, found := cronRemove(out)
		if !found {
			return
		}
		say("service: removing the SeaTime @reboot line from your crontab")
		if rest == "" {
			_, _ = s.run("", "crontab", "-r")
		} else {
			_, _ = s.run(rest, "crontab", "-")
		}
	case mLaunchd:
		p := launchdPath()
		if _, err := os.Stat(p); err == nil {
			_, _ = s.run("", "launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid()), p)
			rm(p)
		}
	case mStartup:
		rm(startupVBSPath())
	case mSchtasks:
		if s.cfg.Service.Method == mSchtasks {
			_, _ = s.run("", "schtasks", "/Delete", "/TN", schtaskName, "/F")
		}
	}
}

// UninstallService removes every autostart mechanism and stops the daemon.
func UninstallService(o Options, cfg *config.Config) error {
	s := newSvc(o, cfg, "")
	for _, m := range []string{mSystemd, mXDG, mCron, mLaunchd, mStartup, mSchtasks} {
		s.remove(m, false)
	}
	if pr := s.probe(); pr.Alive && pr.PID > 0 {
		o.say("service: stopping the running daemon (pid %d)", pr.PID)
		if !o.DryRun {
			_ = s.r.Kill(pr.PID)
		}
	}
	if !o.DryRun {
		cfg.Service = config.ServiceState{}
	}
	return nil
}

// ServiceStatus describes how the daemon is kept alive, "" if not set up.
func ServiceStatus(cfg config.Config) (method string, ok bool) {
	paths := map[string]string{mSystemd: systemdUnitPath(), mXDG: xdgDesktopPath(), mLaunchd: launchdPath(), mStartup: startupVBSPath()}
	if m := cfg.Service.Method; m != "" {
		if p, file := paths[m]; file {
			_, err := os.Stat(p)
			return m, err == nil
		}
		return m, true // cron / schtasks / detached: no file to check
	}
	for m, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return m, true
		}
	}
	return "", false
}
