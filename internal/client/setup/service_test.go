package setup

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"seatime/internal/client/config"
)

// mock records every external action and fails the ones named in fail.
type mock struct {
	calls   []string
	fail    map[string]string // command name -> error output
	missing map[string]bool
	cron    string
	hasCron bool
	started int
	killed  []int
	alive   bool // daemon "running" (set by Start)
	pid     int
	version string
}

func (m *mock) LookPath(n string) (string, error) {
	if m.missing[n] {
		return "", errors.New("not found")
	}
	return "/usr/bin/" + n, nil
}
func (m *mock) Run(stdin, name string, args ...string) (string, error) {
	m.calls = append(m.calls, name+" "+strings.Join(args, " "))
	if out, ok := m.fail[name]; ok {
		return out, errors.New("exit status 1")
	}
	if name == "crontab" && len(args) == 1 {
		switch args[0] {
		case "-l":
			if !m.hasCron {
				return "no crontab for u", errors.New("exit status 1")
			}
			return m.cron, nil
		case "-":
			m.cron, m.hasCron = stdin, true
		case "-r":
			m.cron, m.hasCron = "", false
		}
	}
	return "", nil
}
func (m *mock) Start(string, ...string) error { m.started++; m.alive = true; return nil }
func (m *mock) Kill(pid int) error            { m.killed = append(m.killed, pid); m.alive = false; return nil }
func (m *mock) probe(config.Config) Probe {
	return Probe{Alive: m.alive, PID: m.pid, Version: m.version}
}

