package checks_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"seatime/internal/client/checks"
	"seatime/internal/client/config"
	"seatime/internal/client/emit"
	"seatime/internal/client/segment"
	"seatime/internal/client/stats"
	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/pkg/api"
	"seatime/pkg/audit"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
	"seatime/pkg/promptcheck"
)

const (
	prompt1 = "PROMPT-CANARY make the zorblax counter purple"
	reply1  = "REPLY-CANARY I made the zorblax button purple"
)

type fixture struct {
	st   *store.Store
	p    *emit.Pipeline
	proj string
	snap *promptcheck.Snapshot
	t0   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SetMeta(store.MetaPromptCheck, "1"); err != nil { // as the sync answer sets it
		t.Fatal(err)
	}
	proj := filepath.Join(home, "zorblax")
	os.MkdirAll(proj, 0o755)
	f := &fixture{st: st, p: emit.New(st, home), proj: proj, snap: &promptcheck.Snapshot{}, t0: time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)}
	f.p.Snap = func(string) *promptcheck.Snapshot { return f.snap }
	return f
}

func (f *fixture) emit(t *testing.T, at time.Duration, e event.Event) event.Event {
	t.Helper()
	e.TS, e.Harness, e.CWD = f.t0.Add(at), "claude-code", f.proj
	if e.SessionID == "" {
		e.SessionID = "s1"
	}
	out, err := f.p.EmitEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffOf(file, line string) map[string]string {
	return promptcheck.ParseDiff("diff --git a/" + file + " b/" + file + "\n--- a/" + file + "\n+++ b/" + file + "\n@@ -1 +1 @@\n-old\n+" + line + "\n")
}

type fakeServer struct {
	reqs   []api.PromptCheckReq
	pass   map[string]bool
	down   bool
	status int // answer this HTTP status instead of a verdict (503, 404, ...)
}

func (s *fakeServer) send(_ context.Context, req api.PromptCheckReq) (api.PromptCheckResp, int, error) {
	if s.down {
		return api.PromptCheckResp{}, 0, errors.New("connection refused")
	}
	if s.status != 0 {
		return api.PromptCheckResp{}, s.status, errors.New("server returned an error")
	}
	s.reqs = append(s.reqs, req)
	return api.PromptCheckResp{CheckHash: req.CheckHash}, 200, nil
}

func TestTurnsAreSentAndCounted(t *testing.T) {
	f := newFixture(t)
	p1 := f.emit(t, 0, event.Event{Kind: event.Prompt, Text: prompt1})
	if p1.CheckHash == "" || len(p1.CheckHash) != 64 {
		t.Fatalf("a new prompt got no hash: %+v", p1)
	}
	f.snap = &promptcheck.Snapshot{Files: diffOf("z.html", "purple")}
	stop := f.emit(t, 3*time.Minute, event.Event{Kind: event.AgentStop, Extra: map[string]any{promptcheck.ReplyKey: reply1}})
	if _, ok := stop.Extra[promptcheck.ReplyKey]; ok {
		t.Fatal("the reply was stored in the log")
	}
	// the switch drops the 4-word minimum: a short prompt that is not a
	// repeat is sent too; an exact repeat is not
	p2 := f.emit(t, 5*time.Minute, event.Event{Kind: event.Prompt, Text: "teach me that"})
	f.snap = &promptcheck.Snapshot{Files: diffOf("z.html", "violet")}
	p3 := f.emit(t, 6*time.Minute, event.Event{Kind: event.Prompt, Text: "Teach me that!"}) // interrupts turn 2
	if p2.CheckHash == "" || p3.CheckHash != "" {
		t.Fatalf("short new prompt hash %q, repeat hash %q", p2.CheckHash, p3.CheckHash)
	}
	evs, _ := f.st.Events(store.Query{})
	for _, e := range evs {
		if b, _ := json.Marshal(e); strings.Contains(string(b), "REPLY-CANARY") {
			t.Fatalf("the log holds the reply: %s", b)
		}
	}

	srv := &fakeServer{pass: map[string]bool{prompt1: true}}
	now := f.t0.Add(20 * time.Minute)
	n, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, now, 20)
	if err != nil || n != 2 || len(srv.reqs) != 2 {
		t.Fatalf("sent %d (%d requests): %v", n, len(srv.reqs), err)
	}
	r1, r2 := srv.reqs[0], srv.reqs[1]
	if r1.Turn.Prompt != prompt1 || promptcheck.Hash(r1.Salt, r1.Turn.Prompt) != p1.CheckHash || r1.CheckHash != p1.CheckHash {
		t.Fatalf("request 1 does not match the log: %+v", r1)
	}
	if !strings.Contains(r1.Turn.Diff, "+purple") || r1.Turn.Activity != "start of session" || len(r1.Context) != 0 || r1.Turn.PrevReply != "" {
		t.Fatalf("request 1 turn: %+v", r1.Turn)
	}
	if r2.Turn.Prompt != "teach me that" || r2.Turn.PrevReply != reply1 || len(r2.Context) != 1 || r2.Context[0].Prompt != prompt1 {
		t.Fatalf("request 2: %+v", r2)
	}
	// the interrupted turn is judged on what changed until the next prompt
	if !strings.Contains(r2.Turn.Diff, "+violet") || !strings.Contains(r2.Turn.Activity, "time since the AI finished: 2m00s") {
		t.Fatalf("request 2 turn: %+v", r2.Turn)
	}
	// the prompt after the interrupted turn sees that it was interrupted
	last, _ := f.st.LastTurn("s1")
	if last.PrevReply != checks.Interrupted || last.State != store.TurnOpen {
		t.Fatalf("turn 3: %+v", last)
	}

	// the verdicts decide what counts
	got, err := stats.Events(f.st, f.t0, now)
	if err != nil {
		t.Fatal(err)
	}
	newBy := map[string]bool{}
	for _, e := range got {
		if e.Kind == event.Prompt {
			newBy[e.CheckHash] = *e.New
		}
	}
	// the device is not told the verdicts: no checked prompt counts here,
	// the server counts them when the records arrive
	if newBy[p1.CheckHash] || newBy[p2.CheckHash] || newBy[""] {
		t.Fatalf("a prompt counted on the device: %v", newBy)
	}
}

