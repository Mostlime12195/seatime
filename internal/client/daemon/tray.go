package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"seatime/internal/client/canary"
	"seatime/internal/client/config"
	"seatime/internal/client/logx"
	"seatime/internal/client/records"
	"seatime/internal/client/setup"
	"seatime/internal/client/stats"
	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/internal/client/tray"
	"seatime/internal/client/update"
)

// The menu bar icon (package tray) is a second process that only draws.
// It asks this daemon what to show and passes on what the person clicks;
// the daemon also keeps it installed and running (trayLoop).

// trayTotals are the account's numbers from the server, kept for a short
// while so that an open menu does not ask the server each time.
type trayTotals struct {
	mu       sync.Mutex
	val      *tray.Totals
	at       time.Time // when val was read
	asked    time.Time // when the server was last asked
	fetching bool
	device   string    // the device val belongs to
	none     time.Time // until when not to ask: the server has no such numbers
}

const (
	totalsActive = time.Minute      // how old the numbers may be while working
	totalsIdle   = 10 * time.Minute // and while nothing is going on
	totalsDrop   = 30 * time.Minute // older numbers are not shown at all
)

// TrayStatus is what the icon shows now.
func (d *Daemon) TrayStatus(ctx context.Context) tray.Status {
	cfg := config.Load()
	st := tray.Status{Version: d.Version, Off: !cfg.TrayEnabled(), Updates: d.Update != nil && update.IsRelease(d.Version)}
	now := time.Now()
	recent, _ := d.Store.Events(store.Query{Since: now.Add(-30 * time.Minute)})
	if c := stats.CountingNow(stats.CountingInput{Now: now, Recent: recent}); c.Yes && c.Key != "" {
		st.Now = stats.ProjectName(d.Store.Label(c.Key))
	}
	auth, signedIn := d.auth()
	if signedIn {
		st.Account = auth.Username
		st.Site = auth.APIBase(cfg) + "/@" + auth.Username
	}
	blocked, _ := d.Store.GetMeta(MetaSyncBlocked)
	switch {
	case !signedIn || blocked != "":
		st.Problem = tray.ProblemSignedOut
		var p pendingSignIn
		if d.Store.GetJSON(MetaSignIn, &p) == nil && p.URL != "" && now.Before(p.Expires) {
			st.SignIn = p.URL
		}
	case d.metaSet("last_sync_error"):
		st.Problem = tray.ProblemServer
	default:
		st.Problem, st.Tool = d.toolProblem(cfg)
	}
	if signedIn && st.Problem != tray.ProblemSignedOut {
		st.Today = d.totals(ctx, auth, cfg, st.Now != "")
	}
	return st
}

func (d *Daemon) metaSet(key string) bool {
	v, _ := d.Store.GetMeta(key)
	return v != ""
}

// toolProblem: an AI tool in use that SeaTime does not hear, or nothing
// connected at all (known once the first look for tools has run).
func (d *Daemon) toolProblem(cfg config.Config) (code, tool string) {
	if v, _ := d.Store.GetMeta(MetaSilent); v != "" {
		var ss []canary.Silent
		if json.Unmarshal([]byte(v), &ss) == nil && len(ss) > 0 {
			return tray.ProblemSilent, ss[0].Label
		}
	}
	d.hookMu.Lock()
	res := d.hookRes
	d.hookMu.Unlock()
	if res == nil {
		return "", ""
	}
	for _, r := range res {
		switch r.Status {
		case "installed", "repaired", "ok":
			return "", ""
		}
	}
	if setup.WakaProxyActive(cfg) {
		return "", ""
	}
	return tray.ProblemNoTools, ""
}

