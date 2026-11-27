package tray

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"seatime/internal/client/config"
)

// One icon runs per account on a computer. The running one holds
// <data dir>/tray.lock and writes its process id to tray.pid.

func lockPath() string   { return filepath.Join(config.DataDir(), "tray.lock") }
func pidPath() string    { return filepath.Join(config.DataDir(), "tray.pid") }
func hiddenPath() string { return filepath.Join(config.DataDir(), "tray.hidden") }

// acquire takes the lock, waiting up to wait for an icon that is on its
// way out (it starts its successor before it exits). release frees it.
func acquire(wait time.Duration) (release func(), ok bool) {
	if err := os.MkdirAll(config.DataDir(), 0o700); err != nil {
		return nil, false
	}
	deadline := time.Now().Add(wait)
	for {
		if release, ok := tryLock(lockPath()); ok {
			_ = os.WriteFile(pidPath(), []byte(strconv.Itoa(os.Getpid())), 0o600)
			return func() { _ = os.Remove(pidPath()); release() }, true
		}
		if !time.Now().Before(deadline) {
			return nil, false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Running reports whether an icon is running.
func Running() bool {
	if _, err := os.Stat(lockPath()); err != nil {
		return false
	}
	release, ok := tryLock(lockPath())
	if ok {
		release()
	}
	return !ok
}

// PID is the running icon's process id (0: none is running).
func PID() int {
	if !Running() {
		return 0
	}
	b, err := os.ReadFile(pidPath())
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// Hide records that the person hid the icon. It comes back when the icon
// or the daemon next starts (Unhide), which is at the next login.
func Hide() { _ = os.WriteFile(hiddenPath(), nil, 0o600) }

// Hidden reports whether the person hid the icon and it has not come back.
func Hidden() bool {
	_, err := os.Stat(hiddenPath())
	return err == nil
}

// Unhide forgets that the icon was hidden.
func Unhide() { _ = os.Remove(hiddenPath()) }
