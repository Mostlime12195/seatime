package setup

import (
	"fmt"
	"os"
	"runtime"

	"seatime/internal/adapters"
	"seatime/internal/client/config"
)

// HookResult is what SyncHooks did (or chose not to do) for one harness.
type HookResult struct {
	Name   string
	Status string // installed|repaired|ok|failed|opted-out|absent
	Err    error
}

func (r HookResult) String() string {
	switch r.Status {
	case "failed":
		return fmt.Sprintf("%s: install failed: %v", r.Name, r.Err)
	case "opted-out":
		return fmt.Sprintf("%s: skipped (you uninstalled it; `seatime setup --harness %s` brings it back)", r.Name, r.Name)
	}
	return r.Name + ": " + r.Status
}

// HookBinary is the SeaTime binary harness hooks should call: the stable copy in
// the data dir when present, else the running executable.
func HookBinary() string {
	name := "seatime"
	if isWindows() {
		name = "seatime.exe"
	}
	p := config.BinDir() + string(os.PathSeparator) + name
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return name
}

// SyncHooks installs hooks for detected harnesses that lack them (a harness
// update may have wiped them: "repaired") and records state in cfg. only, when
// non-empty, limits it to those harnesses and overrides their opt-out
// (explicit request). Otherwise opted-out harnesses are left alone.
// It returns the results and whether cfg changed. dry only reports.
func SyncHooks(list []adapters.Adapter, bin string, cfg *config.Config, only []string, dry bool) ([]HookResult, bool) {
	var res []HookResult
	changed := false
	want := map[string]bool{}
	for _, n := range only {
		want[n] = true
	}
	for _, a := range list {
		n := a.Name()
		if len(want) > 0 && !want[n] {
			continue
		}
		if cfg.OptedOut(n) {
			if !want[n] {
				res = append(res, HookResult{Name: n, Status: "opted-out"})
				continue
			}
			if !dry {
				cfg.SetOptOut(n, false)
				changed = true
			}
		}
		if !a.Detect() {
			res = append(res, HookResult{Name: n, Status: "absent"})
			continue
		}
		if ok, _ := a.Installed(); ok {
			if !cfg.WasHooked(n) && !dry {
				cfg.SetHooked(n, true)
				changed = true
			}
			res = append(res, HookResult{Name: n, Status: "ok"})
			continue
		}
		status := "installed"
		if cfg.WasHooked(n) {
			status = "repaired"
		}
		if dry {
			res = append(res, HookResult{Name: n, Status: "would " + status[:len(status)-2]})
			continue
		}
		if err := a.Install(bin); err != nil {
			res = append(res, HookResult{Name: n, Status: "failed", Err: err})
			continue
		}
		cfg.SetHooked(n, true)
		changed = true
		res = append(res, HookResult{Name: n, Status: status})
	}
	return res, changed
}

// UninstallHarness removes one harness's hooks and records the opt-out so
// neither setup nor the daemon puts them back.
func UninstallHarness(o Options, list []adapters.Adapter, cfg *config.Config, names []string) error {
	for _, n := range names {
		var found adapters.Adapter
		for _, a := range list {
			if a.Name() == n {
				found = a
			}
		}
		if found == nil {
			return fmt.Errorf("unknown harness %q", n)
		}
		if o.DryRun {
			o.say("harness %-12s would remove hooks and stop auto-hooking it", n)
			continue
		}
		if ok, _ := found.Installed(); ok {
			if err := found.Uninstall(); err != nil {
				return fmt.Errorf("%s: %w", n, err)
			}
		}
		cfg.SetOptOut(n, true)
		cfg.SetHooked(n, false)
		o.say("harness %-12s hooks removed; SeaTime won't hook it again unless you run `seatime setup --harness %s`", n, n)
	}
	return nil
}

func isWindows() bool { return runtime.GOOS == "windows" }