func TestOfflineChecksWaitAndRetry(t *testing.T) {
	f := newFixture(t)
	p1 := f.emit(t, 0, event.Event{Kind: event.Prompt, Text: prompt1})
	f.emit(t, time.Minute, event.Event{Kind: event.AgentStop})
	now := f.t0.Add(15 * time.Minute)
	if pend, _ := stats.Pending(f.st, f.t0.Add(-time.Hour), now); pend <= 0 {
		t.Fatal("no time shows as waiting for the check")
	}
	srv := &fakeServer{down: true, pass: map[string]bool{prompt1: true}}
	if _, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, now, 20); err == nil {
		t.Fatal("an unreachable server was not reported")
	}
	if q, _ := f.st.QueuedChecks(); q != 1 {
		t.Fatalf("queued %d", q)
	}
	srv.down = false
	if n, _ := checks.Drain(context.Background(), f.st, "dev1", srv.send, now.Add(time.Second), 20); n != 0 {
		t.Fatal("retried before its backoff")
	}
	if n, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, now.Add(2*time.Hour), 20); n != 1 || err != nil {
		t.Fatalf("retry: %d %v", n, err)
	}
	pass, _ := f.st.CheckPasses()
	if _, ok := pass[p1.CheckHash]; !ok {
		t.Fatal("the answer was not kept")
	}
	if pend, _ := stats.Pending(f.st, f.t0.Add(-time.Hour), now); pend != 0 {
		t.Fatalf("still waiting: %v", pend)
	}
}

