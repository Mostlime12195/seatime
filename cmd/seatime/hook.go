package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"seatime/internal/adapters"
	"seatime/internal/client/checks"
	"seatime/internal/client/config"
	"seatime/internal/client/daemon"
	"seatime/internal/client/emit"
	"seatime/internal/client/gitscan"
	"seatime/internal/client/idle"
	"seatime/internal/client/logx"
	"seatime/internal/client/presence"
	"seatime/internal/client/protected"
	"seatime/internal/client/spool"
	"seatime/internal/client/store"
	"seatime/pkg/event"
	"seatime/pkg/promptcheck"
)

// hookBudget is the most a hook may take, start to exit, whatever happens
// (a locked or corrupt database, a slow disk). Past it the hook spools what it
// has not stored yet and exits 0: the harness is never held up for longer.
func hookBudget() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("SEATIME_HOOK_BUDGET")); err == nil && d > 0 {
		return d
	}
	return 2 * time.Second
}

// memSink collects the adapter's events so the hook can answer the harness
// before it touches the database.
type memSink struct{ evs []event.Event }

func (m *memSink) Emit(e event.Event) error {
	if e.TS.IsZero() {
		e.TS = time.Now() // stamp now: a spooled event is stored later
	}
	m.evs = append(m.evs, e)
	return nil
}

// hookWriter stores a hook's events, spooling the rest if the store fails or
// the budget runs out. Exactly one of store or spool gets each event; an
// event caught mid-commit by the deadline may land in both, and the daemon's
// spool drain skips it (store.HasEvent).
type hookWriter struct {
	mu       sync.Mutex
	evs      []event.Event
	next     int
	finished bool
	spoolDir string
}

// spoolRest spools events not yet stored. Callers hold mu.
func (w *hookWriter) spoolRest(why string) {
	if w.finished {
		return
	}
	w.finished = true
	rest := w.evs[w.next:]
	if len(rest) == 0 {
		return
	}
	if err := spool.Write(w.spoolDir, rest); err != nil {
		logx.Printf("hook: %s; could not spool %d event(s), dropped: %v", why, len(rest), err)
		return
	}
	logx.Printf("hook: %s; spooled %d event(s) for the daemon", why, len(rest))
}

// run appends the events through the normal pipeline.
func (w *hookWriter) run(open func() (*store.Store, *emit.Pipeline, error)) {
	st, p, err := open()
	if err != nil {
		w.mu.Lock()
		w.spoolRest("database unavailable (" + err.Error() + ")")
		w.mu.Unlock()
		return
	}
	defer st.Close()
	scan := map[string]bool{} // projects whose agent turn or session ended
	defer func() { scanGuardedCommits(p, scan) }()
	for {
		w.mu.Lock()
		if w.finished || w.next >= len(w.evs) {
			w.finished = true
			w.mu.Unlock()
			return
		}
		e := w.evs[w.next]
		w.mu.Unlock()
		stored, err := p.EmitEvent(e)
		if err == nil && (stored.Kind == event.AgentStop || stored.Kind == event.SessionEnd) && stored.Project != "" {
			scan[stored.Project] = true
		}
		w.mu.Lock()
		if err != nil {
			w.spoolRest("store failed (" + err.Error() + ")")
			w.mu.Unlock()
			return
		}
		if !w.finished {
			w.next++
		}
		w.mu.Unlock()
	}
}

// scanGuardedCommits reads new commits for projects in folders the daemon
// may not open on this OS (macOS Desktop/Documents/..., package protected).
// The hook runs inside the user's terminal, which already has that access.
func scanGuardedCommits(p *emit.Pipeline, keys map[string]bool) {
	if len(keys) == 0 || !protected.ShouldEnable() {
		return
	}
	dirs := protected.Dirs(config.Home())
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	for k := range keys {
		pr, ok, err := p.Store.GetProject(k)
		if err != nil || !ok || !protected.Under(pr.Root, dirs) {
			continue
		}
		if n, err := gitscan.ScanRoot(ctx, p, k); err != nil {
			logx.Printf("hook: commit scan %s: %v", k, err)
		} else if n > 0 {
			logx.Printf("hook: %d new commit(s) in %s", n, k)
		}
	}
}

// runHook implements `seatime hook <adapter> <hookEvent>`. It must never fail,
// hang or print anything except what the adapter returns.
func runHook(args []string) {
	runHookWith(args, os.Stdin, os.Stdout, hookBudget(), openHookPipeline, func() { os.Exit(0) })
}

