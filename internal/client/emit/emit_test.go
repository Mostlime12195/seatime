package emit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seatime/internal/client/store"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

func setup(t *testing.T) (*Pipeline, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	for name, remote := range map[string]string{"a": "https://github.com/me/a.git", "b": "https://github.com/me/b.git"} {
		d := filepath.Join(home, name)
		os.MkdirAll(filepath.Join(d, ".git"), 0o755)
		os.WriteFile(filepath.Join(d, ".git", "config"), []byte("[remote \"origin\"]\n\turl = "+remote+"\n"), 0o644)
	}
	st, err := store.Open(filepath.Join(root, "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, home), home
}

func TestPromptFeaturesAndScrub(t *testing.T) {
	p, home := setup(t)
	e, err := p.EmitEvent(event.Event{Kind: event.Prompt, Harness: "h", SessionID: "s", CWD: filepath.Join(home, "a"),
		Text: "use key sk-abcdefghijklmnopqrstuvwxyz123 and mail me@x.io"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.Text, "sk-abc") || strings.Contains(e.Text, "me@x.io") {
		t.Fatalf("text not scrubbed: %q", e.Text)
	}
	if e.Project != "github.com/me/a" || e.CWD != "" {
		t.Fatalf("project/cwd: %q %q", e.Project, e.CWD)
	}
	// "use key [REDACTED:api-key] and mail [email]" normalises to "use key
	// redacted api key and mail email", 8 words
	if e.New == nil || !*e.New || e.Words != 8 || e.TextHash != "" || e.Chars != 0 || e.TS.IsZero() {
		t.Fatalf("features: %+v", e)
	}
	if n, err := p.Store.VerifyChain(); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

// The report: one session started in repo a; it reads in b, runs a command
// after `cd b`, writes in b and in the AI tool's own memory folder. All of
// it is a's, no other project appears, and the outside writes are counted
// as files outside the project.
func TestSessionBelongsToWhereItStarted(t *testing.T) {
	p, home := setup(t)
	a, b := filepath.Join(home, "a"), filepath.Join(home, "b")
	mem := filepath.Join(home, ".claude", "projects", "x", "memory", "note.md")
	t0 := time.Now().Add(-10 * time.Minute)
	at := func(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }
	zero := 0
	for _, e := range []event.Event{
		{Kind: event.SessionStart, SessionID: "s", CWD: a, TS: at(0), Model: "m-1"},
		{Kind: event.Prompt, SessionID: "s", CWD: a, TS: at(1), Text: "compare our parser with the one in b"},
		{Kind: event.AgentStart, SessionID: "s", CWD: a, TS: at(1)},
		{Kind: event.Tool, SessionID: "s", CWD: a, TS: at(20), Tool: "Read", Files: []string{filepath.Join(b, "parse.go")}},
		{Kind: event.Tool, SessionID: "s", CWD: b, TS: at(40), Tool: "Bash", Command: "go test ./...", ExitCode: &zero},
		{Kind: event.Tool, SessionID: "s", CWD: b, TS: at(60), Tool: "Write", Write: true, Files: []string{"notes.md"}},
		{Kind: event.Tool, SessionID: "s", CWD: b, TS: at(80), Tool: "Write", Write: true, Files: []string{mem}},
		{Kind: event.Tool, SessionID: "s", CWD: b, TS: at(100), Tool: "Edit", Write: true, Files: []string{filepath.Join(a, "parse.go")}},
		{Kind: event.AgentStop, SessionID: "s", CWD: b, TS: at(120)},
		{Kind: event.Prompt, SessionID: "s", CWD: b, TS: at(200), Text: "now the same for the lexer please"},
	} {
		e.Harness = "claude-code"
		if _, err := p.EmitEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	evs, _ := p.Store.Events(store.Query{})
	for _, e := range evs {
		if e.Project != "github.com/me/a" {
			t.Errorf("%s at %s went to %q", e.Kind, e.TS.Sub(t0), e.Project)
		}
		if e.Model != "m-1" {
			t.Errorf("%s: model %q, want the session's", e.Kind, e.Model)
		}
		if e.Kind == event.AgentStop && (e.Run == nil || e.Run.FilesInside != 1 || e.Run.FilesOutside != 2) {
			t.Errorf("stop facts %+v, want 1 file inside and 2 outside", e.Run)
		}
		for _, f := range e.Files {
			if e.Write && f != "parse.go" && !strings.HasPrefix(f, "~/") {
				t.Errorf("an outside write is stored as %q", f)
			}
		}
	}
	ps, _ := p.Store.Projects(time.Time{})
	if len(ps) != 1 || ps[0].Key != "github.com/me/a" || ps[0].Root != a {
		t.Fatalf("projects: %+v", ps)
	}
	r := ledger.Compute(evs, t0.Add(-time.Hour), t0.Add(time.Hour))
	if len(r.ByProject) != 1 || r.ByProject["github.com/me/a"] <= 0 {
		t.Fatalf("counted time by project: %v", r.ByProject)
	}
	// the same session again later (resumed), now standing in b: still a's
	e, _ := p.EmitEvent(event.Event{Kind: event.Prompt, SessionID: "s", CWD: b, Text: "and a third thing to do here"})
	if e.Project != "github.com/me/a" {
		t.Fatalf("a resumed session moved to %q", e.Project)
	}
}

// An editor's edit of a file in an AI tool's own folder (its memory, a tool
// result opened to read) is not project work: nothing is recorded, and no
// project called "memory" appears.
func TestHandEditInToolFolderDropped(t *testing.T) {
	p, home := setup(t)
	for _, f := range []string{filepath.Join(home, ".claude", "projects", "x", "memory", "note.md"), filepath.Join(home, ".claude", "projects", "x", "tool-results", "b1.txt")} {
		os.MkdirAll(filepath.Dir(f), 0o755)
		os.WriteFile(f, []byte("x"), 0o644)
		if e, err := p.EmitEvent(event.Event{Kind: event.Edit, Harness: "editor:vscode", Files: []string{f}}); err != nil || e.Kind != "" {
			t.Fatalf("an edit of %s was recorded: %+v %v", f, e, err)
		}
	}
	if e, err := p.EmitEvent(event.Event{Kind: event.Edit, Harness: "editor:vscode", Files: []string{filepath.Join(home, "a", "m.go")}}); err != nil || e.Project != "github.com/me/a" {
		t.Fatalf("an edit in a project: %+v %v", e, err)
	}
	evs, _ := p.Store.Events(store.Query{})
	ps, _ := p.Store.Projects(time.Time{})
	if len(evs) != 1 || len(ps) != 1 || ps[0].Key != "github.com/me/a" {
		t.Fatalf("%d records, projects %+v", len(evs), ps)
	}
}

// A session started outside any repo goes by where it writes.
func TestSessionStartedOutsideARepo(t *testing.T) {
	p, home := setup(t)
	a, b := filepath.Join(home, "a"), filepath.Join(home, "b")
	mem := filepath.Join(home, ".claude", "projects", "x", "memory", "note.md")
	emit := func(e event.Event) event.Event {
		t.Helper()
		out, err := p.EmitEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// in the home folder: the home folder until it writes in a real project
	if e := emit(event.Event{Kind: event.Prompt, SessionID: "h", CWD: home, Text: "one two three four"}); e.Project != "local:~" {
		t.Fatalf("before any write: %q", e.Project)
	}
	if e := emit(event.Event{Kind: event.Tool, SessionID: "h", CWD: home, Tool: "Write", Write: true, Files: []string{mem}}); e.Project != "local:~" {
		t.Fatalf("a write in the tool's own folder went to %q", e.Project)
	}
	if e := emit(event.Event{Kind: event.Tool, SessionID: "h", CWD: home, Tool: "Write", Write: true, Files: []string{filepath.Join(b, "x.go")}}); e.Project != "github.com/me/b" {
		t.Fatalf("a write in b went to %q", e.Project)
	}
	if e := emit(event.Event{Kind: event.Tool, SessionID: "h", CWD: a, Tool: "Bash", Command: "ls"}); e.Project != "github.com/me/b" {
		t.Fatalf("a command after cd went to %q", e.Project)
	}
	if e := emit(event.Event{Kind: event.Prompt, SessionID: "h", CWD: home, Text: "five six seven eight"}); e.Project != "github.com/me/b" {
		t.Fatalf("the next prompt went to %q", e.Project)
	}
	// in a plain folder: the folder is the project; a repo inside it takes
	// what is written in it, a repo beside it does not
	plain := filepath.Join(home, "plain")
	os.MkdirAll(filepath.Join(plain, "inner", ".git"), 0o755)
	if e := emit(event.Event{Kind: event.Prompt, SessionID: "p", CWD: plain, Text: "one two three four"}); e.Project != "local:plain" {
		t.Fatalf("plain folder: %q", e.Project)
	}
	if e := emit(event.Event{Kind: event.Tool, SessionID: "p", CWD: plain, Tool: "Write", Write: true, Files: []string{filepath.Join(plain, "src", "x.py")}}); e.Project != "local:plain" || e.Files[0] != "src/x.py" {
		t.Fatalf("a write in a subfolder: %q %v", e.Project, e.Files)
	}
	if e := emit(event.Event{Kind: event.Tool, SessionID: "p", CWD: b, Tool: "Write", Write: true, Files: []string{filepath.Join(b, "y.go"), mem}}); e.Project != "local:plain" {
		t.Fatalf("a write in a repo beside the folder went to %q", e.Project)
	}
	if e := emit(event.Event{Kind: event.Tool, SessionID: "q", CWD: plain, Tool: "Write", Write: true, Files: []string{filepath.Join(plain, "inner", "z.go")}}); e.Project != "local:inner" {
		t.Fatalf("a write in a repo inside the folder went to %q", e.Project)
	}
	ps, _ := p.Store.Projects(time.Time{})
	for _, pr := range ps {
		if pr.Key == "local:memory" || pr.Key == "local:src" || pr.Key == "github.com/me/a" {
			t.Errorf("project %q should not exist", pr.Key)
		}
	}
}

// A record that names no model takes the session's last named one; a hand
// edit never takes one.
func TestSessionModelCarriedForward(t *testing.T) {
	p, home := setup(t)
	a := filepath.Join(home, "a")
	emit := func(e event.Event) event.Event {
		out, _ := p.EmitEvent(e)
		return out
	}
	if e := emit(event.Event{Kind: event.Prompt, SessionID: "s", CWD: a, Text: "one two three four"}); e.Model != "" {
		t.Fatalf("no model named yet, got %q", e.Model)
	}
	emit(event.Event{Kind: event.SessionStart, SessionID: "s", CWD: a, Model: " opus-1 "})
	if e := emit(event.Event{Kind: event.Prompt, SessionID: "s", CWD: a, Text: "five six seven eight"}); e.Model != "opus-1" {
		t.Fatalf("carried model %q", e.Model)
	}
	emit(event.Event{Kind: event.AgentStop, SessionID: "s", CWD: a, Model: "sonnet-2"})
	if e := emit(event.Event{Kind: event.Prompt, SessionID: "s", CWD: a, Text: "nine ten eleven twelve"}); e.Model != "sonnet-2" {
		t.Fatalf("after a switch %q", e.Model)
	}
	if e := emit(event.Event{Kind: event.Prompt, SessionID: "other", CWD: a, Text: "a b c d"}); e.Model != "" {
		t.Fatalf("another session took %q", e.Model)
	}
	if e := emit(event.Event{Kind: event.Edit, Harness: "editor:vscode", Files: []string{filepath.Join(a, "m.go")}}); e.Model != "" {
		t.Fatalf("a hand edit took %q", e.Model)
	}
}

func TestEditEventFromFile(t *testing.T) {
	p, home := setup(t)
	e, err := p.EmitEvent(event.Event{Kind: event.Edit, Harness: "editor:vscode", Files: []string{filepath.Join(home, "b", "src", "m.go")}, TS: time.Now().Add(-time.Minute)})
	if err != nil || e.Project != "github.com/me/b" || e.Files[0] != "src/m.go" {
		t.Fatalf("%+v %v", e, err)
	}
	ps, _ := p.Store.Projects(time.Time{})
	if len(ps) != 1 || ps[0].Root == "" {
		t.Fatalf("%+v", ps)
	}
}

func TestDecodeEvents(t *testing.T) {
	d := Defaults{Harness: "plug", CWD: "/x"}
	for _, in := range []string{
		`{"kind":"prompt","text":"hi"}`,
		`[{"kind":"prompt"},{"kind":"edit"}]`,
		"{\"kind\":\"prompt\"}\n{\"kind\":\"run\",\"run_kind\":\"test\"}\n",
	} {
		evs, err := DecodeEvents([]byte(in), d)
		if err != nil || len(evs) == 0 || evs[0].Harness != "plug" || evs[0].CWD != "/x" {
			t.Fatalf("%q: %v %+v", in, err, evs)
		}
	}
	if _, err := DecodeEvents([]byte(`{"kind":"nope"}`), d); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err := DecodeEvents([]byte(`{"kind":"edit"`), d); err == nil {
		t.Fatal("bad json accepted")
	}
	if _, err := DecodeEvents([]byte(`{"kind":"prompt","harness":"web:chatgpt","text":"hi"}`), d); err == nil {
		t.Fatal("browser chat accepted")
	}
}

// With keep_prompt_text off, no prompt text or command reaches the log and
// the chain still verifies; "new" is still decided, from the text, before
// it is dropped: the second prompt differs only in capitals and punctuation.
func TestNoTextModeKeepsNew(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := New(st, t.TempDir())
	p.NoText = true
	a, _ := p.EmitEvent(event.Event{Kind: event.Prompt, SessionID: "s", Text: "the login page still returns error 500 after submit"})
	b, _ := p.EmitEvent(event.Event{Kind: event.Prompt, SessionID: "s", Text: "The login page still returns error 500 after submit!"})
	c, _ := p.EmitEvent(event.Event{Kind: event.Tool, SessionID: "s", Tool: "Bash", Command: "cat .env"})
	for _, e := range []event.Event{a, b, c} {
		if (e.Text != "" && !strings.HasPrefix(e.Text, "sha256:")) || (e.Command != "" && !strings.HasPrefix(e.Command, "sha256:")) {
			t.Fatalf("text kept: %+v", e)
		}
	}
	if a.Words != 9 || a.New == nil || !*a.New || b.New == nil || *b.New {
		t.Fatalf("words %d, new %v then %v; want 9, true then false", a.Words, a.New, b.New)
	}
	if _, err := st.VerifyChain(); err != nil {
		t.Fatal(err)
	}
}

// TestRepeatSurvivesRestart: the same prompt before and after the store is
// closed and reopened (a daemon restart) is new once. "seatime forget"
// clears the set, and the prompt is new again.
func TestRepeatSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	emit := func(text string) bool {
		st, err := store.Open(filepath.Join(dir, "g.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		e, err := New(st, dir).EmitEvent(event.Event{Kind: event.Prompt, SessionID: "s", Project: "github.com/me/a", Text: text})
		if err != nil || e.New == nil {
			t.Fatal(err, e.New)
		}
		return *e.New
	}
	if !emit("add a dark mode toggle") || emit("Add a dark-mode toggle.") {
		t.Fatal("the same prompt after a restart was new again")
	}
	if emit("make it blue") {
		t.Fatal("3 words are not new")
	}
	st, _ := store.Open(filepath.Join(dir, "g.db"))
	if _, err := st.Forget(""); err != nil {
		t.Fatal(err)
	}
	st.Close()
	// forget deletes text, not the fingerprints: a prompt sent again after
	// it is still a repeat, so deleting text cannot make it count again
	if emit("add a dark mode toggle") {
		t.Fatal("a prompt sent before counted as new after forget")
	}
}