// The check going off only pauses the queue: nothing is sent, nothing is
// dropped, prompts are still kept and hashed, and every turn goes out when
// it is back on.
func TestSwitchOffPausesTurns(t *testing.T) {
	f := newFixture(t)
	p1 := f.emit(t, 0, event.Event{Kind: event.Prompt, Text: prompt1})
	f.emit(t, time.Minute, event.Event{Kind: event.AgentStop, Extra: map[string]any{promptcheck.ReplyKey: reply1}})
	f.st.SetMeta(store.MetaPromptCheck, "0")
	srv := &fakeServer{pass: map[string]bool{prompt1: true}}
	if _, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, f.t0.Add(time.Hour), 20); err != nil || len(srv.reqs) != 0 {
		t.Fatalf("sent with the switch off: %v", err)
	}
	if q, _ := f.st.QueuedChecks(); q != 1 {
		t.Fatalf("the queue lost its turn: %d", q)
	}
	// with it off, prompts are still hashed and kept; definition 2 is as before
	p := f.emit(t, 2*time.Minute, event.Event{Kind: event.Prompt, Text: "PROMPT-CANARY another one about the counter"})
	if p.CheckHash == "" || p.New == nil || !*p.New {
		t.Fatalf("%+v", p)
	}
	f.emit(t, 3*time.Minute, event.Event{Kind: event.AgentStop})
	f.st.SetMeta(store.MetaPromptCheck, "1")
	if n, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, f.t0.Add(2*time.Hour), 20); n != 2 || err != nil {
		t.Fatalf("back on: %d %v", n, err)
	}
	if pass, _ := f.st.CheckPasses(); len(pass) != 2 {
		t.Fatalf("the paused turns (the first: %s) were not checked: %v", p1.CheckHash, pass)
	}
}

// Owner's outage cases: the server unreachable for days, a storm of 503s,
// a 404 then back on, and a key added later all end with the queue drained
// and the minutes counted, nothing lost and nothing decided meanwhile.
func TestQueueSurvivesOutages(t *testing.T) {
	for _, c := range []struct {
		name string
		fail func(*fakeServer)
	}{
		{"unreachable for days", func(s *fakeServer) { s.down = true }},
		{"503 storm", func(s *fakeServer) { s.status = 503 }},
		{"404 then back on", func(s *fakeServer) { s.status = 404 }},
		{"no key yet (503 until the key is set)", func(s *fakeServer) { s.status = 503 }},
		{"too many checks today (429)", func(s *fakeServer) { s.status = 429 }},
		{"old client told to update (426)", func(s *fakeServer) { s.status = 426 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.emit(t, 0, event.Event{Kind: event.Prompt, Text: prompt1})
			f.emit(t, 4*time.Minute, event.Event{Kind: event.AgentStop})
			srv := &fakeServer{pass: map[string]bool{prompt1: true}}
			c.fail(srv)
			now := f.t0.Add(10 * time.Minute)
			for i := 0; i < 5*24; i++ { // five days, a round an hour
				checks.Drain(context.Background(), f.st, "dev1", srv.send, now, 20)
				now = now.Add(time.Hour)
			}
			if q, _ := f.st.QueuedChecks(); q != 1 || !f.st.PromptCheckOn() {
				t.Fatalf("after the outage: queued %d, switch %v", q, f.st.PromptCheckOn())
			}
			if pass, _ := f.st.CheckPasses(); len(pass) != 0 {
				t.Fatal("the outage decided a prompt")
			}
			if pend, _ := stats.Pending(f.st, f.t0.Add(-time.Hour), f.t0.Add(time.Hour)); pend <= 0 {
				t.Fatal("the waiting time is not pending")
			}
			*srv = fakeServer{pass: srv.pass} // back
			if n, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, now.Add(2*time.Hour), 20); n != 1 || err != nil {
				t.Fatalf("drain after: %d %v", n, err)
			}
			if pass, _ := f.st.CheckPasses(); len(pass) != 1 {
				t.Fatal("no answer after the outage")
			}
		})
	}
}