// openHookPipeline is openPipeline that also reads presence at a prompt and
// git's counts at an agent stop: the hook runs in the person's session at
// that moment, inside the project (the daemon never does either).
func openHookPipeline() (*store.Store, *emit.Pipeline, error) {
	st, p, err := openPipeline()
	if err != nil {
		return st, p, err
	}
	cfg := config.Load()
	front := config.FrontOn(cfg, st.PromptCheckOn())
	p.Presence = func() presence.Facts {
		probe := presence.System(front)
		if front {
			// the tool's own window, by process ids only: the daemon
			// checks every 5 s whether it is in front (front_slices)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			if chain := probe.Chain(ctx); len(chain) > 0 {
				_ = st.SetMeta(store.MetaFrontChain, store.JoinPIDs(chain))
			}
			cancel()
		}
		return emit.PresenceOf(context.Background(), probe)()
	}
	// how long the keyboard and mouse were in use before the prompt: the
	// daemon has counted, the hook has only this moment
	p.Input = func(since time.Time) (idle.Input, bool) { return daemon.InputSince(cfg, since) }
	p.Repo = emit.GitRepo(context.Background())
	p.Snap = func(root string) *promptcheck.Snapshot { return checks.Snapshot(context.Background(), root) }
	home := config.Home()
	p.Facts = func(root, name string) *promptcheck.Project {
		return checks.Facts(context.Background(), root, name, home)
	}
	return st, p, nil
}

func runHookWith(args []string, stdin *os.File, stdout io.Writer, budget time.Duration,
	open func() (*store.Store, *emit.Pipeline, error), exit func()) {
	start := time.Now()
	w := &hookWriter{spoolDir: config.SpoolDir()}
	done := make(chan struct{})
	defer close(done)
	go func() { // watchdog: whatever is stuck, give the harness its process back
		t := time.NewTimer(budget)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
			w.mu.Lock()
			w.spoolRest(fmt.Sprintf("over the %s budget", budget))
			w.mu.Unlock()
			exit()
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			logx.Printf("hook panic: %v args=%v", r, args)
			w.mu.Lock()
			w.spoolRest("panic")
			w.mu.Unlock()
		}
	}()
	if len(args) < 1 {
		logx.Printf("hook: missing adapter name")
		return
	}
	name := args[0]
	hookEvent := ""
	if len(args) > 1 {
		hookEvent = args[1]
	}
	var ad adapters.Adapter
	for _, a := range adapters.All() {
		if a.Name() == name {
			ad = a
		}
	}
	if ad == nil {
		logx.Printf("hook: unknown adapter %q", name)
		return
	}
	in := readStdin(stdin, budget/2)
	var sink memSink
	out, err := ad.HandleHook(hookEvent, in, &sink)
	if err != nil {
		logx.Printf("hook %s %s: %v", name, hookEvent, err)
	}
	if len(out) > 0 {
		stdout.Write(out)
	}
	w.mu.Lock()
	w.evs = sink.evs
	w.mu.Unlock()
	if len(w.evs) > 0 {
		w.run(open)
	}
	if d := time.Since(start); d > time.Second {
		logx.Printf("hook %s %s: slow (%s)", name, hookEvent, d.Round(time.Millisecond))
	}
}

// readStdin reads all of stdin but gives up after timeout (a harness that
// forgets to close the pipe must not hang us).
func readStdin(f *os.File, timeout time.Duration) []byte {
	if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return nil // interactive terminal, nothing piped
	}
	ch := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(f, 8<<20))
		ch <- b
	}()
	select {
	case b := <-ch:
		return b
	case <-time.After(timeout):
		logx.Printf("hook: stdin read timed out")
		return nil
	}
}

func cmdEmit(args []string) error {
	fs := newFlagSet("emit")
	harness := fs.String("harness", "", "default harness name for events without one")
	session := fs.String("session", "", "default session id")
	proj := fs.String("project", "", "default project key (otherwise attributed from cwd)")
	cwd := fs.String("cwd", "", "default cwd (defaults to the current directory)")
	fs.Parse(args)
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 16<<20))
	if err != nil {
		return err
	}
	d := *cwd
	if d == "" {
		d, _ = os.Getwd()
	}
	evs, err := emit.DecodeEvents(b, emit.Defaults{Harness: *harness, SessionID: *session, Project: *proj, CWD: d})
	if err != nil {
		return err
	}
	st, p, err := openPipeline()
	if err != nil {
		return err
	}
	defer st.Close()
	for _, e := range evs {
		if err := p.Emit(e); err != nil {
			return err
		}
	}
	fmt.Printf("emitted %d event(s)\n", len(evs))
	return nil
}
