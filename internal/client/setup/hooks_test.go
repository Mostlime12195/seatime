package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"seatime/internal/adapters"
	"seatime/internal/client/config"
)

// fakeAdapter is a harness whose presence/hook state the test controls.
type fakeAdapter struct {
	name      string
	detected  bool
	installed bool
	installs  int
}

func (f *fakeAdapter) Name() string             { return f.name }
func (f *fakeAdapter) Detect() bool             { return f.detected }
func (f *fakeAdapter) Installed() (bool, error) { return f.installed, nil }
func (f *fakeAdapter) Install(string) error     { f.installed = true; f.installs++; return nil }
func (f *fakeAdapter) Uninstall() error         { f.installed = false; return nil }
func (f *fakeAdapter) HandleHook(string, []byte, adapters.Sink) ([]byte, error) {
	return nil, nil
}
func status(rs []HookResult, n string) string {
	for _, r := range rs {
		if r.Name == n {
			return r.Status
		}
	}
	return "<none>"
}

func TestSyncHooksNewRepairAndOptOut(t *testing.T) {
	a := &fakeAdapter{name: "a", detected: true}
	b := &fakeAdapter{name: "b"} // not installed on the machine yet
	list := []adapters.Adapter{a, b}
	var cfg config.Config

	rs, changed := SyncHooks(list, "/g", &cfg, nil, false)
	if status(rs, "a") != "installed" || status(rs, "b") != "absent" || !changed || !cfg.WasHooked("a") {
		t.Fatalf("%+v", rs)
	}
	// second pass: nothing to do
	rs, changed = SyncHooks(list, "/g", &cfg, nil, false)
	if status(rs, "a") != "ok" || changed || a.installs != 1 {
		t.Fatalf("%+v installs=%d", rs, a.installs)
	}
	// a harness update wipes the hooks -> repaired
	a.installed = false
	rs, _ = SyncHooks(list, "/g", &cfg, nil, false)
	if status(rs, "a") != "repaired" || a.installs != 2 {
		t.Fatalf("%+v", rs)
	}
	// b gets installed later -> picked up as new
	b.detected = true
	rs, _ = SyncHooks(list, "/g", &cfg, nil, false)
	if status(rs, "b") != "installed" {
		t.Fatalf("%+v", rs)
	}
	// explicit uninstall of a: hooks removed, opt-out recorded, never re-hooked
	o := Options{}
	if err := UninstallHarness(o, list, &cfg, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if a.installed || !cfg.OptedOut("a") || cfg.WasHooked("a") {
		t.Fatalf("%+v", cfg)
	}
	rs, _ = SyncHooks(list, "/g", &cfg, nil, false)
	if status(rs, "a") != "opted-out" || a.installed {
		t.Fatalf("%+v", rs)
	}
	// explicit --harness a re-enables it
	rs, _ = SyncHooks(list, "/g", &cfg, []string{"a"}, false)
	if status(rs, "a") != "installed" || cfg.OptedOut("a") || !a.installed {
		t.Fatalf("%+v %+v", rs, cfg)
	}
	if err := UninstallHarness(o, list, &cfg, []string{"nope"}); err == nil {
		t.Fatal("unknown harness should error")
	}
}

func TestSyncHooksDryRunChangesNothing(t *testing.T) {
	a := &fakeAdapter{name: "a", detected: true}
	var cfg config.Config
	rs, changed := SyncHooks([]adapters.Adapter{a}, "/g", &cfg, nil, true)
	if status(rs, "a") != "would install" || changed || a.installed || len(cfg.Hooked) != 0 {
		t.Fatalf("%+v", rs)
	}
}

func TestConfigAutoHookDefault(t *testing.T) {
	var c config.Config
	if !c.AutoHookEnabled() {
		t.Fatal("default must be on")
	}
	f := false
	c.AutoHook = &f
	if c.AutoHookEnabled() {
		t.Fatal()
	}
}

// End-to-end `setup --yes` in a sandboxed home with a real adapter and a wakatime cfg.
func TestSetupYesEndToEnd(t *testing.T) {
	home := sandbox(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.MkdirAll(filepath.Join(home, ".vscode"), 0o755)
	cfgPath := filepath.Join(home, ".wakatime.cfg")
	orig := "[settings]\napi_url = https://hackatime.hackclub.com/api/hackatime/v1\napi_key = k\n"
	os.WriteFile(cfgPath, []byte(orig), 0o600)

	m := &mock{fail: map[string]string{"systemctl": "Failed to connect to user scope bus"}}
	o := opts(m, "linux", true)
	o.Yes, o.Server = true, "https://seatime.example/" // copies the binary to <data>/bin/seatime like a real install
	var sb strings.Builder
	o.Out = &sb
	rep, err := Setup(o)
	if err != nil {
		t.Fatal(err)
	}
	if status(rep.Hooks, "claude-code") != "installed" {
		t.Fatalf("%+v\n%s", rep.Hooks, sb.String())
	}
	if rep.Service.Method != mCron || !rep.Service.Running {
		t.Fatalf("%+v", rep.Service)
	}
	cfg := config.Load()
	const up = "https://hackatime.hackclub.com/api/hackatime/v1"
	if cfg.Server != "https://seatime.example" || !cfg.Waka.Enabled || cfg.ForwardURL != up {
		t.Fatalf("default must keep forwarding to Hackatime: %+v", cfg)
	}
	if rep.Waka == "" || strings.Count(rep.Waka, "\n") != 0 || !strings.Contains(rep.Waka, "forwarded unchanged to "+up) {
		t.Fatalf("waka note should be one line naming the forward: %q", rep.Waka)
	}
	// a later `setup --also-hackatime=false` (no --yes) turns forwarding off;
	// a plain `setup --yes` afterwards keeps it off; =true turns it back on
	no, yes := false, true
	o2 := opts(m, "linux", true)
	o2.AlsoHackatime = &no
	if _, err := Setup(o2); err != nil {
		t.Fatal(err)
	}
	if cfg = config.Load(); cfg.ForwardURL != "" {
		t.Fatalf("opt-out not saved: %+v", cfg)
	}
	o2 = opts(m, "linux", true)
	o2.Yes = true
	if _, err := Setup(o2); err != nil {
		t.Fatal(err)
	}
	if cfg = config.Load(); cfg.ForwardURL != "" {
		t.Fatalf("plain setup flipped the opt-out: %+v", cfg)
	}
	o2 = opts(m, "linux", true)
	o2.AlsoHackatime = &yes
	if _, err := Setup(o2); err != nil {
		t.Fatal(err)
	}
	if cfg = config.Load(); cfg.ForwardURL != up {
		t.Fatalf("re-enable ignored: %+v", cfg)
	}
	var sum strings.Builder
	rep.Summary(&sum, "logged in as x")
	for _, want := range []string{"claude-code", "crontab @reboot", "WakaTime", "logged in as x"} {
		if !strings.Contains(sum.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, sum.String())
		}
	}
	// uninstall restores the cfg exactly and removes everything
	if err := Uninstall(Options{Runner: m, Probe: m.probe, goos: "linux"}, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfgPath); string(b) != orig {
		t.Fatalf("wakatime cfg not restored exactly: %q", b)
	}
	if m.hasCron {
		t.Fatal("cron line left behind")
	}
	if ok, _ := adapters.ByName("claude-code").Installed(); ok {
		t.Fatal("hooks left behind")
	}
	// (Installed() only recognises a binary named seatime; check the text too)
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); strings.Contains(string(b), "hook claude-code") {
		t.Fatalf("hooks left behind: %s", b)
	}
}

