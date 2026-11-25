package checks_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"seatime/internal/client/checks"
	"seatime/internal/client/emit"
	"seatime/internal/client/stats"
	"seatime/internal/client/store"
	"seatime/pkg/api"
	"seatime/pkg/event"
	"seatime/pkg/promptcheck"
)

// chainFixture is a device talking to a server that takes prompt hashes,
// with the server's check on and the account's setting given by user.
func chainFixture(t *testing.T, user []promptcheck.Period) *fixture {
	t.Helper()
	f := newFixture(t)
	f.st.SetMeta(store.MetaChain, "1")
	f.st.SetMeta(store.MetaServerCheck, "1")
	f.st.PutJSON(store.MetaServerPeriods, []promptcheck.Period{{From: time.UnixMilli(0).UTC(), On: true}})
	setUser(t, f, user)
	return f
}

// setUser stores the account's setting history as a sync answer would,
// with PromptCheck as the server sends it (its switch and the setting now).
func setUser(t *testing.T, f *fixture, user []promptcheck.Period) {
	t.Helper()
	f.st.PutJSON(store.MetaUserPeriods, user)
	on := "0"
	if promptcheck.UserOnAt(user, time.Now()) {
		on = "1"
	}
	f.st.SetMeta(store.MetaPromptCheck, on)
}

// turn is a prompt at at and the AI stopping 5 minutes later.
func (f *fixture) turn(t *testing.T, at time.Duration, text string) event.Event {
	t.Helper()
	p := f.emit(t, at, event.Event{Kind: event.Prompt, Text: text})
	f.emit(t, at+5*time.Minute, event.Event{Kind: event.AgentStop})
	return p
}

func credit(t *testing.T, f *fixture) float64 {
	t.Helper()
	res, _, err := stats.Credit(f.st, f.t0.Add(-time.Hour), f.t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return res.Total
}

// Off, then on halfway through a project: the prompts from while it was
// off were kept, wait while it is off, are all sent once it is on, and the
// stretch counts as if the check were off until every one of them has a
// verdict, then by the check.
func TestOffThenOnHalfway(t *testing.T) {
	f := chainFixture(t, nil)
	off := []promptcheck.Period{{From: f.t0.Add(-time.Minute), On: false}}
	setUser(t, f, off)
	a := f.turn(t, 0, "PROMPT-CANARY make the zorblax counter purple")
	b := f.turn(t, 20*time.Minute, "PROMPT-CANARY and also please make it bounce")
	if a.CheckHash == "" || b.CheckHash == "" {
		t.Fatal("a prompt was not hashed while the check was off")
	}
	srv := &fakeServer{pass: map[string]bool{"PROMPT-CANARY make the zorblax counter purple": true}}
	if n, _ := checks.Drain(context.Background(), f.st, "dev1", srv.send, f.t0.Add(time.Hour), 20); n != 0 || len(srv.reqs) != 0 {
		t.Fatal("sent while the account had it off")
	}
	plain := credit(t, f)
	if plain < 9*60 {
		t.Fatalf("while off, the two prompts count as definition 2 says: %v s", plain)
	}
	if es, _ := f.st.UnsentChain(10); len(es) != 2 || promptcheck.VerifyChain(0, promptcheck.ChainGenesis, es) != nil {
		t.Fatalf("chain %+v", es)
	}

	// turned back on: one verdict back is not enough to switch the stretch
	setUser(t, f, append(off, promptcheck.Period{From: f.t0.Add(time.Hour), On: true}))
	if n, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, f.t0.Add(90*time.Minute), 1); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if got := credit(t, f); got != plain {
		t.Fatalf("half checked: %v s, want the plain %v s", got, plain)
	}
	if n, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, f.t0.Add(90*time.Minute), 20); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	// all checked: the stretch counts by the check, and the server counts
	// its prompts, not the device
	if got := credit(t, f); got != 0 {
		t.Fatalf("checked: %v s on the device, plain was %v s", got, plain)
	}
	if set := stats.PromptChecks(f.st); set == nil || !set.OnAt(f.t0.Add(time.Minute)) || !set.OnAt(f.t0.Add(30*time.Minute)) {
		t.Fatalf("the stretch did not switch: %+v", set)
	}
}

