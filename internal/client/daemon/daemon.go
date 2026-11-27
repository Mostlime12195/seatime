// Package daemon is `seatime daemon`: sync loop, adapter watchers, git commit
// scanning and the local HTTP endpoint (WakaTime-compatible + plugin emit).
// Everything is timer driven; idle CPU is ~0.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"seatime/internal/client/presence"
	"seatime/pkg/ledger"
	"sort"
	"sync"
	"time"

	"seatime/internal/adapters"
	"seatime/internal/client/canary"
	"seatime/internal/client/checks"
	"seatime/internal/client/config"
	"seatime/internal/client/emit"
	"seatime/internal/client/gitscan"
	"seatime/internal/client/idle"
	"seatime/internal/client/logx"
	"seatime/internal/client/records"
	"seatime/internal/client/setup"
	"seatime/internal/client/spool"
	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/internal/client/update"
	"seatime/internal/client/wakaproxy"
	"seatime/pkg/event"
)

type Daemon struct {
	Version  string
	Store    *store.Store
	Pipeline *emit.Pipeline
	Adapters []adapters.Adapter
	Started  time.Time

	// HookInterval is how often harness detection re-runs (default 10 min; it
	// also runs once shortly after startup).
	HookInterval time.Duration
	// HookBin is the binary hooks call (default setup.HookBinary()).
	HookBin string
	// Service: started by launchd/systemd. If another daemon already runs,
	// wait for it to go away and take over, instead of exiting (which the
	// service manager would answer with a respawn every few seconds).
	Service bool
	// Idle samples the OS idle timer once a minute (nil = idle.Default()).
	Idle *idle.Recorder
	// Exit replaces os.Exit (tests).
	Exit func(code int)
	// Probes overrides the canary's harness list (tests).
	Probes []canary.Probe
	// RetryListen is how often a waiting service daemon retries (default 30 s).
	RetryListen time.Duration
	// Update installs new releases (nil = never). Its BaseURL is refreshed
	// from config before each check.
	Update *update.Updater
	// UpdateFirst is the wait before the first update check (default 2 min).
	UpdateFirst time.Duration

	// Auth overrides the stored credentials for the check-in (tests).
	Auth *config.Auth
	// CheckInEvery is how often the daemon checks in (default 1 min).
	CheckInEvery time.Duration
	// Uninstall replaces removing SeaTime from this computer (tests).
	Uninstall func(purge bool)
	// OpenBrowser, when set, lets the daemon sign this computer in again
	// by itself when the server stops accepting it: it opens the sign-in
	// page, at most once a day (signin.go). nil: no automatic sign-in.
	OpenBrowser func(url string) bool

	restart chan struct{}
	hookMu  sync.Mutex
	hookRes []setup.HookResult
}

// ErrRestart is what Run returns when the daemon should start again on the
// binary at its path: after an update, or when `seatime update` asks.
var ErrRestart = errors.New("daemon: restart requested")

// requestRestart makes Run return ErrRestart.
func (d *Daemon) requestRestart() {
	select {
	case d.restart <- struct{}{}:
	default:
	}
}