func TestSetupYesNoWakaCfgGivesEditorHint(t *testing.T) {
	home := sandbox(t)
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".cursor"), 0o755)
	m := &mock{}
	o := opts(m, "linux", true)
	o.Yes, o.NoCopy = true, true
	rep, err := Setup(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".wakatime.cfg")); err == nil {
		t.Fatal("must not create a wakatime cfg or install extensions")
	}
	if !strings.Contains(strings.Join(rep.Hints, "|"), "Cursor") || config.Load().Waka.Enabled {
		t.Fatalf("%v", rep.Hints)
	}
}

func TestSetupYesRollsBackWakaWhenDaemonWontStart(t *testing.T) {
	home := sandbox(t)
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(home, ".wakatime.cfg")
	orig := "[settings]\napi_key = k\n"
	os.WriteFile(cfgPath, []byte(orig), 0o600)
	m := &neverAlive{mock: mock{fail: map[string]string{"systemctl": "x"}}}
	o := opts(&m.mock, "linux", true)
	o.Runner, o.Probe = m, func(config.Config) Probe { return Probe{} }
	o.Yes, o.NoCopy = true, true
	rep, err := Setup(o)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfgPath); string(b) != orig || rep.Waka != "" || config.Load().Waka.Enabled {
		t.Fatalf("waka should be rolled back: %q", b)
	}
}

type neverAlive struct{ mock }

func (n *neverAlive) Start(string, ...string) error { return nil }

// snapshot maps every regular file under root to its contents.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			b, _ := os.ReadFile(p)
			out[p] = string(b)
		}
		return nil
	})
	return out
}

// macOS install then uninstall in a fake home: the agent, the hooks and the
// proxy setting are gone, every file that existed before is byte-identical,
// and nothing new is left outside seatime's own data dir.
func TestMacInstallUninstallLeavesNoTrace(t *testing.T) {
	home := sandbox(t)
	t.Setenv("HOME", home)
	t.Setenv("SEATIME_HOME", filepath.Join(home, "Library", "Application Support", "seatime"))
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"),
		[]byte("{\n  \"model\": \"opus\",\n    \"hooks\": {\"Stop\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"say done\"}]}]}\n}\n"), 0o644)
	os.WriteFile(filepath.Join(home, ".wakatime.cfg"), []byte("[settings]\n; mine\napi_url = https://hackatime.hackclub.com/api/hackatime/v1\napi_key   =  k\n"), 0o600)
	os.MkdirAll(filepath.Join(home, "Library", "LaunchAgents"), 0o755)
	os.WriteFile(filepath.Join(home, "Library", "LaunchAgents", "com.other.plist"), []byte("<plist/>"), 0o644)
	before := snapshot(t, home)

	m := &mock{}
	o := opts(m, "darwin", false)
	o.Yes = true // copies the binary to <data>/bin/seatime, so hooks are recognisably seatime's
	o.Out = &strings.Builder{}
	rep, err := Setup(o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Service.Method != mLaunchd || status(rep.Hooks, "claude-code") != "installed" {
		t.Fatalf("%+v %+v", rep.Service, rep.Hooks)
	}
	if _, err := os.Stat(launchdPath()); err != nil {
		t.Fatal("agent plist not written")
	}
	if err := Uninstall(Options{Runner: m, Probe: m.probe, goos: "darwin"}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(m.calls, "\n"), "launchctl bootout") {
		t.Fatal("agent must be unloaded")
	}
	after := snapshot(t, home)
	for p, b := range before {
		if after[p] != b {
			t.Errorf("%s changed:\n%q\nwant\n%q", p, after[p], b)
		}
	}
	data := config.DataDir() + string(filepath.Separator)
	for p := range after {
		if _, had := before[p]; !had && !strings.HasPrefix(p, data) {
			t.Errorf("left behind: %s", p)
		}
	}
}