// A turn never expires: however long it waits (the server's old queue
// limit is ignored), it keeps its text and is sent once the server answers.
func TestQueuedTurnNeverExpires(t *testing.T) {
	f := newFixture(t)
	f.emit(t, 0, event.Event{Kind: event.Prompt, Text: prompt1})
	f.emit(t, 4*time.Minute, event.Event{Kind: event.AgentStop})
	f.st.SetMeta(store.MetaQueueDays, "2")
	srv := &fakeServer{down: true}
	for _, at := range []time.Duration{24 * time.Hour, 49 * time.Hour, 60 * 24 * time.Hour} {
		checks.Drain(context.Background(), f.st, "dev1", srv.send, f.t0.Add(at), 20)
	}
	if q, _ := f.st.QueuedChecks(); q != 1 {
		t.Fatal("the turn left the queue")
	}
	if x, _ := f.st.ExpiredChecks(); x != 0 {
		t.Fatalf("expired %d", x)
	}
	if last, _ := f.st.LastTurn("s1"); last == nil || last.Prompt != prompt1 || last.Salt == "" {
		t.Fatalf("text dropped: %+v", last)
	}
	if pass, _ := f.st.CheckPasses(); len(pass) != 0 {
		t.Fatal("a waiting turn got a verdict")
	}
	srv.down = false
	srv.pass = map[string]bool{prompt1: true}
	if n, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, f.t0.Add(90*24*time.Hour), 20); n != 1 || err != nil {
		t.Fatalf("drain after 60 days: %d %v", n, err)
	}
	if pass, _ := f.st.CheckPasses(); len(pass) != 1 {
		t.Fatal("no answer")
	}
}

// Guard: with the switch on, a submission still carries no prompt text and
// no reply, only the hash; the server counts its prompts, not the device.
func TestSubmissionCarriesNoCheckText(t *testing.T) {
	f := newFixture(t)
	p1 := f.emit(t, 0, event.Event{Kind: event.Prompt, Text: prompt1})
	f.emit(t, 4*time.Minute, event.Event{Kind: event.AgentStop, Extra: map[string]any{promptcheck.ReplyKey: reply1}})
	f.emit(t, 12*time.Minute, event.Event{Kind: event.Prompt, Text: "PROMPT-CANARY now make it bounce"})
	f.emit(t, 15*time.Minute, event.Event{Kind: event.AgentStop})
	srv := &fakeServer{pass: map[string]bool{prompt1: true}}
	if _, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, f.t0.Add(30*time.Minute), 20); err != nil {
		t.Fatal(err)
	}
	priv, pub, _ := syncer.NewKey()
	auth := config.Auth{DeviceID: "dev1", PrivateKey: priv, PublicKey: pub}
	keys, _ := f.st.ProjectKeys()
	var key string
	for k := range keys {
		key = k
	}
	head, _, _ := f.st.Head()
	prep, err := segment.Build(f.st, auth, segment.Options{Project: key, MaxSeq: head})
	if err != nil {
		t.Fatal(err)
	}
	if prep.Bundle.ScoreVersion != ledger.CapVersion {
		t.Fatalf("rule %q with the check on", prep.Bundle.ScoreVersion)
	}
	raw, _ := json.Marshal(prep.Bundle)
	for _, leak := range []string{"CANARY", "zorblax", "bounce"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the bundle carries %q", leak)
		}
	}
	if !strings.Contains(string(raw), p1.CheckHash) {
		t.Fatal("the bundle lacks the prompt hash the server counts by")
	}
	// the device is not told the verdicts, so its claim counts no prompt
	pass, _ := f.st.CheckPasses()
	if len(pass) != 2 || prep.Bundle.Claimed.Seconds() != 0 {
		t.Fatalf("answers %v, claim %v", pass, prep.Bundle.Claimed.Seconds())
	}
	// the server counts with its own verdicts (the first prompt passed, the
	// second did not), whatever the device claimed; with the first prompt's
	// verdict missing, its time shows as waiting
	server := map[string]bool{}
	for h := range pass {
		server[h] = h == p1.CheckHash
	}
	hash, _ := f.st.HashAt(head)
	cps := []api.ServerCheckpoint{{Seq: head, Hash: hash, ServerTS: f.t0.Add(16 * time.Minute)}}
	a := audit.Analyze(audit.Input{Bundle: prep.Bundle, Checkpoints: cps, PromptCheck: &promptcheck.Set{On: true, Pass: server}})
	if a.Raw.Seconds() < 4*60 {
		t.Fatalf("the server counted %v s", a.Raw.Seconds())
	}
	delete(server, p1.CheckHash)
	a = audit.Analyze(audit.Input{Bundle: prep.Bundle, Checkpoints: cps, PromptCheck: &promptcheck.Set{On: true, Pass: server}})
	if a.Pending != 1 || a.PendingSeconds < 4*60 || a.Raw.Seconds() != 0 {
		t.Fatalf("waiting: %d prompts, %v s pending, %v s counted", a.Pending, a.PendingSeconds, a.Raw.Seconds())
	}
}