// Ping asks a daemon on cfg's port whether it is alive.
func Ping(cfg config.Config) (map[string]any, bool) {
	c := http.Client{Timeout: 700 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/seatime/ping", cfg.WakaPortOrDefault()))
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var m map[string]any
	if json.NewDecoder(resp.Body).Decode(&m) != nil || m["name"] != "seatime" {
		return nil, false
	}
	return m, true
}

// Handler is the local HTTP surface (WakaTime endpoint, /seatime/emit),
// exported for the simulator.
func (d *Daemon) Handler(cfg config.Config) http.Handler { return d.handler(cfg) }

func (d *Daemon) handler(cfg config.Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/seatime/input", d.handleInput)
	mux.HandleFunc("/seatime/ping", func(w http.ResponseWriter, r *http.Request) {
		var ms debugStats
		ms.read()
		w.Header().Set("Content-Type", "application/json")
		m := map[string]any{"name": "seatime", "version": d.Version, "pid": os.Getpid(),
			"uptime_s": int(time.Since(d.Started).Seconds()), "heap_bytes": ms.heap}
		if d.Idle != nil {
			m["idle"] = d.Idle.Last()
		}
		_ = json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("/seatime/emit", d.handleEmit)
	mux.HandleFunc("/seatime/restart", d.handleRestart)
	waka := wakaproxy.New(d.Pipeline, cfg.ForwardURL)
	waka.ForwardFn = newForwardWatcher(cfg.ForwardURL).get
	mux.Handle("/", waka)
	return guard(mux)
}

// Run blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	debug.SetGCPercent(40)
	debug.SetMemoryLimit(48 << 20)
	d.Started = time.Now()
	d.restart = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cfg := config.Load()

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.WakaPortOrDefault())
	ln, err := net.Listen("tcp", addr)
	if err != nil && d.Service {
		ln, err = d.waitForPeer(ctx, cfg, addr, err)
		if ctx.Err() != nil {
			return nil
		}
	}
	if err != nil {
		if _, alive := Ping(cfg); alive {
			return errors.New("seatime daemon is already running")
		}
		logx.Printf("daemon: cannot listen on %s: %v (continuing without local endpoint)", addr, err)
		ln = nil
	}
	if ln != nil {
		srv := &http.Server{Handler: d.handler(cfg), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()
		logx.Printf("daemon: listening on %s (forward=%q)", addr, cfg.ForwardURL)
	}

	if d.Idle == nil {
		d.Idle = &idle.Recorder{Src: idle.Default()}
	}
	if d.Idle.Emit == nil {
		d.Idle.Emit = d.Pipeline.Emit
	}
	if d.Idle.Presence == nil {
		supported := presence.FrontSupported(os.Getenv)
		d.Idle.Presence = func(ok bool) string {
			return presence.Level(ok, false, config.FrontOn(cfg, d.Store.PromptCheckOn()), supported)
		}
	}
	if d.Idle.Front == nil {
		d.Idle.Front = d.frontNow(cfg, presence.SystemWindows())
	}
	if d.Idle.Active == nil {
		d.Idle.Active = d.projectActive
	}
	idleDone := make(chan struct{})
	go func() { defer close(idleDone); d.Idle.Run(ctx, time.Minute) }()
	defer func() { <-idleDone }() // the open window is written before the store closes

	for _, a := range d.Adapters {
		if w, ok := a.(adapters.Watcher); ok {
			go d.watch(ctx, a.Name(), w)
		}
	}
	go d.gitLoop(ctx)
	go d.spoolLoop(ctx)
	go d.heldLoop(ctx)
	go d.canaryLoop(ctx)
	go d.healthLoop(ctx)
	go d.hookLoop(ctx)
	go d.syncLoop(ctx)
	go d.checkInLoop(ctx)
	go d.segmentLoop(ctx)
	go d.checkLoop(ctx)
	go d.updateLoop(ctx)
	go func() { // hand freed memory back to the OS
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				debug.FreeOSMemory()
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil
	case <-d.restart:
		cancel()
		return ErrRestart
	}
}

// waitForPeer waits while another seatime daemon answers on addr, retrying the
// listen; it returns once the port is ours or nobody we know holds it.
func (d *Daemon) waitForPeer(ctx context.Context, cfg config.Config, addr string, err error) (net.Listener, error) {
	every := d.RetryListen
	if every <= 0 {
		every = 30 * time.Second
	}
	logged := false
	for ctx.Err() == nil {
		if _, alive := Ping(cfg); !alive {
			return net.Listen("tcp", addr)
		}
		if !logged {
			logx.Printf("daemon: another seatime daemon is running; waiting to take over")
			logged = true
		}
		sleep(ctx, every)
		var ln net.Listener
		if ln, err = net.Listen("tcp", addr); err == nil {
			return ln, nil
		}
	}
	return nil, err
}

func (d *Daemon) watch(ctx context.Context, name string, w adapters.Watcher) {
	dir := config.StateDir(name)
	_ = os.MkdirAll(dir, 0o700)
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := safeWatch(ctx, w, dir, &gatedSink{name: name, inner: d.Pipeline})
		if ctx.Err() != nil {
			return
		}
		logx.Printf("watcher %s stopped: %v", name, err)
		if time.Since(start) > 10*time.Minute {
			backoff = 5 * time.Second
		}
		sleep(ctx, backoff)
		if backoff < 10*time.Minute {
			backoff *= 2
		}
	}
}

func safeWatch(ctx context.Context, w adapters.Watcher, dir string, sink adapters.Sink) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return w.Watch(ctx, dir, sink)
}

