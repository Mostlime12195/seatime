package emit

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"seatime/internal/client/presence"
	"seatime/internal/client/store"
	"seatime/pkg/event"
	"seatime/pkg/promptcheck"
)

// logSays is a stand-in for the AI tool's log: prompt id -> sender.
type logSays map[string]string

func (l logSays) sender(_, id string) (string, bool) { k, ok := l[id]; return k, ok }

func held(e event.Event, id string) event.Event {
	e.Harness = "claude-code"
	e.Extra = map[string]any{event.HoldKey: map[string]any{"log": "/logs/s.jsonl", "id": id}}
	return e
}

func kinds(t *testing.T, st *store.Store) []event.Kind {
	t.Helper()
	evs, err := st.Events(store.Query{})
	if err != nil {
		t.Fatal(err)
	}
	var out []event.Kind
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func same(a, b []event.Kind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A prompt is in no record until the tool's log says a person sent it. It
// is then recorded with the time it was sent and with what the hook read
// at that moment, before the session's next record.
func TestHeldPromptIsRecordedWhenAPersonSentIt(t *testing.T) {
	p, home := setup(t)
	log := logSays{}
	p.Sender = log.sender
	reads := 0
	ms := int64(120)
	p.Presence = func() presence.Facts { reads++; return presence.Facts{Presence: "active", InputBeforeMS: &ms} }
	snaps := 0
	p.Snap = func(string) *promptcheck.Snapshot {
		snaps++
		return &promptcheck.Snapshot{Files: map[string]string{"at": "the prompt"}}
	}
	facts := 0
	p.Facts = func(_, name string) *promptcheck.Project { facts++; return &promptcheck.Project{Name: name} }
	a := filepath.Join(home, "a")
	t0 := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	for _, e := range []event.Event{
		held(event.Event{Kind: event.Prompt, SessionID: "s", CWD: a, TS: t0, Text: "make the parser keep comments"}, "p1"),
		held(event.Event{Kind: event.AgentStart, SessionID: "s", CWD: a, TS: t0}, "p1"),
	} {
		got, err := p.EmitEvent(e)
		if err != nil || got.Seq != 0 {
			t.Fatalf("%s: stored %+v, %v", e.Kind, got, err)
		}
	}
	if k := kinds(t, p.Store); len(k) != 0 {
		t.Fatalf("recorded before the log said who sent it: %v", k)
	}
	if n := p.ReleaseHeld(time.Now()); n != 0 {
		t.Fatalf("the daemon decided %d with nothing in the log", n)
	}
	// the tool writes the prompt to its log; its first tool call follows
	log["p1"] = "human"
	p.Presence = func() presence.Facts { t.Error("presence read again at the release"); return presence.Facts{} }
	p.Snap = func(string) *promptcheck.Snapshot {
		return &promptcheck.Snapshot{Files: map[string]string{"at": "later"}}
	}
	if _, err := p.EmitEvent(event.Event{Kind: event.Tool, Harness: "claude-code", SessionID: "s", CWD: a, TS: t0.Add(20 * time.Second), Tool: "Read"}); err != nil {
		t.Fatal(err)
	}
	if k := kinds(t, p.Store); !same(k, []event.Kind{event.Prompt, event.AgentStart, event.Tool}) {
		t.Fatalf("records: %v", k)
	}
	evs, _ := p.Store.Events(store.Query{Kinds: []event.Kind{event.Prompt}})
	pr := evs[0]
	if !pr.TS.Equal(t0) || pr.Presence != "active" || pr.InputBeforeMS == nil || *pr.InputBeforeMS != 120 || pr.SessionPrompts != 1 || pr.CheckHash == "" || pr.Project != "github.com/me/a" {
		t.Fatalf("prompt: %+v", pr)
	}
	if _, has := pr.Extra[event.HoldKey]; has {
		t.Fatalf("the hold mark was stored: %v", pr.Extra)
	}
	if reads != 1 || snaps != 1 || facts != 1 {
		t.Fatalf("read at the prompt: presence %d, folder %d, facts %d", reads, snaps, facts)
	}
	turn, err := p.Store.LastTurn("s")
	if err != nil || turn == nil || turn.Start == nil || turn.Start.Files["at"] != "the prompt" || turn.Info == nil {
		t.Fatalf("turn: %+v %v", turn, err)
	}
	if rows, _ := p.Store.HeldRows(""); len(rows) != 0 {
		t.Fatalf("still held: %d", len(rows))
	}
	if n, err := p.Store.VerifyChain(); err != nil || n != 3 {
		t.Fatal(n, err)
	}
}

// A message the tool made itself never becomes a prompt: only the run it
// started is recorded.
func TestHeldPromptFromTheToolIsNoPrompt(t *testing.T) {
	for _, kind := range []string{"task-notification", "peer", "channel", "auto-continuation", "coordinator"} {
		p, home := setup(t)
		p.Sender = logSays{"p1": kind}.sender
		a := filepath.Join(home, "a")
		for _, e := range []event.Event{
			held(event.Event{Kind: event.Prompt, SessionID: "s", CWD: a, Text: "a long report nobody typed"}, "p1"),
			held(event.Event{Kind: event.AgentStart, SessionID: "s", CWD: a}, "p1"),
			{Kind: event.AgentStop, Harness: "claude-code", SessionID: "s", CWD: a},
		} {
			if _, err := p.EmitEvent(e); err != nil {
				t.Fatal(err)
			}
		}
		if k := kinds(t, p.Store); !same(k, []event.Kind{event.AgentStart, event.AgentStop}) {
			t.Errorf("%s: records %v", kind, k)
		}
		if turn, _ := p.Store.LastTurn("s"); turn != nil {
			t.Errorf("%s: a turn was kept for the check: %+v", kind, turn)
		}
		if es, _ := p.Store.UnsentChain(10); len(es) != 0 {
			t.Errorf("%s: a prompt hash was made: %v", kind, es)
		}
	}
}

// The daemon records a held prompt once the log names its sender, and one
// the log never holds once HeldWait has passed. A log that holds the
// prompt and names no sender (an older tool) leaves it a prompt.
func TestDaemonReleasesHeldPrompts(t *testing.T) {
	p, home := setup(t)
	log := logSays{}
	p.Sender = log.sender
	a := filepath.Join(home, "a")
	now := time.Now()
	p.Now = func() time.Time { return now }
	hold := func(session, id string) {
		t.Helper()
		for _, k := range []event.Kind{event.Prompt, event.AgentStart} {
			e := held(event.Event{Kind: k, SessionID: session, CWD: a}, id)
			if k == event.Prompt {
				e.Text = "prompt " + id
			}
			if _, err := p.EmitEvent(e); err != nil {
				t.Fatal(err)
			}
		}
	}
	hold("s1", "typed")
	hold("s2", "never-logged")
	hold("s3", "no-sender")
	hold("s4", "tool")
	log["typed"], log["no-sender"], log["tool"] = "human", "", "task-notification"
	if n := p.ReleaseHeld(now.Add(time.Second)); n != 2 { // typed, tool
		t.Fatalf("decided %d, want 2", n)
	}
	if n := p.ReleaseHeld(now.Add(heldSettle + time.Second)); n != 1 { // no-sender
		t.Fatalf("decided %d, want 1", n)
	}
	if n := p.ReleaseHeld(now.Add(HeldWait - time.Second)); n != 0 {
		t.Fatalf("gave up on the log early: %d", n)
	}
	if n := p.ReleaseHeld(now.Add(HeldWait + time.Second)); n != 1 { // never-logged
		t.Fatalf("decided %d, want 1", n)
	}
	prompts, _ := p.Store.Events(store.Query{Kinds: []event.Kind{event.Prompt}})
	starts, _ := p.Store.Events(store.Query{Kinds: []event.Kind{event.AgentStart}})
	if len(prompts) != 3 || len(starts) != 4 {
		t.Fatalf("%d prompts, %d agent starts; want 3 and 4", len(prompts), len(starts))
	}
	for _, e := range prompts {
		if e.SessionID == "s4" {
			t.Fatalf("the tool's own message was recorded as a prompt")
		}
	}
}

// A second prompt of a session, held while the first still is, comes after
// it; a release that died half way is finished without a second copy.
func TestHeldOrderAndUnfinishedRelease(t *testing.T) {
	p, home := setup(t)
	log := logSays{}
	p.Sender = log.sender
	a := filepath.Join(home, "a")
	t0 := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	for i, id := range []string{"p1", "p2"} {
		ts := t0.Add(time.Duration(i) * 10 * time.Second)
		for _, k := range []event.Kind{event.Prompt, event.AgentStart} {
			e := held(event.Event{Kind: k, SessionID: "s", CWD: a, TS: ts}, id)
			if k == event.Prompt {
				e.Text = "prompt number " + id
			}
			if _, err := p.EmitEvent(e); err != nil {
				t.Fatal(err)
			}
		}
	}
	// the log holds neither yet: both wait, in order
	if rows, _ := p.Store.HeldRows("s"); len(rows) != 2 || rows[0].PromptID != "p1" || len(rows[1].After) != 1 {
		t.Fatalf("held: %+v", rows)
	}
	if k := kinds(t, p.Store); len(k) != 0 {
		t.Fatalf("records: %v", k)
	}
	// the log gets the second only: it still waits behind the first
	log["p2"] = "human"
	if n := p.ReleaseHeld(time.Now()); n != 0 {
		t.Fatalf("a later prompt went in ahead of an earlier one: %d", n)
	}
	// a process claims the first, stores its prompt and dies
	log["p1"] = "human"
	rows, _ := p.Store.HeldRows("s")
	now := time.Now()
	if ok, _ := p.Store.ClaimHeld(rows[0].ID, now); !ok {
		t.Fatal("claim")
	}
	if _, err := p.emit(rows[0].Prompt, nil, &rows[0]); err != nil {
		t.Fatal(err)
	}
	if n := p.ReleaseHeld(now.Add(time.Second)); n != 0 {
		t.Fatalf("took a release another process holds: %d", n)
	}
	if n := p.ReleaseHeld(now.Add(store.HeldClaim + time.Second)); n != 2 {
		t.Fatalf("did not finish the release: %d", n)
	}
	if k := kinds(t, p.Store); !same(k, []event.Kind{event.Prompt, event.AgentStart, event.Prompt, event.AgentStart}) {
		t.Fatalf("records: %v", k)
	}
	evs, _ := p.Store.Events(store.Query{Kinds: []event.Kind{event.Prompt}})
	if evs[0].SessionPrompts != 1 || evs[1].SessionPrompts != 2 || !evs[1].TS.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("prompts: %+v", evs)
	}
}

// A record of the session can come in before the tool has written the
// prompt to its log (a subagent's tool call, in the moment between). The
// prompt then stays held: a log that does not hold it yet says nothing
// about who sent it. When the log does, a message from the tool is
// dropped as ever.
func TestRecordBeforeTheLogHoldsThePrompt(t *testing.T) {
	p, home := setup(t)
	log := logSays{}
	p.Sender = log.sender
	a := filepath.Join(home, "a")
	for _, k := range []event.Kind{event.Prompt, event.AgentStart} {
		e := held(event.Event{Kind: k, SessionID: "s", CWD: a}, "p1")
		if k == event.Prompt {
			e.Text = "a message from a channel that nobody typed here"
		}
		if _, err := p.EmitEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.EmitEvent(event.Event{Kind: event.Tool, Harness: "claude-code", SessionID: "s", CWD: a, Tool: "Read"}); err != nil {
		t.Fatal(err)
	}
	if k := kinds(t, p.Store); !same(k, []event.Kind{event.Tool}) {
		t.Fatalf("with the prompt not in the log yet: %v", k)
	}
	log["p1"] = "channel"
	if _, err := p.EmitEvent(event.Event{Kind: event.AgentStop, Harness: "claude-code", SessionID: "s", CWD: a}); err != nil {
		t.Fatal(err)
	}
	if k := kinds(t, p.Store); !same(k, []event.Kind{event.Tool, event.AgentStart, event.AgentStop}) {
		t.Fatalf("records: %v", k)
	}
}

// The same prompt can reach the pipeline twice: from its hook and, when
// the hook ran out of time, from the hook's spool. It is held once.
func TestSamePromptIsHeldOnce(t *testing.T) {
	p, home := setup(t)
	log := logSays{}
	p.Sender = log.sender
	a := filepath.Join(home, "a")
	ts := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	for range 2 {
		for _, k := range []event.Kind{event.Prompt, event.AgentStart} {
			e := held(event.Event{Kind: k, SessionID: "s", CWD: a, TS: ts}, "p1")
			if k == event.Prompt {
				e.Text = "make the parser keep comments"
			}
			if _, err := p.EmitEvent(e); err != nil {
				t.Fatal(err)
			}
		}
	}
	log["p1"] = "human"
	if _, err := p.EmitEvent(event.Event{Kind: event.Tool, Harness: "claude-code", SessionID: "s", CWD: a, Tool: "Read"}); err != nil {
		t.Fatal(err)
	}
	if k := kinds(t, p.Store); !same(k, []event.Kind{event.Prompt, event.AgentStart, event.Tool}) {
		t.Fatalf("records: %v", k)
	}
}

func TestMain(m *testing.M) {
	heldPoll = 0 // no waiting for a log in tests
	os.Exit(m.Run())
}
