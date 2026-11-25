package update

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// State is what the updater remembers, in <data dir>/update.json.
type State struct {
	LastCheck time.Time `json:"last_check,omitempty"` // last attempt, successful or not
	LastError string    `json:"last_error,omitempty"` // why the last attempt failed ("" = it worked)
	Latest    string    `json:"latest,omitempty"`     // newest release seen
	// Bad lists releases that failed their self-test here and were rolled
	// back. They are never tried again; the next newer release is.
	Bad []string `json:"bad,omitempty"`
	// Notice is shown once by the next interactive command, then cleared.
	Notice string `json:"notice,omitempty"`
}

func statePath(dir string) string { return filepath.Join(dir, "update.json") }

// LoadState reads the updater state in dir (zero value if none).
func LoadState(dir string) State {
	var s State
	if b, err := os.ReadFile(statePath(dir)); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func saveState(dir string, s State) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := statePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(dir))
}

// TakeNotice returns the pending notice and clears it, so the user is told
// once.
func TakeNotice(dir string) string {
	s := LoadState(dir)
	if s.Notice == "" {
		return ""
	}
	n := s.Notice
	s.Notice = ""
	if saveState(dir, s) != nil {
		return "" // could not clear it: better silent than repeating every run
	}
	return n
}

// ErrBusy: another update is running.
var ErrBusy = errors.New("another update is already running")

// lock makes sure only one updater (the daemon or `seatime update`) works at
// a time. A lock older than staleLock is from a process that died.
func lock(dir string) (func(), error) {
	const staleLock = 30 * time.Minute
	p := filepath.Join(dir, "update.lock")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	for i := 0; i < 2; i++ {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { _ = os.Remove(p) }, nil
		}
		if fi, serr := os.Stat(p); serr == nil && time.Since(fi.ModTime()) > staleLock {
			_ = os.Remove(p)
			continue
		}
		return nil, ErrBusy
	}
	return nil, ErrBusy
}
