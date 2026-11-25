package common

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Offsets persists "how much of each source has been processed" (byte offsets,
// message counts) for watcher adapters, so a daemon restart neither replays
// history nor loses new activity. Keys are caller-chosen (usually file paths).
type Offsets struct {
	path string
	mu   sync.Mutex
	m    map[string]int64
	dirt bool
}

// LoadOffsets reads <stateDir>/offsets.json (missing = empty).
func LoadOffsets(stateDir string) *Offsets {
	o := &Offsets{path: filepath.Join(stateDir, "offsets.json"), m: map[string]int64{}}
	if b, err := os.ReadFile(o.path); err == nil {
		_ = json.Unmarshal(b, &o.m)
	}
	if o.m == nil {
		o.m = map[string]int64{}
	}
	return o
}

// Get returns the stored offset and whether the key was known.
func (o *Offsets) Get(k string) (int64, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	v, ok := o.m[k]
	return v, ok
}

// Set stores an offset (persisted by Flush).
func (o *Offsets) Set(k string, v int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if old, ok := o.m[k]; !ok || old != v {
		o.m[k] = v
		o.dirt = true
	}
}

// Flush writes changes to disk (best effort).
func (o *Offsets) Flush() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.dirt || o.path == "" {
		return
	}
	if len(o.m) > 5000 { // forget the oldest-looking entries rather than grow forever
		o.m = map[string]int64{}
	}
	b, _ := json.Marshal(o.m)
	if err := WriteFile(o.path, b, 0o600); err == nil {
		o.dirt = false
	}
}

// PollLoop calls poll every interval until ctx is done (an immediate first call
// included). It returns ctx.Err(); poll errors are ignored (next tick retries).
func PollLoop(ctx context.Context, interval time.Duration, poll func(now time.Time)) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	poll(time.Now())
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-t.C:
			poll(now)
		}
	}
}

// Stale reports whether an observation from ts is too old to emit as live
// activity (watchers never backfill history).
func Stale(ts, now time.Time) bool { return now.Sub(ts) > 15*time.Minute }

// VSCodeUserDirs lists the "User" directories of VS Code and its common forks
// (the extensions' globalStorage lives under <dir>/globalStorage/<extension id>).
func VSCodeUserDirs() []string {
	h := Home()
	var base string
	switch runtime.GOOS {
	case "darwin":
		base = filepath.Join(h, "Library", "Application Support")
	case "windows":
		base = os.Getenv("APPDATA")
		if base == "" || os.Getenv("SEATIME_FAKE_HOME") != "" {
			base = filepath.Join(h, "AppData", "Roaming")
		}
	default:
		base = filepath.Join(h, ".config")
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && os.Getenv("SEATIME_FAKE_HOME") == "" {
			base = x
		}
	}
	var out []string
	for _, n := range []string{"Code", "Code - Insiders", "VSCodium", "Cursor", "Windsurf"} {
		out = append(out, filepath.Join(base, n, "User"))
	}
	return out
}

// WatchOnly can be embedded by adapters that tail files instead of installing
// hooks: there is nothing to install, so setup reports them as in place once
// Detect() is true, and the daemon's Watcher does the work.
type WatchOnly struct{}

func (WatchOnly) Installed() (bool, error)                        { return true, nil }
func (WatchOnly) Install(string) error                            { return nil }
func (WatchOnly) Uninstall() error                                { return nil }
func (WatchOnly) HandleHook(string, []byte, Sink) ([]byte, error) { return nil, nil }
