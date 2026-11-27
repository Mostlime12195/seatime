package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"seatime/internal/adapters"
	"seatime/internal/client/config"
	"seatime/internal/client/daemon"
	"seatime/internal/client/emit"
	"seatime/internal/client/logx"
	"seatime/internal/client/protected"
	"seatime/internal/client/store"
)

func cmdDaemon(args []string) error {
	fs := newFlagSet("daemon")
	service := fs.Bool("service", false, "started by launchd/systemd: wait for an already running daemon instead of exiting")
	fs.Parse(args)
	exe := selfPath() // before any update renames the running file
	trimFile(filepath.Join(config.DataDir(), "daemon.err"), 1<<20)
	// check the log; a damaged one is moved aside and a new one continues
	// the chain from the last checkpoint sent to the server
	st, rec, err := store.OpenChecked(config.DBPath())
	if err != nil {
		return err
	}
	if rec.Quarantined != "" {
		logx.Printf("daemon: the local log was damaged (%s); moved it to %s and started a new one continuing from checkpoint %d",
			rec.Reason, rec.Quarantined, rec.Base)
	}
	p := emit.New(st, config.Home())
	p.NoText = !config.Load().KeepsPromptText()
	if protected.ShouldEnable() {
		// macOS: never open Desktop/Documents/Downloads/iCloud from the
		// background, that raises a permission dialog (see package protected)
		protected.Enable(true)
		p.GuardProtected(config.Home())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := &daemon.Daemon{Version: version, Store: st, Pipeline: p, Adapters: adapters.All(), Service: *service,
		Update: newUpdater(exe, true), OpenBrowser: openBrowser, TrayExe: exe}
	logx.Printf("daemon starting (version %s, pid %d)", version, os.Getpid())
	err = d.Run(ctx)
	st.Close()
	if errors.Is(err, daemon.ErrRestart) {
		logx.Printf("daemon: restarting on %s", exe)
		err = restartDaemon(exe, args)
		if err == nil {
			return nil // Windows: the new daemon is starting
		}
		logx.Printf("daemon: restart failed: %v", err)
		return err // non-zero exit: systemd and launchd start it again
	}
	logx.Printf("daemon stopped: %v", err)
	return err
}

// trimFile empties a file that grew past max (launchd's stderr capture only
// ever appends).
func trimFile(path string, max int64) {
	if st, err := os.Stat(path); err == nil && st.Size() > max {
		_ = os.Truncate(path, 0)
	}
}