// The activity line's front-window time comes from the idle records' 5 s
// slices; without them it is "not measured" and the check request says so
// (the server then skips the question that needs it).
func TestFrontWindowTime(t *testing.T) {
	for _, measured := range []bool{false, true} {
		f := newFixture(t)
		f.emit(t, 0, event.Event{Kind: event.Prompt, Text: prompt1})
		f.emit(t, time.Minute, event.Event{Kind: event.AgentStop, Extra: map[string]any{promptcheck.ReplyKey: reply1}})
		m := f.t0.Add(time.Minute).Truncate(time.Minute).UnixMilli()
		idle := event.Event{Kind: event.Idle, Harness: "seatime", TS: f.t0.Add(4 * time.Minute), WindowMS: 4 * 60000,
			InputSlices: [][2]int64{{m, 0xfff}, {m + 60000, 0xfff}}}
		if measured {
			idle.FrontSlices = [][2]int64{{m, 0x00f}, {m + 60000, 0}}
		}
		if _, err := f.p.EmitEvent(idle); err != nil {
			t.Fatal(err)
		}
		f.emit(t, 3*time.Minute, event.Event{Kind: event.Prompt, Text: "PROMPT-CANARY the button flickers when I click it"})
		f.emit(t, 4*time.Minute, event.Event{Kind: event.AgentStop})
		srv := &fakeServer{}
		if _, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, f.t0.Add(5*time.Minute), 20); err != nil || len(srv.reqs) != 2 {
			t.Fatalf("%v, %d sent", err, len(srv.reqs))
		}
		r := srv.reqs[1]
		if r.FrontMeasured != measured {
			t.Fatalf("measured %v: request says %v (%s)", measured, r.FrontMeasured, r.Turn.Activity)
		}
		notMeasured := strings.Contains(r.Turn.Activity, "window in front: not measured")
		if notMeasured == measured || strings.Contains(r.Turn.Activity, "input in that time: not measured") {
			t.Fatalf("measured %v: activity %q", measured, r.Turn.Activity)
		}
	}
}