// totals returns the server's numbers, asking for new ones in the
// background once they are a minute old (ten while idle). The first time
// it waits a moment for the answer, so a menu opened right after login is
// not empty.
func (d *Daemon) totals(ctx context.Context, auth config.Auth, cfg config.Config, active bool) *tray.Totals {
	t := &d.trayTotals
	t.mu.Lock()
	if t.device != auth.DeviceID {
		t.val, t.at, t.asked, t.none, t.device = nil, time.Time{}, time.Time{}, time.Time{}, auth.DeviceID
	}
	maxAge := totalsIdle
	if active {
		maxAge = totalsActive
	}
	now := time.Now()
	var done chan struct{}
	if !t.fetching && now.After(t.none) && now.Sub(t.asked) >= maxAge {
		t.fetching, t.asked = true, now
		done = make(chan struct{})
		go d.fetchTotals(auth, cfg, done)
	}
	first := t.val == nil
	t.mu.Unlock()
	if done != nil && first {
		select {
		case <-done:
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.val == nil || time.Since(t.at) > totalsDrop {
		return nil
	}
	v := *t.val
	return &v
}

func (d *Daemon) fetchTotals(auth config.Auth, cfg config.Config, done chan struct{}) {
	defer close(done)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	td, err := syncer.NewClient(auth.APIBase(cfg), auth.Token, d.Version).Today(ctx)
	t := &d.trayTotals
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fetching = false
	if t.device != auth.DeviceID {
		return // signed in as someone else meanwhile
	}
	var he *syncer.HTTPError
	switch {
	case err == nil:
		t.val, t.at = &tray.Totals{Seconds: td.Seconds, UncheckedSeconds: td.UncheckedSeconds, UncheckedPrompts: td.UncheckedPrompts}, time.Now()
	case errors.As(err, &he) && he.Status == http.StatusNotFound:
		t.none = time.Now().Add(6 * time.Hour) // an older server: it has no such numbers
	}
}

// forgetTotals makes the next status ask the server again (after a send).
func (d *Daemon) forgetTotals() {
	d.trayTotals.mu.Lock()
	d.trayTotals.asked = time.Time{}
	d.trayTotals.mu.Unlock()
}

// trayRequest checks a request from the icon: the local secret, no browser.
func trayRequest(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		http.Error(w, method+" required", http.StatusMethodNotAllowed)
		return false
	}
	return authorize(w, r)
}

func trayAnswer(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func (d *Daemon) handleTray(w http.ResponseWriter, r *http.Request) {
	if trayRequest(w, r, http.MethodGet) {
		trayAnswer(w, d.TrayStatus(r.Context()))
	}
}

// handleTraySend is "Send now": what `seatime send` does.
func (d *Daemon) handleTraySend(w http.ResponseWriter, r *http.Request) {
	if !trayRequest(w, r, http.MethodPost) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	trayAnswer(w, tray.Reply{Result: d.SendNow(ctx)})
}

// SendNow syncs and sends this computer's records now, in turn with the
// sync loop. It returns tray.Sent, tray.SignedOut or tray.Failed.
func (d *Daemon) SendNow(ctx context.Context) string {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	s := &syncer.Syncer{Store: d.Store, Version: d.Version, Auth: d.Auth}
	_, err := s.SyncOnce(ctx)
	if err == nil {
		if _, err = d.RecordsOnce(ctx); errors.Is(err, records.ErrUnsupported) {
			err = nil // an older server: it gets each stretch of work later
		}
	}
	rejected, _ := authRejected(err)
	switch {
	case err == nil:
		d.forgetTotals()
		return tray.Sent
	case rejected, errors.Is(err, syncer.ErrNotLoggedIn):
		return tray.SignedOut
	}
	logx.Printf("send now: %v", err)
	return tray.Failed
}

// handleTrayUpdate is "Check for updates": what `seatime update` does.
func (d *Daemon) handleTrayUpdate(w http.ResponseWriter, r *http.Request) {
	if !trayRequest(w, r, http.MethodPost) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)
	defer cancel()
	rep := d.UpdateNow(ctx)
	trayAnswer(w, rep)
	if rep.Result == tray.Updated {
		d.requestRestart()
	}
}

// UpdateNow installs the newest release, whatever the daily check and the
// automatic-updates setting say. The caller restarts the daemon after
// tray.Updated.
func (d *Daemon) UpdateNow(ctx context.Context) tray.Reply {
	if d.Update == nil {
		return tray.Reply{Result: tray.Failed}
	}
	u := *d.Update
	u.BaseURL = config.Load().UpdateBase()
	res, err := u.Apply(ctx)
	switch {
	case err != nil:
		logx.Printf("update (asked from the menu bar icon): %v", err)
		return tray.Reply{Result: tray.Failed}
	case res.Updated:
		logx.Printf("update: installed %s (was %s), asked from the menu bar icon; restarting on it", res.Latest, res.Current)
		return tray.Reply{Result: tray.Updated, Version: res.Latest}
	case res.RolledBack, res.Skipped:
		return tray.Reply{Result: tray.Stayed}
	}
	return tray.Reply{Result: tray.Current}
}

// trayLoop keeps the icon installed and running: shortly after the daemon
// starts (which is how a computer that just updated gets its icon) and
// every ten minutes after. An icon the person hid stays away until the
// daemon or the icon next starts, which is at the next login.
func (d *Daemon) trayLoop(ctx context.Context) {
	if d.TrayExe == "" || !tray.Supported {
		return
	}
	tray.Unhide()
	sleep(ctx, 20*time.Second)
	for ctx.Err() == nil {
		d.TrayOnce()
		sleep(ctx, 10*time.Minute)
	}
}

// TrayOnce is one pass of trayLoop.
func (d *Daemon) TrayOnce() {
	defer func() {
		if r := recover(); r != nil {
			logx.Printf("menu bar icon: panic: %v", r)
		}
	}()
	var out strings.Builder
	o := setup.Options{Out: &out}
	cfg := config.Load()
	// a daemon that systemd runs is outside the desktop session, and what
	// it starts shares its memory limit: there the session starts the icon
	spawn := cfg.Service.Method != "systemd"
	if !cfg.TrayEnabled() {
		setup.RemoveTray(o)
	} else if _, err := setup.EnsureTray(o, d.TrayExe, spawn); err != nil {
		logx.Printf("menu bar icon: %v", err)
	}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l != "" {
			logx.Printf("menu bar icon: %s", strings.TrimPrefix(l, "tray: "))
		}
	}
}
