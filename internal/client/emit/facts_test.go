package emit

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"seatime/internal/client/idle"
	"seatime/internal/client/presence"
	"seatime/internal/client/store"
	"seatime/pkg/event"
)

// A run in project a: the agent reads 2 files, greps once, writes
// src/x.go (12 lines) twice and ~/notes.md (3 lines) once, runs 3 commands
// of which 1 fails, and fetches a web page. Its stop carries: 2 reads,
// 1 search, 3 writes, 3 commands, 1 failed, 1 web; 1 file written inside the
// project and 1 outside; 12 + 12 + 3 = 27 lines. The session's two prompts
// count 1 and 2, the presence facts come from the probe, git's from the
// repository reader, and the prompt is found in the tool's log.
func TestRunAndPromptFacts(t *testing.T) {
	p, home := setup(t)
	a := filepath.Join(home, "a")
	log := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(log, []byte(`{"type":"user","message":{"role":"user","content":"add a <b>bold</b> header to the page"}}`+"\n"), 0o600)
	ms := int64(1800)
	p.Presence = func() presence.Facts { return presence.Facts{Presence: presence.InputOnly, InputBeforeMS: &ms} }
	p.Repo = func(root string) *event.GitFacts {
		if root != a {
			t.Errorf("git asked about %q", root)
		}
		return &event.GitFacts{FilesChanged: 1, Insertions: 24, Head: "abc123"}
	}
	t0 := time.Now().Add(-10 * time.Minute)
	n := 0
	at := func() time.Time { n++; return t0.Add(time.Duration(n) * time.Second) }
	fail, ok := 1, 0
	evs := []event.Event{
		{Kind: event.Prompt, SessionID: "s", CWD: a, Text: "add a <b>bold</b> header to the page"},
		{Kind: event.Tool, SessionID: "s", CWD: a, Tool: "Read", Files: []string{filepath.Join(a, "x.go")}},
		{Kind: event.Tool, SessionID: "s", CWD: a, Tool: "Read", Files: []string{filepath.Join(a, "y.go")}},
		{Kind: event.Tool, SessionID: "s", CWD: a, Tool: "Grep"},
		{Kind: event.Tool, SessionID: "s", CWD: a, Tool: "Edit", Write: true, Lines: 12, Files: []string{filepath.Join(a, "src", "x.go")}},
		{Kind: event.Tool, SessionID: "s", CWD: a, Tool: "Edit", Write: true, Lines: 12, Files: []string{filepath.Join(a, "src", "x.go")}},
		{Kind: event.Tool, SessionID: "s", CWD: a, Tool: "Write", Write: true, Lines: 3, Files: []string{filepath.Join(home, "notes.md")}},
		{Kind: event.Tool, SessionID: "s", CWD: a, Tool: "Bash", Command: "go test", ExitCode: &fail},
		{Kind: event.Tool, SessionID: "s", CWD: a, Tool: "Bash", Command: "go vet", ExitCode: &ok},
		{Kind: event.Tool, SessionID: "s", CWD: a, Tool: "Bash", Command: "ls", ExitCode: &ok},
		{Kind: event.Tool, SessionID: "s", CWD: a, Tool: "WebFetch"},
		{Kind: event.AgentStop, SessionID: "s", CWD: a, Extra: map[string]any{"transcript_path": log}},
		{Kind: event.Prompt, SessionID: "s", CWD: a, Text: "now make it red please"},
	}
	var got []event.Event
	for _, e := range evs {
		e.TS = at()
		s, err := p.EmitEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	stop := got[11]
	want := event.RunFacts{Reads: 2, Searches: 1, Writes: 3, Commands: 3, CommandsFailed: 1, Web: 1, FilesInside: 1, FilesOutside: 1, LinesWritten: 27}
	if stop.Run == nil || *stop.Run != want {
		t.Fatalf("run facts %+v, want %+v", stop.Run, want)
	}
	if stop.PromptInLog == nil || !*stop.PromptInLog || stop.Git == nil || stop.Git.Insertions != 24 {
		t.Fatalf("stop: in log %v, git %+v", stop.PromptInLog, stop.Git)
	}
	if got[0].SessionPrompts != 1 || got[12].SessionPrompts != 2 || got[0].Presence != presence.InputOnly || *got[0].InputBeforeMS != 1800 {
		t.Fatalf("prompt facts: %+v / %+v", got[0], got[12])
	}
	if _, err := p.Store.VerifyChain(); err != nil {
		t.Fatal(err)
	}
	stored, _ := p.Store.Events(store.Query{Kinds: []event.Kind{event.AgentStop}})
	if len(stored) != 1 || stored[0].Run == nil || stored[0].Run.LinesWritten != 27 {
		t.Fatalf("stored stop %+v", stored)
	}
}

// A prompt the log does not hold is "no"; a prompt with a blanked secret is
// not looked for.
func TestInLog(t *testing.T) {
	log := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(log, []byte(`{"content":"something else"}`), 0o600)
	if f := inLog(log, "a prompt that was never sent"); f == nil || *f {
		t.Fatalf("%v", f)
	}
	if f := inLog(log, "use key [REDACTED:api-key] now"); f != nil {
		t.Fatalf("%v", f)
	}
}

// git diff --numstat: 10 and 2 lines in one file, a binary file (- -):
// 2 files, 10 insertions, 2 deletions.
func TestParseNumstat(t *testing.T) {
	g := ParseNumstat([]byte("10\t2\tsrc/a.go\n-\t-\tlogo.png\n"), "abc")
	if g != (event.GitFacts{FilesChanged: 2, Insertions: 10, Deletions: 2, Head: "abc"}) {
		t.Fatalf("%+v", g)
	}
}

// A plugin cannot preset SeaTime's own measurements through `seatime emit`.
func TestEmitStripsFacts(t *testing.T) {
	evs, err := DecodeEvents([]byte(`{"kind":"prompt","text":"hello there my friend","input_before_ms":5,"input_ms":90000,"input_over_ms":90000,"input_unwatched_ms":60000,"presence":"input only","run":{"writes":9},"front_window":"yes"}`), Defaults{Harness: "plug"})
	if err != nil {
		t.Fatal(err)
	}
	if e := evs[0]; e.InputBeforeMS != nil || e.InputMS != nil || e.InputOverMS != 0 || e.InputUnwatchedMS != 0 || e.Presence != "" || e.Run != nil || e.FrontWindow != "" {
		t.Fatalf("%+v", e)
	}
}

// A prompt the tool held in its queue gets how long it waited, from the
// typed time the adapter read in the tool's log. A typed time before the
// session's previous prompt is a stale match and is ignored, as is one
// under a second before sending (recent Claude Code logs every prompt).
// The key itself is never stored.
func TestQueuedPrompt(t *testing.T) {
	p, home := setup(t)
	a := filepath.Join(home, "a")
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	typed := func(d time.Duration) map[string]any {
		return map[string]any{event.TypedAtKey: t0.Add(d).Format(time.RFC3339Nano)}
	}
	evs := []event.Event{
		{TS: t0.Add(10 * time.Second), Kind: event.Prompt, SessionID: "q", CWD: a, Text: "build the settings page please", Extra: typed(10*time.Second - 5*time.Millisecond)},
		{TS: t0.Add(200 * time.Second), Kind: event.AgentStop, SessionID: "q", CWD: a},
		{TS: t0.Add(201 * time.Second), Kind: event.Prompt, SessionID: "q", CWD: a, Text: "and keep the old colours behind a flag", Extra: typed(95 * time.Second)},
		{TS: t0.Add(400 * time.Second), Kind: event.Prompt, SessionID: "q", CWD: a, Text: "build the settings page please again", Extra: typed(5 * time.Second)},
	}
	want := []int64{0, 0, 106000, 0}
	for i, e := range evs {
		s, err := p.EmitEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		if s.QueuedMS != want[i] {
			t.Errorf("record %d: queued %d ms, want %d", i, s.QueuedMS, want[i])
		}
		if _, ok := s.Extra[event.TypedAtKey]; ok {
			t.Errorf("record %d keeps the typed time in Extra", i)
		}
	}
}

// A plugin cannot mark its own prompts as queued: that would take them out
// of the reply-time facts.
func TestEmitCannotPresetQueued(t *testing.T) {
	evs, err := DecodeEvents([]byte(`{"kind":"prompt","text":"hello there agent","queued_ms":90000,"extra":{"typed_at":"2026-11-20T10:00:00Z"}}`), Defaults{})
	if err != nil || len(evs) != 1 {
		t.Fatal(err)
	}
	if evs[0].QueuedMS != 0 || evs[0].Extra[event.TypedAtKey] != nil {
		t.Errorf("kept: %d %v", evs[0].QueuedMS, evs[0].Extra)
	}
}

// A prompt carries the input the daemon counted since the project's
// previous prompt, whatever the session: the first prompt of a project
// asks as far back as the daemon counts, the next one since the first. A
// prompt whose device did not read its own idle timer (over SSH, or with
// none) carries nothing, and the daemon is not asked.
func TestInputBeforeAPrompt(t *testing.T) {
	p, home := setup(t)
	a, b := filepath.Join(home, "a"), filepath.Join(home, "b")
	ms := int64(900)
	local := presence.Facts{Presence: presence.InputOnly, InputBeforeMS: &ms}
	p.Presence = func() presence.Facts { return local }
	var asked []time.Time
	p.Input = func(since time.Time) (idle.Input, bool) {
		asked = append(asked, since)
		return idle.Input{Input: 40 * time.Second, Unwatched: 5 * time.Second, Over: 3 * time.Minute}, true
	}
	t0 := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Millisecond)
	send := func(at time.Duration, session, dir, text string) event.Event {
		t.Helper()
		e, err := p.EmitEvent(event.Event{TS: t0.Add(at), Kind: event.Prompt, SessionID: session, CWD: dir, Text: text})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	first := send(0, "s1", a, "add a settings page with a dark mode toggle")
	if first.InputMS == nil || *first.InputMS != 40000 || first.InputOverMS != 180000 || first.InputUnwatchedMS != 5000 {
		t.Fatalf("first prompt: %+v", first)
	}
	send(time.Minute, "s9", b, "write the importer for the old format")
	send(3*time.Minute, "s2", a, "keep the old colours behind a flag") // a new chat in the same project
	if len(asked) != 3 || !asked[0].IsZero() || !asked[1].IsZero() || !asked[2].Equal(t0) {
		t.Fatalf("asked since %v, want the start, the start, and project a's first prompt (%v)", asked, t0)
	}
	local = presence.Facts{Presence: presence.TerminalOnly}
	if e := send(5*time.Minute, "s2", a, "now the same for the profile page"); e.InputMS != nil || e.InputOverMS != 0 || len(asked) != 3 {
		t.Fatalf("a prompt over SSH: %+v, asked %d times", e, len(asked))
	}
	// the daemon does not answer: nothing is recorded, never "no input"
	local = presence.Facts{Presence: presence.InputOnly, InputBeforeMS: &ms}
	p.Input = func(time.Time) (idle.Input, bool) { return idle.Input{}, false }
	if e := send(6*time.Minute, "s2", a, "and one more page for the billing settings"); e.InputMS != nil {
		t.Fatalf("with no answer: %+v", e)
	}
	if _, err := p.Store.VerifyChain(); err != nil {
		t.Fatal(err)
	}
}
