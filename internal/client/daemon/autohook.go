package daemon

import (
	"context"
	"os"
	"sync"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/logx"
	"seatime/internal/client/setup"
)

// hookLoop re-runs harness detection at startup and every HookInterval: it
// hooks harnesses installed since setup and repairs hooks a harness update
// removed. Opt-outs ("auto_hook": false, or a harness the user uninstalled)
// are honoured; everything it does is logged.
func (d *Daemon) hookLoop(ctx context.Context) {
	iv := d.HookInterval
	if iv <= 0 {
		iv = 10 * time.Minute
	}
	sleep(ctx, 3*time.Second)
	for ctx.Err() == nil {
		d.AutoHookOnce()
		d.autoWaka()
		sleep(ctx, iv)
	}
}

// AutoHookOnce is one detection pass (exported for tests). It re-reads the
// config so opt-outs written by `seatime uninstall --harness` take effect at once.
func (d *Daemon) AutoHookOnce() []setup.HookResult {
	defer func() {
		if r := recover(); r != nil {
			logx.Printf("auto-hook: panic: %v", r)
		}
	}()
	cfg := config.Load()
	bin := d.HookBin
	if bin == "" {
		bin = setup.HookBinary()
	}
	if !cfg.AutoHookEnabled() {
		res, _ := setup.SyncHooks(d.Adapters, bin, &cfg, nil, true) // only to report (health)
		d.keepHookResults(res)
		return nil
	}
	res, changed := setup.SyncHooks(d.Adapters, bin, &cfg, nil, false)
	d.keepHookResults(res)
	for _, r := range res {
		switch r.Status {
		case "installed":
			logx.Printf("auto-hook: new harness %s detected, hooks installed", r.Name)
		case "repaired":
			logx.Printf("auto-hook: hooks for %s were missing (harness update?), repaired", r.Name)
		case "failed":
			logx.Printf("auto-hook: %s", r)
		}
		if r.Name == "codex" && (r.Status == "installed" || r.Status == "repaired") {
			logx.Printf("auto-hook: Codex needs you to run /hooks once to approve the hooks")
		}
	}
	if changed {
		if err := config.Save(cfg); err != nil {
			logx.Printf("auto-hook: saving config: %v", err)
		}
	}
	return res
}

// keepHookResults keeps the last pass's results for the check-in (health).
func (d *Daemon) keepHookResults(res []setup.HookResult) {
	d.hookMu.Lock()
	d.hookRes = res
	d.hookMu.Unlock()
}

// forwardWatcher serves the WakaTime forward URL, re-reading config.json when
// it changes so `seatime setup` / `seatime uninstall` apply without a daemon restart.
type forwardWatcher struct {
	mu      sync.Mutex
	mtime   time.Time
	size    int64
	url     string
	checked time.Time
}

func newForwardWatcher(initial string) *forwardWatcher {
	return &forwardWatcher{url: initial}
}

func (f *forwardWatcher) get() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if time.Since(f.checked) < 2*time.Second {
		return f.url
	}
	f.checked = time.Now()
	fi, err := os.Stat(config.ConfigPath())
	if err != nil {
		return f.url
	}
	if fi.ModTime() != f.mtime || fi.Size() != f.size {
		f.mtime, f.size = fi.ModTime(), fi.Size()
		f.url = config.Load().ForwardURL
	}
	return f.url
}