func (d *Daemon) gitLoop(ctx context.Context) {
	sleep(ctx, 5*time.Second)
	for ctx.Err() == nil {
		if n, err := gitscan.Scan(ctx, d.Pipeline, 3*24*time.Hour); err != nil {
			logx.Printf("gitscan: %v", err)
		} else if n > 0 {
			logx.Printf("gitscan: %d new commits", n)
		}
		sleep(ctx, 90*time.Second)
	}
}

// healthLoop checks the local log every 10 minutes (a full quick check every
// 6 hours). If it has become damaged while running, the daemon exits with
// a non-zero status: launchd/systemd start it again, and startup moves the
// damaged file aside and continues the chain (store.OpenChecked).
func (d *Daemon) healthLoop(ctx context.Context) {
	n := 0
	for ctx.Err() == nil {
		sleep(ctx, 10*time.Minute)
		if ctx.Err() != nil {
			return
		}
		_, _, err := d.Store.Head()
		if err == nil && n%36 == 35 {
			err = d.Store.Check()
		}
		n++
		if store.IsCorrupt(err) {
			logx.Printf("daemon: the local log is damaged (%v); exiting so the restart can recover it", err)
			if d.Exit != nil {
				d.Exit(3)
				return
			}
			os.Exit(3)
		}
	}
}

// canaryLoop watches for harnesses in use whose hooks are silent.
func (d *Daemon) canaryLoop(ctx context.Context) {
	sleep(ctx, time.Minute)
	for ctx.Err() == nil {
		if _, err := d.CanaryOnce(time.Now()); err != nil {
			logx.Printf("canary: %v", err)
		}
		sleep(ctx, 2*time.Minute)
	}
}

// CanaryOnce checks every probe once: it records silent harnesses for
// `status`, asks auto-hook to repair, and backfills from session files
// (events labelled "backfilled"). Returns the number of events backfilled.
func (d *Daemon) CanaryOnce(now time.Time) (int, error) {
	probes := d.Probes
	if probes == nil {
		probes = canary.Probes()
	}
	silent, err := canary.Check(d.Store, config.Home(), now, probes)
	if err != nil {
		return 0, err
	}
	prev, _ := d.Store.GetMeta(MetaSilent)
	b, _ := json.Marshal(silent)
	if len(silent) == 0 {
		b = nil
	}
	if string(b) != prev {
		_ = d.Store.SetMeta(MetaSilent, string(b))
		for _, s := range silent {
			logx.Printf("canary: %s is in use but its hooks are silent (last hook %s); repairing and backfilling", s.Label, s.LastHook.Format(time.RFC3339))
		}
	}
	if len(silent) == 0 {
		return 0, nil
	}
	if config.Load().AutoHookEnabled() {
		d.AutoHookOnce()
	}
	n := 0
	for _, s := range silent {
		var p canary.Probe
		for _, q := range probes {
			if q.Harness == s.Harness {
				p = q
			}
		}
		if p.Backfill == nil {
			continue
		}
		wmKey := "backfill_wm:" + s.Harness
		from := now.Add(-24 * time.Hour)
		if s.LastHook.After(from) {
			from = s.LastHook
		}
		if v, _ := d.Store.GetMeta(wmKey); v != "" {
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil && t.After(from) {
				from = t
			}
		}
		to := now.Add(-30 * time.Second) // leave a turn that is being written alone
		var evs []event.Event
		for _, f := range s.Files {
			evs = append(evs, p.Backfill(f, from, to)...)
		}
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].TS.Before(evs[j].TS) })
		for _, e := range evs {
			if err := d.Pipeline.Emit(e); err != nil {
				return n, err
			}
			n++
		}
		_ = d.Store.SetMeta(wmKey, to.Format(time.RFC3339Nano))
	}
	if n > 0 {
		logx.Printf("canary: backfilled %d event(s) from session files", n)
	}
	return n, nil
}