// A finished turn goes out at once: the time before its prompt is read from
// the idle recorder's open window when no record of it is written yet.
func TestSentAtTurnEnd(t *testing.T) {
	f := newFixture(t)
	f.emit(t, 0, event.Event{Kind: event.Prompt, Text: prompt1})
	f.emit(t, time.Minute, event.Event{Kind: event.AgentStop})
	f.emit(t, 3*time.Minute, event.Event{Kind: event.Prompt, Text: "PROMPT-CANARY make it count down instead"})
	f.emit(t, 4*time.Minute, event.Event{Kind: event.AgentStop})
	m := f.t0.Add(time.Minute).Truncate(time.Minute).UnixMilli()
	live := event.Event{Kind: event.Idle, TS: f.t0.Add(4 * time.Minute), WindowMS: 4 * 60000,
		InputSlices: [][2]int64{{m, 0xfff}, {m + 60000, 0xfff}, {m + 120000, 0xfff}}}
	srv := &fakeServer{}
	now := f.t0.Add(4*time.Minute + time.Second) // a second after the AI stopped
	if n, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, now, 20, live); err != nil || n != 2 {
		t.Fatalf("sent %d: %v", n, err)
	}
	if a := srv.reqs[1].Turn.Activity; strings.Contains(a, "input in that time: not measured") {
		t.Fatalf("live input not used: %q", a)
	}
	// without any record of that time, it still goes out, as not measured
	f2 := newFixture(t)
	f2.emit(t, 0, event.Event{Kind: event.Prompt, Text: prompt1})
	f2.emit(t, time.Minute, event.Event{Kind: event.AgentStop})
	f2.emit(t, 3*time.Minute, event.Event{Kind: event.Prompt, Text: "PROMPT-CANARY make it count down instead"})
	f2.emit(t, 4*time.Minute, event.Event{Kind: event.AgentStop})
	srv2 := &fakeServer{}
	if n, _ := checks.Drain(context.Background(), f2.st, "dev1", srv2.send, now, 20); n != 2 || !strings.Contains(srv2.reqs[1].Turn.Activity, "input in that time: not measured") {
		t.Fatalf("sent %d: %+v", n, srv2.reqs)
	}
}

// topicJudge stands in for the model on the undo red team's paths: a
// prompt is a repeat when an earlier prompt of the same topic (the word
// after "TOPIC-") is anywhere in what the check is shown, context turns
// or earlier prompts. It answers only from the request, as the model does.
type topicJudge struct {
	reqs []api.PromptCheckReq
	pass map[string]bool // by prompt hash: the decisions stay with the judge, as on the server
}

func topic(s string) string {
	_, t, ok := strings.Cut(s, "TOPIC-")
	if !ok {
		return ""
	}
	return strings.Fields(t)[0]
}

func (j *topicJudge) send(_ context.Context, req api.PromptCheckReq) (api.PromptCheckResp, int, error) {
	j.reqs = append(j.reqs, req)
	if j.pass == nil {
		j.pass = map[string]bool{}
	}
	// the device sends the earlier prompts it has; the server shows the
	// judge those it passed
	shown := slices.Clone(req.Context)
	for _, e := range req.Earlier {
		if j.pass[e.CheckHash] {
			shown = append(shown, e)
		}
	}
	tp, again := topic(req.Turn.Prompt), false
	for _, t := range shown {
		if tp != "" && topic(t.Prompt) == tp {
			again = true
		}
	}
	j.pass[req.CheckHash] = tp != "" && !again // throwaway prompts ("ok 3") have no topic and fail
	return api.PromptCheckResp{CheckHash: req.CheckHash}, 200, nil
}

func (j *topicJudge) last(t *testing.T, prompt string) api.PromptCheckReq {
	t.Helper()
	for i := len(j.reqs) - 1; i >= 0; i-- {
		if j.reqs[i].Turn.Prompt == prompt {
			return j.reqs[i]
		}
	}
	t.Fatalf("%q was never sent", prompt)
	return api.PromptCheckReq{}
}

