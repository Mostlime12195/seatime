package daemon

import (
	"context"
	"net/http"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/logx"
	"seatime/internal/client/update"
)

// updateLoop installs a new signed release about once a day (package update
// decides; this only asks every hour) and then restarts the daemon on it.
func (d *Daemon) updateLoop(ctx context.Context) {
	if d.Update == nil {
		return
	}
	first := d.UpdateFirst
	if first <= 0 {
		first = 2 * time.Minute
	}
	sleep(ctx, first)
	for ctx.Err() == nil {
		if res, _ := d.UpdateOnce(ctx); res.Updated {
			return
		}
		sleepWall(ctx, time.Hour, 30*time.Second)
	}
}

// UpdateOnce runs one automatic check with the current config. After an
// update it asks Run to restart.
func (d *Daemon) UpdateOnce(ctx context.Context) (update.Result, error) {
	cfg := config.Load()
	u := *d.Update
	u.BaseURL = cfg.UpdateBase()
	res, err := u.Auto(ctx, cfg.AutoUpdateEnabled())
	switch {
	case err != nil:
		logx.Printf("update: %v", err)
	case res.Updated:
		logx.Printf("update: installed %s (was %s); restarting on it", res.Latest, res.Current)
		d.requestRestart()
	case res.RolledBack:
		logx.Printf("update: %s failed its self-test (%s); went back to %s", res.Latest, res.Detail, res.Current)
	}
	return res, err
}

// handleRestart lets `seatime update` restart the daemon on the binary it
// just installed. It needs the local secret and refuses browser origins.
func (d *Daemon) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if !authorize(w, r) {
		return
	}
	logx.Printf("daemon: restart requested by `seatime update`")
	w.WriteHeader(http.StatusAccepted)
	d.requestRestart()
}