// MetaSilent holds the JSON list of silent harnesses (canary.Silent).
const MetaSilent = "silent_harnesses"

// spoolLoop stores events that hooks spooled because the database was busy,
// broken or slow (see package spool).
func (d *Daemon) spoolLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if n, err := d.DrainSpool(config.SpoolDir()); err != nil {
			logx.Printf("spool: stored %d, then: %v", n, err)
		} else if n > 0 {
			logx.Printf("spool: stored %d event(s) hooks had queued", n)
		}
		sleep(ctx, 15*time.Second)
	}
}

// heldLoop records the prompts hooks kept back until the AI tool's log
// says who sent them (emit hold.go). A hook does it too, at the session's
// next record; this is for the moments between.
func (d *Daemon) heldLoop(ctx context.Context) {
	for ctx.Err() == nil {
		sleep(ctx, heldEvery)
		if ctx.Err() == nil {
			d.Pipeline.ReleaseHeld(time.Now())
		}
	}
}

// heldEvery is how often the daemon looks at held prompts.
const heldEvery = 2 * time.Second

// DrainSpool feeds spooled hook events through the pipeline, skipping any
// that a timed-out hook did manage to store.
func (d *Daemon) DrainSpool(dir string) (int, error) {
	return spool.Drain(dir, func(e event.Event) error {
		if dup, err := d.Store.HasEvent(e); err != nil {
			return err
		} else if dup {
			return nil
		}
		return d.Pipeline.Emit(e)
	})
}

func (d *Daemon) syncLoop(ctx context.Context) {
	s := &syncer.Syncer{Store: d.Store, Version: d.Version}
	fails := 0
	sleep(ctx, 10*time.Second)
	for ctx.Err() == nil {
		_, err := s.SyncOnce(ctx)
		if rejected, revoked := authRejected(err); rejected && d.OpenBrowser != nil {
			// sign in again by itself instead of asking for `seatime login`
			wait, done := d.signInStep(ctx, revoked, time.Now())
			if done {
				fails = 0
				continue
			}
			sleepWall(ctx, wait, 30*time.Second)
			continue
		}
		var wait time.Duration
		wait, fails = syncWait(err, fails, rand.Int63n)
		if err != nil && !errors.Is(err, syncer.ErrNotLoggedIn) {
			logx.Printf("sync failed (%d): %v; retry in %s", fails, err, wait.Round(time.Second))
			_ = d.Store.SetMeta("last_sync_error", err.Error())
		}
		_ = d.Store.SetMeta(MetaSyncBlocked, blockedMessage(err))
		if err == nil {
			// the records behind the checkpoint that sync carried
			if _, rerr := d.RecordsOnce(ctx); rerr != nil && !errors.Is(rerr, syncer.ErrNotLoggedIn) && !errors.Is(rerr, records.ErrUnsupported) {
				logx.Printf("records: %v", rerr)
			}
		}
		if woke := sleepWall(ctx, wait, 30*time.Second); woke {
			fails = 0 // back from sleep or a clock change: try now, not after the old backoff
			logx.Printf("sync: the clock jumped (sleep/wake or a time change); syncing now")
		}
	}
}

// MetaSyncBlocked holds a plain sentence when syncing cannot work until the
// user acts (login expired, device removed, history mismatch); "" otherwise.
const MetaSyncBlocked = "sync_blocked"

// syncWait is the pause before the next sync: 90 s normally, exponential
// backoff with jitter capped at 10 min on errors, an hour when only the user
// can fix it.
func syncWait(err error, fails int, jitter func(int64) int64) (time.Duration, int) {
	const normal, maxWait = 90 * time.Second, 10 * time.Minute
	switch {
	case err == nil:
		return normal, 0
	case errors.Is(err, syncer.ErrNotLoggedIn):
		return 5 * time.Minute, 0
	case blockedMessage(err) != "":
		return time.Hour, fails + 1
	}
	fails++
	wait := normal << min(fails, 6)
	if wait > maxWait {
		wait = maxWait
	}
	return wait - wait/5 + time.Duration(jitter(int64(wait/5)+1)), fails
}