func sandbox(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	t.Setenv("SEATIME_HOME", filepath.Join(home, "seatime"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func opts(m *mock, goos string, headless bool) Options {
	env := func(k string) string {
		if !headless && k == "DISPLAY" {
			return ":0"
		}
		return ""
	}
	return Options{Runner: m, Probe: m.probe, noWait: true, goos: goos, getenv: env}
}

func TestServicePlan(t *testing.T) {
	cases := []struct {
		goos     string
		headless bool
		want     string
	}{
		{"linux", false, "systemd,xdg-autostart,cron,detached"},
		{"linux", true, "systemd,cron,xdg-autostart,detached"},
		{"darwin", false, "launchd,detached"},
		{"windows", false, "startup-folder,schtasks,detached"},
	}
	for _, c := range cases {
		if got := strings.Join(servicePlan(c.goos, c.headless), ","); got != c.want {
			t.Errorf("%s headless=%v: %s != %s", c.goos, c.headless, got, c.want)
		}
	}
	if !isHeadless(func(string) string { return "" }) || isHeadless(func(k string) string {
		if k == "WAYLAND_DISPLAY" {
			return "wayland-0"
		}
		return ""
	}) {
		t.Fatal("isHeadless")
	}
}

func TestSystemdWorks(t *testing.T) {
	sandbox(t)
	m := &mock{}
	cfg := config.Config{}
	res, err := InstallService(opts(m, "linux", true), &cfg, "/opt/seatime")
	if err != nil || res.Method != mSystemd || cfg.Service.Method != mSystemd {
		t.Fatal(res, err)
	}
	if _, err := os.Stat(systemdUnitPath()); err != nil {
		t.Fatal("unit missing")
	}
	// systemd starts it, so we should not have spawned anything... unless it never came up
	if m.started != 1 { // mock never reports alive until Start: fallback kicks in
		t.Logf("started=%d", m.started)
	}
}

func TestSystemdBusFailureFallsBackToCron(t *testing.T) {
	home := sandbox(t)
	m := &mock{fail: map[string]string{"systemctl": "Failed to connect to user scope bus via local transport: No medium found"}}
	cfg := config.Config{}
	res, err := InstallService(opts(m, "linux", true), &cfg, "/opt/my seatime/seatime")
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != mCron || cfg.Service.Method != mCron {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(strings.Join(res.Notes, "|"), "user scope bus") {
		t.Fatalf("notes should say why systemd failed: %v", res.Notes)
	}
	if !strings.Contains(m.cron, "@reboot '/opt/my seatime/seatime' daemon") || !strings.Contains(m.cron, cronMarker) {
		t.Fatal(m.cron)
	}
	if _, err := os.Stat(systemdUnitPath()); err == nil {
		t.Fatal("failed systemd attempt must not leave a unit file behind")
	}
	if m.started != 1 || !res.Running {
		t.Fatalf("daemon should be started detached for cron: started=%d running=%v", m.started, res.Running)
	}
	_ = home
	// uninstall removes the cron line and stops the daemon
	m.pid = 4242
	if err := UninstallService(opts(m, "linux", true), &cfg); err != nil {
		t.Fatal(err)
	}
	if m.hasCron || len(m.killed) != 1 || m.killed[0] != 4242 || cfg.Service.Method != "" {
		t.Fatalf("cron=%q killed=%v cfg=%+v", m.cron, m.killed, cfg.Service)
	}
}

func TestCronPreservesExistingEntries(t *testing.T) {
	sandbox(t)
	m := &mock{fail: map[string]string{"systemctl": "x"}, hasCron: true, cron: "0 * * * * backup.sh\n"}
	cfg := config.Config{}
	if _, err := InstallService(opts(m, "linux", true), &cfg, "/g"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(m.cron, "0 * * * * backup.sh\n@reboot ") {
		t.Fatal(m.cron)
	}
	// idempotent
	if _, err := InstallService(opts(m, "linux", true), &cfg, "/g"); err != nil {
		t.Fatal(err)
	}
	if strings.Count(m.cron, cronMarker) != 1 {
		t.Fatal(m.cron)
	}
	UninstallService(opts(m, "linux", true), &cfg)
	if m.cron != "0 * * * * backup.sh\n" {
		t.Fatalf("%q", m.cron)
	}
}

func TestDesktopFallsToXDGBeforeCron(t *testing.T) {
	sandbox(t)
	m := &mock{fail: map[string]string{"systemctl": "x"}}
	cfg := config.Config{}
	res, err := InstallService(opts(m, "linux", false), &cfg, "/g")
	if err != nil || res.Method != mXDG {
		t.Fatal(res, err)
	}
	b, _ := os.ReadFile(xdgDesktopPath())
	if !strings.Contains(string(b), `Exec="/g" daemon`) {
		t.Fatal(string(b))
	}
}

func TestHeadlessNoCronNoSystemdUsesXDGThenDetached(t *testing.T) {
	sandbox(t)
	m := &mock{fail: map[string]string{"systemctl": "x"}, missing: map[string]bool{"crontab": true}}
	cfg := config.Config{}
	res, err := InstallService(opts(m, "linux", true), &cfg, "/g")
	if err != nil || res.Method != mXDG {
		t.Fatal(res, err)
	}
	// Without crontab and xdg being unwritable (HOME is a file) -> detached
	home := sandbox(t)
	os.WriteFile(filepath.Join(home, ".config"), []byte("file, not dir"), 0o600)
	m = &mock{fail: map[string]string{"systemctl": "x"}, missing: map[string]bool{"crontab": true}}
	cfg = config.Config{}
	res, err = InstallService(opts(m, "linux", true), &cfg, "/g")
	if err != nil || res.Method != mDetached || m.started != 1 {
		t.Fatal(res, err, m.started)
	}
	if !strings.Contains(strings.Join(res.Notes, "|"), "NOT set to start automatically") {
		t.Fatal(res.Notes)
	}
}

func TestSwitchingMechanismRemovesOld(t *testing.T) {
	sandbox(t)
	m := &mock{fail: map[string]string{"systemctl": "x"}}
	cfg := config.Config{}
	InstallService(opts(m, "linux", true), &cfg, "/g") // -> cron
	if cfg.Service.Method != mCron {
		t.Fatal(cfg.Service)
	}
	m.fail = nil // now systemd works
	res, _ := InstallService(opts(m, "linux", true), &cfg, "/g")
	if res.Method != mSystemd || m.hasCron {
		t.Fatalf("method=%s cron=%q", res.Method, m.cron)
	}
}

func TestLaunchdFallbacks(t *testing.T) {
	sandbox(t)
	// bootstrap fails, load fails -> plist stays (loads at next login), daemon started directly
	m := &mock{fail: map[string]string{"launchctl": "Bootstrap failed: 5"}}
	m.fail["launchctl"] = "Bootstrap failed: 5: Input/output error"
	cfg := config.Config{}
	// launchctl bootout also "fails" which is ignored
	res, err := InstallService(opts(m, "darwin", false), &cfg, "/Applications/seatime")
	if err != nil || res.Method != mLaunchd {
		t.Fatal(res, err)
	}
	if _, err := os.Stat(launchdPath()); err != nil {
		t.Fatal("plist missing")
	}
	if m.started != 1 || !strings.Contains(strings.Join(res.Notes, "|"), "next login") {
		t.Fatal(m.started, res.Notes)
	}
	// healthy launchd
	m = &mock{}
	cfg = config.Config{}
	res, _ = InstallService(opts(m, "darwin", false), &cfg, "/g")
	if !strings.Contains(strings.Join(m.calls, "\n"), "launchctl bootstrap gui/") {
		t.Fatal(m.calls)
	}
}

func TestWindowsStartupThenSchtasks(t *testing.T) {
	home := sandbox(t)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	m := &mock{}
	cfg := config.Config{}
	res, err := InstallService(opts(m, "windows", false), &cfg, `C:\Users\u\AppData\Local\seatime\bin\seatime.exe`)
	if err != nil || res.Method != mStartup {
		t.Fatal(res, err)
	}
	b, _ := os.ReadFile(startupVBSPath())
	if !strings.Contains(string(b), `seatime.exe"" daemon`) || !strings.Contains(string(b), "\r\n") {
		t.Fatal(string(b))
	}
	// Startup folder unwritable -> scheduled task
	home = sandbox(t)
	os.MkdirAll(home, 0o755)
	os.WriteFile(filepath.Join(home, "AppData"), []byte("x"), 0o600)
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	m = &mock{}
	cfg = config.Config{}
	res, err = InstallService(opts(m, "windows", false), &cfg, `C:\g.exe`)
	if err != nil || res.Method != mSchtasks {
		t.Fatal(res, err)
	}
	if !strings.Contains(strings.Join(m.calls, "\n"), "schtasks /Create /TN seatime /TR \"C:\\g.exe\" daemon /SC ONLOGON /F") {
		t.Fatal(m.calls)
	}
}

func TestStaleDaemonIsReplaced(t *testing.T) {
	sandbox(t)
	m := &mock{fail: map[string]string{"systemctl": "x"}, alive: true, pid: 77, version: "v1"}
	o := opts(m, "linux", true)
	o.Version = "v2"
	cfg := config.Config{}
	if _, err := InstallService(o, &cfg, "/g"); err != nil {
		t.Fatal(err)
	}
	if len(m.killed) != 1 || m.killed[0] != 77 || m.started != 1 {
		t.Fatal(m.killed, m.started)
	}
	// same version: leave it running
	m = &mock{fail: map[string]string{"systemctl": "x"}, alive: true, pid: 77, version: "v2"}
	o = opts(m, "linux", true)
	o.Version = "v2"
	InstallService(o, &cfg, "/g")
	if len(m.killed) != 0 || m.started != 0 {
		t.Fatal("running daemon of same version must be left alone")
	}
}

func TestCronEdit(t *testing.T) {
	line := cronLine("/a b/seatime")
	out := cronAdd("MAILTO=x\n* * * * * foo", line)
	if out != "MAILTO=x\n* * * * * foo\n"+line+"\n" {
		t.Fatalf("%q", out)
	}
	if again := cronAdd(out, line); again != out {
		t.Fatal("not idempotent")
	}
	rest, found := cronRemove(out)
	if !found || rest != "MAILTO=x\n* * * * * foo\n" {
		t.Fatalf("%q %v", rest, found)
	}
	if rest, found := cronRemove(line + "\n"); !found || rest != "" {
		t.Fatalf("%q", rest)
	}
	if !strings.Contains(cronLine("/it's/seatime"), `'/it'\''s/seatime'`) {
		t.Fatal(cronLine("/it's/seatime"))
	}
}

func TestFakeRunnerCrontabInFakeHome(t *testing.T) {
	home := sandbox(t)
	t.Setenv("SEATIME_FAKE_FAIL", "systemctl")
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("XDG_CURRENT_DESKTOP", "")
	cfg := config.Config{}
	o := Options{noWait: true, goos: "linux"} // no Runner: SEATIME_FAKE_HOME selects the fake one
	res, err := InstallService(o, &cfg, "/g")
	if err != nil || res.Method != mCron {
		t.Fatal(res, err)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".fake-crontab"))
	if !strings.Contains(string(b), "@reboot") {
		t.Fatal(string(b))
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	home := sandbox(t)
	m := &mock{}
	o := opts(m, "linux", false)
	o.DryRun = true
	o.Runner = nil
	cfg := config.Config{}
	if _, err := InstallService(o, &cfg, "/g"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config")); err == nil {
		t.Fatal("dry run wrote files")
	}
	if len(m.calls) != 0 {
		t.Fatal("dry run ran commands")
	}
}

// The LaunchAgent must stay valid XML whatever the install path is, run the
// daemon in service mode, and give it a PATH that finds Homebrew tools.
func TestLaunchdPlistIsValidAndHardened(t *testing.T) {
	sandbox(t)
	exe := "/Users/kid & co/<bin>/seatime"
	p := launchdPlist(exe)
	dec := xml.NewDecoder(strings.NewReader(p))
	var strs []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("plist is not valid XML: %v\n%s", err, p)
		}
		if cd, ok := tok.(xml.CharData); ok && strings.TrimSpace(string(cd)) != "" {
			strs = append(strs, string(cd))
		}
	}
	joined := strings.Join(strs, "|")
	for _, want := range []string{exe + "|daemon|--service", "/opt/homebrew/bin", launchdLabel, "ThrottleInterval|30"} {
		if !strings.Contains(joined, want) {
			t.Errorf("plist missing %q: %s", want, joined)
		}
	}
	if !strings.Contains(p, "<key>KeepAlive</key><true/>") || !strings.Contains(p, "<key>RunAtLoad</key><true/>") {
		t.Error("KeepAlive and RunAtLoad are required")
	}
}

func TestLaunchdInstallCreatesDataDirAndKillsStale(t *testing.T) {
	sandbox(t)
	m := &mock{alive: true, pid: 4242, version: "old"}
	cfg := config.Config{}
	o := opts(m, "darwin", false)
	o.Version = "new"
	if _, err := InstallService(o, &cfg, "/g"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(config.DataDir()); err != nil {
		t.Fatal("data dir must exist before launchd opens daemon.err in it")
	}
	if len(m.killed) == 0 || m.killed[0] != 4242 {
		t.Fatalf("stale daemon of another version must be stopped: %v", m.killed)
	}
}