// The undo red team's farming paths, each caught by what the check is now
// shown: the project's turns across chats (fix 1) and its earlier prompts
// that passed (fix 2); an honest new chat still passes.
func TestUndoFarmingPaths(t *testing.T) {
	drain := func(t *testing.T, f *fixture, j *topicJudge, at time.Duration) {
		t.Helper()
		if _, err := checks.Drain(context.Background(), f.st, "dev1", j.send, f.t0.Add(at), 50); err != nil {
			t.Fatal(err)
		}
	}
	ask := func(t *testing.T, f *fixture, at time.Duration, sess, text string) event.Event {
		p := f.emit(t, at, event.Event{Kind: event.Prompt, Text: text, SessionID: sess})
		f.emit(t, at+time.Minute, event.Event{Kind: event.AgentStop, SessionID: sess, Extra: map[string]any{promptcheck.ReplyKey: "done"}})
		return p
	}

	t.Run("new chat before a reworded resend", func(t *testing.T) {
		f, j := newFixture(t), &topicJudge{}
		ask(t, f, 0, "s1", "add a pause key TOPIC-pause to the game")
		drain(t, f, j, 5*time.Minute)
		again := "let me pause the game with a key TOPIC-pause please"
		p := ask(t, f, 10*time.Minute, "s2", again)
		drain(t, f, j, 15*time.Minute)
		r := j.last(t, again)
		if j.pass[p.CheckHash] || r.Turn.PrevReply != promptcheck.NewChat || strings.Contains(r.Turn.Activity, "start of session") ||
			!strings.Contains(r.Turn.Activity, "time since the AI finished: 9m00s") || len(r.Context) != 1 || r.Context[0].CheckHash == "" {
			t.Fatalf("passed %v; request %+v", j.pass[p.CheckHash], r.Turn)
		}
	})
	t.Run("six throwaway prompts then rewind and resend", func(t *testing.T) {
		f, j := newFixture(t), &topicJudge{}
		ask(t, f, 0, "s1", "add a high score table TOPIC-score")
		for i := 1; i <= 6; i++ {
			ask(t, f, time.Duration(i)*2*time.Minute, "s1", fmt.Sprintf("ok %d", i))
		}
		drain(t, f, j, 20*time.Minute)
		again := "show the best scores in a table TOPIC-score"
		p := ask(t, f, 30*time.Minute, "s1", again)
		drain(t, f, j, 35*time.Minute)
		r := j.last(t, again)
		// the device sends every checked prompt before the context turns;
		// the server keeps the one it passed
		if j.pass[p.CheckHash] || len(r.Context) != 6 || len(r.Earlier) != 1 || topic(r.Earlier[0].Prompt) != "score" || r.Earlier[0].CheckHash == "" {
			t.Fatalf("passed %v; context %d, earlier %+v", j.pass[p.CheckHash], len(r.Context), r.Earlier)
		}
	})
	t.Run("seven-ask cycle replayed", func(t *testing.T) {
		f, j := newFixture(t), &topicJudge{}
		topics := []string{"menu", "sound", "levels", "colors", "speed", "walls", "lives"}
		for i, tp := range topics {
			ask(t, f, time.Duration(i)*3*time.Minute, "s1", "please add TOPIC-"+tp+" to the snake game")
		}
		drain(t, f, j, 30*time.Minute)
		again := "can the snake game get TOPIC-menu now"
		p := ask(t, f, 40*time.Minute, "s1", again)
		drain(t, f, j, 45*time.Minute)
		if j.pass[p.CheckHash] {
			t.Fatalf("the replayed ask passed; earlier %d", len(j.last(t, again).Earlier))
		}
	})
	t.Run("honest new chat still passes", func(t *testing.T) {
		f, j := newFixture(t), &topicJudge{}
		ask(t, f, 0, "s1", "add a pause key TOPIC-pause to the game")
		drain(t, f, j, 5*time.Minute)
		fresh := "make the snake wrap around the edges TOPIC-wrap"
		p := ask(t, f, 10*time.Minute, "s2", fresh)
		drain(t, f, j, 15*time.Minute)
		if !j.pass[p.CheckHash] || j.last(t, fresh).Turn.PrevReply != promptcheck.NewChat {
			t.Fatal("an honest new chat failed")
		}
	})
}
