package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seatime/internal/adapters"
	"seatime/internal/client/config"
)

type fakeAd struct {
	name                string
	detected, installed bool
	installs            int
}

func (f *fakeAd) Name() string             { return f.name }
func (f *fakeAd) Detect() bool             { return f.detected }
func (f *fakeAd) Installed() (bool, error) { return f.installed, nil }
func (f *fakeAd) Install(string) error     { f.installed = true; f.installs++; return nil }
func (f *fakeAd) Uninstall() error         { f.installed = false; return nil }
func (f *fakeAd) HandleHook(string, []byte, adapters.Sink) ([]byte, error) {
	return nil, nil
}

func sandbox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	t.Setenv("SEATIME_HOME", filepath.Join(home, "seatime"))
}

func TestAutoHookOnce(t *testing.T) {
	sandbox(t)
	a := &fakeAd{name: "a", detected: true}
	b := &fakeAd{name: "b"}
	d := &Daemon{Adapters: []adapters.Adapter{a, b}, HookBin: "/g"}

	d.AutoHookOnce()
	if !a.installed || b.installed {
		t.Fatal("startup pass should hook detected harness only")
	}
	b.detected = true // installed later
	d.AutoHookOnce()
	if !b.installed {
		t.Fatal("new harness not hooked")
	}
	a.installed = false // harness update wiped hooks
	d.AutoHookOnce()
	if !a.installed || a.installs != 2 {
		t.Fatal("not repaired")
	}

	// per-harness opt-out (what `seatime uninstall --harness a` records)
	cfg := config.Load()
	cfg.SetOptOut("a", true)
	config.Save(cfg)
	a.installed = false
	d.AutoHookOnce()
	if a.installed {
		t.Fatal("opted-out harness was re-hooked")
	}

	// global opt-out
	cfg = config.Load()
	cfg.SetOptOut("a", false)
	off := false
	cfg.AutoHook = &off
	config.Save(cfg)
	b.installed = false
	d.AutoHookOnce()
	if a.installed || b.installed {
		t.Fatal("auto_hook:false was ignored")
	}
	// ...and the on-disk key is the documented one
	raw, _ := os.ReadFile(config.ConfigPath())
	if !strings.Contains(string(raw), `"auto_hook": false`) {
		t.Fatal(string(raw))
	}
}

func TestForwardWatcherPicksUpConfigChange(t *testing.T) {
	sandbox(t)
	f := newForwardWatcher("")
	if f.get() != "" {
		t.Fatal()
	}
	f.checked = time.Time{}
	config.Save(config.Config{ForwardURL: "https://up/api"})
	if got := f.get(); got != "https://up/api" {
		t.Fatal(got)
	}
}
