package canary

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seatime/internal/client/store"
	"seatime/pkg/event"
)

var now = time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)

func line(typ string, ts time.Time, content string) string {
	return `{"type":"` + typ + `","timestamp":"` + ts.Format(time.RFC3339Nano) + `","sessionId":"abc","cwd":"/w/app","message":{"role":"` + typ + `","content":` + content + `}}`
}

func transcript(t *testing.T, home string, lines ...string) string {
	t.Helper()
	p := filepath.Join(home, ".claude", "projects", "-w-app", "abc.jsonl")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	os.Chtimes(p, now.Add(-time.Minute), now.Add(-time.Minute))
	return p
}

func TestClaudeBackfillTurns(t *testing.T) {
	home := t.TempDir()
	p := transcript(t, home,
		line("user", now.Add(-9*time.Minute), `"add a dark mode toggle"`),
		line("assistant", now.Add(-8*time.Minute), `[{"type":"text","text":"ok"}]`),
		line("user", now.Add(-8*time.Minute+time.Second), `[{"type":"tool_result","content":"done"}]`),
		line("assistant", now.Add(-7*time.Minute), `[{"type":"text","text":"done"}]`),
		`{"type":"summary","summary":"x"}`,
		`not json`,
		line("user", now.Add(-5*time.Minute), `[{"type":"text","text":"now make it persist"}]`),
		line("assistant", now.Add(-4*time.Minute), `"saved"`),
		line("user", now.Add(-3*time.Minute), `"<command-name>/clear</command-name>"`),
	)
	evs := ClaudeBackfill(p, now.Add(-time.Hour), now)
	var got []string
	for _, e := range evs {
		got = append(got, string(e.Kind)+"@"+e.TS.Format("15:04:05"))
		if e.Extra[ExtraBackfilled] != true || e.Harness != "claude-code" || e.SessionID != "abc" {
			t.Fatalf("unlabelled %+v", e)
		}
	}
	want := "prompt@14:51:00 agent_start@14:51:00 agent_stop@14:53:00 prompt@14:55:00 agent_start@14:55:00 agent_stop@14:56:00"
	if strings.Join(got, " ") != want {
		t.Fatalf("\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if evs[0].Text != "add a dark mode toggle" || evs[3].Text != "now make it persist" {
		t.Fatal(evs[0].Text, evs[3].Text)
	}
	if n := len(ClaudeBackfill(p, now.Add(-6*time.Minute), now)); n != 3 {
		t.Fatalf("window: %d", n)
	}
}

func TestCheckFindsSilentHarnessOnly(t *testing.T) {
	home := t.TempDir()
	transcript(t, home, line("user", now.Add(-2*time.Minute), `"hi"`))
	st, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := Check(st, home, now, Probes())
	if err != nil || len(s) != 1 || s[0].Harness != "claude-code" || !s[0].LastHook.IsZero() {
		t.Fatalf("%+v %v", s, err)
	}
	// a backfilled event does not count as hearing the hooks
	st.Append(event.Event{Kind: event.Prompt, Harness: "claude-code", TS: now.Add(-90 * time.Second), Extra: map[string]any{ExtraBackfilled: true}})
	if s, _ := Check(st, home, now, Probes()); len(s) != 1 {
		t.Fatal("backfill must not silence the canary")
	}
	// a real hook event close to the file change: heard
	st.Append(event.Event{Kind: event.AgentStop, Harness: "claude-code", TS: now.Add(-2 * time.Minute)})
	if s, _ := Check(st, home, now, Probes()); len(s) != 0 {
		t.Fatalf("hooks are heard: %+v", s)
	}
	// session files untouched for longer than the window: not in use
	home2 := t.TempDir()
	p := transcript(t, home2, line("user", now.Add(-time.Hour), `"x"`))
	os.Chtimes(p, now.Add(-time.Hour), now.Add(-time.Hour))
	if s, _ := Check(st, home2, now, Probes()); len(s) != 0 {
		t.Fatal("idle harness is not silent")
	}
}