// On, off, on: each stretch counts by its own setting, the off one by the
// check once its prompts are checked; nothing is lost.
func TestOnOffOn(t *testing.T) {
	f := chainFixture(t, nil)
	user := []promptcheck.Period{{From: f.t0.Add(15 * time.Minute), On: false}, {From: f.t0.Add(45 * time.Minute), On: true}}
	setUser(t, f, user)
	f.turn(t, 0, "PROMPT-CANARY one: the zorblax counter")
	f.turn(t, 20*time.Minute, "PROMPT-CANARY two: while it was off")
	f.turn(t, 50*time.Minute, "PROMPT-CANARY three: back on again")
	srv := &fakeServer{pass: map[string]bool{"PROMPT-CANARY one: the zorblax counter": true, "PROMPT-CANARY three: back on again": true}}
	set := stats.PromptChecks(f.st)
	if set.OnAt(f.t0.Add(20*time.Minute)) || !set.OnAt(f.t0) || !set.OnAt(f.t0.Add(50*time.Minute)) {
		t.Fatalf("periods before the checks: %+v", set.Periods)
	}
	if n, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, f.t0.Add(90*time.Minute), 20); n != 3 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	set = stats.PromptChecks(f.st)
	if !set.OnAt(f.t0.Add(20 * time.Minute)) {
		t.Fatalf("the off stretch did not switch once checked: %+v", set.Periods)
	}
	if pass, _ := f.st.CheckPasses(); len(pass) != 3 {
		t.Fatalf("verdicts %v", pass)
	}
}

// A device that restarts (its store closed and opened again) continues its
// chain; nothing waiting is lost.
func TestChainAcrossRestart(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "c.db")
	proj := filepath.Join(home, "zorblax")
	os.MkdirAll(proj, 0o755)
	t0 := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	emitOne := func(st *store.Store, at time.Duration, text string) {
		p := emit.New(st, home)
		p.Snap = func(string) *promptcheck.Snapshot { return &promptcheck.Snapshot{} }
		if _, err := p.EmitEvent(event.Event{Kind: event.Prompt, Text: text, TS: t0.Add(at), Harness: "claude-code", CWD: proj, SessionID: "s1"}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	emitOne(st, 0, "PROMPT-CANARY first before the restart")
	emitOne(st, time.Minute, "PROMPT-CANARY second before the restart")
	es, _ := st.UnsentChain(10)
	st.MarkChainSent(es[0].Seq) // the server took the first
	st.Close()

	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	emitOne(st, 2*time.Minute, "PROMPT-CANARY third after the restart")
	emitOne(st, 3*time.Minute, "PROMPT-CANARY first before the restart") // a repeat: chained, never judged
	unsent, _ := st.UnsentChain(10)
	if len(unsent) != 3 || unsent[0].Seq != 2 || !unsent[2].Repeat {
		t.Fatalf("unsent after the restart: %+v", unsent)
	}
	if err := promptcheck.VerifyChain(es[0].Seq, es[0].Link, unsent); err != nil {
		t.Fatalf("the chain broke at the restart: %v", err)
	}
	// each turn ended at the next prompt and waits; the repeat's turn is
	// open, and will be context only
	if q, _ := st.QueuedChecks(); q != 3 {
		t.Fatalf("queued %d", q)
	}
}

// While the account has it off, only the prompts a pull asks for are sent.
func TestPullSendsOnlyWhatWasAsked(t *testing.T) {
	f := chainFixture(t, []promptcheck.Period{{From: f0(), On: false}})
	in := f.turn(t, 0, "PROMPT-CANARY a prompt in the shipped project")
	other := filepath.Join(filepath.Dir(f.proj), "other")
	os.MkdirAll(other, 0o755)
	keep := f.proj
	f.proj = other
	// a session of its own: a session stays with the project it started in
	out := f.emit(t, 10*time.Minute, event.Event{Kind: event.Prompt, SessionID: "s2", Text: "PROMPT-CANARY a prompt in another project"})
	f.emit(t, 15*time.Minute, event.Event{Kind: event.AgentStop, SessionID: "s2"})
	f.proj = keep
	if in.Project == out.Project {
		t.Fatal("one project")
	}
	srv := &fakeServer{pass: map[string]bool{}}
	f.st.PutJSON(store.MetaPulls, []store.Pull{{Project: event.ServerKey(in.Project), From: f.t0.Add(-time.Hour), To: f.t0.Add(time.Hour)}})
	if n, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, f.t0.Add(time.Hour), 20); n != 1 || err != nil || srv.reqs[0].CheckHash != in.CheckHash {
		t.Fatalf("pulled %d %v", n, err)
	}
	if q, _ := f.st.QueuedChecks(); q != 1 {
		t.Fatalf("the other project's prompt left the queue: %d", q)
	}
}

func f0() time.Time { return time.Now().Add(-24 * time.Hour).UTC() }

// raErr is a server answer that asks to wait.
type raErr struct{ wait time.Duration }

func (e raErr) Error() string             { return "server returned 429" }
func (e raErr) RetryAfter() time.Duration { return e.wait }