func blockedMessage(err error) string {
	var he *syncer.HTTPError
	if !errors.As(err, &he) {
		return ""
	}
	switch he.Status {
	case 401, 403:
		return "the server no longer accepts this computer (it was signed out or disconnected). Sign it in again from the Settings page on the web, or run `seatime login`. Your time is still recorded on this machine."
	case 409:
		return "the server has a different history for this device than this machine does (the local log was replaced by an older SeaTime). Your time is still recorded here; run `seatime login` to sync from this machine again."
	}
	return ""
}

// sleepWall waits d by the wall clock, checking every tick, and returns
// early with woke=true when wall time jumped ahead of the process clock (the
// machine slept, or the clock was changed). Go's timers use a clock that
// stops during sleep, so a plain timer would add the sleep to the wait.
func sleepWall(ctx context.Context, d, tick time.Duration) (woke bool) {
	startWall, startMono := time.Now().Round(0), time.Now()
	deadline := startWall.Add(d)
	for {
		step := min(tick, time.Until(deadline))
		if step <= 0 {
			return false
		}
		sleep(ctx, step)
		if ctx.Err() != nil {
			return false
		}
		wall, mono := time.Now().Round(0).Sub(startWall), time.Since(startMono)
		if wall-mono > time.Minute || wall < mono-time.Minute {
			return true
		}
		if !time.Now().Round(0).Before(deadline) {
			return false
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// projectActive reports whether a hand edit or a prompt arrived in the last
// 10 minutes (ledger.Window): the idle recorder then samples every 5 s.
func (d *Daemon) projectActive() bool {
	evs, err := d.Store.Events(store.Query{Kinds: []event.Kind{event.Edit, event.Prompt}, Desc: true, Limit: 1})
	return err == nil && len(evs) == 1 && time.Since(evs[0].TS) <= ledger.Window
}

// frontNow answers, for the idle recorder's 5 s slices, whether the AI
// tool's own window is in front: the front window's process is in the
// chain the hook saved at the last prompt. ok is false when the check is
// off, the OS cannot tell, or no prompt has named the tool's window yet.
func (d *Daemon) frontNow(cfg config.Config, w presence.Windows) func(ctx context.Context) (bool, bool) {
	return func(ctx context.Context) (bool, bool) {
		if w == nil || !config.FrontOn(cfg, d.Store.PromptCheckOn()) {
			return false, false
		}
		v, _ := d.Store.GetMeta(store.MetaFrontChain)
		chain := store.SplitPIDs(v)
		if len(chain) == 0 {
			return false, false
		}
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		pid, err := w.FrontPID(ctx)
		if err != nil || pid <= 0 {
			return false, false
		}
		return presence.InChain(pid, chain), true
	}
}

// checkLoop sends each new prompt's hash to the server, and each finished
// turn for the prompt check as soon as it is queued (the hooks queue it
// when the AI stops, the next prompt arrives or the session ends), polling
// every few seconds. A turn that cannot be sent waits its own backoff
// (checks.Backoff); while the account has the check off, only the turns
// the server asked for are sent.
func (d *Daemon) checkLoop(ctx context.Context) {
	s := &syncer.Syncer{Store: d.Store, Version: d.Version, Live: d.Idle.Live}
	for ctx.Err() == nil {
		sleep(ctx, checkEvery)
		if _, err := s.PushHashes(ctx); err != nil && !errors.Is(err, syncer.ErrNotLoggedIn) {
			logx.Printf("prompt hashes: %v", err)
		}
		want := checks.Wanted(d.Store)
		if want == nil || d.Store.ChecksPaused(time.Now()) {
			continue
		}
		if due, err := d.Store.DueChecks(time.Now(), 1, want); err != nil || len(due) == 0 {
			continue
		}
		if n, err := s.CheckOnce(ctx); err != nil && !errors.Is(err, syncer.ErrNotLoggedIn) {
			logx.Printf("prompt check: %d sent; the rest wait: %v", n, err)
		}
	}
}

// checkEvery is how often the daemon looks for finished turns to send.
const checkEvery = 5 * time.Second