// A backlog of many prompts (the check off for weeks, then on) goes out in
// batches at the server's pace: each 429 holds every check for the time the
// server asked, without counting against any prompt; all end with a
// verdict, none is lost.
func TestBacklogAtServerPace(t *testing.T) {
	f := chainFixture(t, []promptcheck.Period{{From: f0(), On: false}})
	const n = 150
	for i := 0; i < n; i++ {
		f.emit(t, time.Duration(i)*time.Second, event.Event{Kind: event.Prompt, Text: fmt.Sprintf("PROMPT-CANARY backlog prompt number %d", i)})
	}
	f.emit(t, n*time.Second, event.Event{Kind: event.AgentStop})
	setUser(t, f, []promptcheck.Period{{From: f0(), On: false}, {From: f.t0.Add(time.Hour), On: true}})

	// the server takes 10 a minute; past that it answers 429, wait 60 s
	now := f.t0.Add(2 * time.Hour)
	window, inWindow, sent, limited := now, 0, 0, 0
	send := func(_ context.Context, req api.PromptCheckReq) (api.PromptCheckResp, int, error) {
		if now.Sub(window) >= time.Minute {
			window, inWindow = now, 0
		}
		if inWindow >= 10 {
			limited++
			return api.PromptCheckResp{}, 429, raErr{time.Minute}
		}
		inWindow++
		sent++
		return api.PromptCheckResp{CheckHash: req.CheckHash}, 200, nil
	}
	for round := 0; round < 10000; round++ {
		if q, _ := f.st.QueuedChecks(); q == 0 {
			break
		}
		checks.Drain(context.Background(), f.st, "dev1", send, now, 20)
		now = now.Add(5 * time.Second) // the daemon's pace
	}
	if q, _ := f.st.QueuedChecks(); q != 0 || sent != n {
		t.Fatalf("queued %d, sent %d", q, sent)
	}
	if limited == 0 || limited > n/10+2 {
		t.Fatalf("429s: %d (each pause should hold every check)", limited)
	}
	if pass, _ := f.st.CheckPasses(); len(pass) != n {
		t.Fatalf("verdicts %d", len(pass))
	}
	if minutes := now.Sub(f.t0.Add(2 * time.Hour)).Minutes(); minutes > 20 {
		t.Fatalf("took %.0f minutes for %d prompts at 10 a minute", minutes, n)
	}
}

// A model failure or timeout (no answer, or 503) never loses or decides a
// prompt: it is sent again later and gets its verdict then.
func TestModelFailureRetriesLater(t *testing.T) {
	f := chainFixture(t, nil)
	p := f.turn(t, 0, "PROMPT-CANARY make the zorblax counter purple")
	now := f.t0.Add(10 * time.Minute)
	fails := []error{context.DeadlineExceeded, raErr{5 * time.Minute}, errors.New("connection reset")}
	codes := []int{0, 503, 0}
	for i := range fails {
		err := fails[i]
		code := codes[i]
		send := func(context.Context, api.PromptCheckReq) (api.PromptCheckResp, int, error) {
			return api.PromptCheckResp{}, code, err
		}
		if n, _ := checks.Drain(context.Background(), f.st, "dev1", send, now, 20); n != 0 {
			t.Fatal("a failure was counted as a verdict")
		}
		now = now.Add(2 * time.Hour)
	}
	if pass, _ := f.st.CheckPasses(); len(pass) != 0 {
		t.Fatal("a failure decided the prompt")
	}
	srv := &fakeServer{pass: map[string]bool{"PROMPT-CANARY make the zorblax counter purple": true}}
	if n, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, now, 20); n != 1 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if pass, _ := f.st.CheckPasses(); len(pass) != 1 {
		t.Fatalf("no answer for %s after the failures", p.CheckHash)
	}
}

// A prompt the server closed without a verdict (its text did not match,
// or never came) counts as not passing on the device too, and settles its
// stretch.
func TestClosedByServer(t *testing.T) {
	f := chainFixture(t, []promptcheck.Period{{From: f0(), On: false}})
	p := f.turn(t, 0, "PROMPT-CANARY make the zorblax counter purple")
	if err := f.st.CloseByServer(p.CheckHash, "missing"); err != nil {
		t.Fatal(err)
	}
	if pass, _ := f.st.CheckPasses(); pass[p.CheckHash] || len(pass) != 1 {
		t.Fatalf("%v", pass)
	}
	if q, _ := f.st.QueuedChecks(); q != 0 {
		t.Fatal("a closed prompt is still queued")
	}
	if set := stats.PromptChecks(f.st); !set.OnAt(f.t0) {
		t.Fatal("the stretch is not settled")
	}
}
