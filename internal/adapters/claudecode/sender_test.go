package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"seatime/internal/adapters/common"
	"seatime/pkg/event"
)

// The log names a prompt's sender on the prompt's own line; other lines of
// the same prompt (a reminder before it, tool results after it) carry the
// id and no sender.
func TestSentBy(t *testing.T) {
	log := filepath.Join(t.TempDir(), "s.jsonl")
	lines := []string{
		`{"type":"user","promptId":"p-typed","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"add a retry to the fetch"}}`,
		`{"type":"assistant","message":{"model":"m","content":[]}}`,
		`{"type":"user","promptId":"p-typed","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"type":"user","promptId":"p-task","isMeta":true,"message":{"role":"user","content":"<system-reminder>x</system-reminder>"}}`,
		`{"type":"user","promptId":"p-task","origin":{"kind":"task-notification","runId":"r"},"promptSource":"system","message":{"role":"user","content":"<task-notification>done</task-notification>"}}`,
		`{"type":"user","promptId":"p-peer","origin":{"kind":"peer"},"promptSource":"system","message":{"role":"user","content":"a long handover from another session"}}`,
		`{"type":"user","promptId":"p-old","message":{"role":"user","content":"a prompt from a Claude Code that names no sender"}}`,
		`{"type":"queue-operation","operation":"enqueue","content":"p-queue is only text here"}`,
	}
	if err := os.WriteFile(log, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id, kind string
		seen     bool
	}{
		{"p-typed", "human", true},
		{"p-task", "task-notification", true},
		{"p-peer", "peer", true},
		{"p-old", "", true},
		{"p-queue", "", false},   // the id only inside another line's text
		{"p-not-yet", "", false}, // the hook runs before the log has the prompt
	} {
		if kind, seen := SentBy(log, c.id); kind != c.kind || seen != c.seen {
			t.Errorf("%s: got %q seen=%v, want %q seen=%v", c.id, kind, seen, c.kind, c.seen)
		}
	}
	if kind, seen := SentBy(filepath.Join(t.TempDir(), "none.jsonl"), "p-typed"); kind != "" || seen {
		t.Errorf("no log: %q %v", kind, seen)
	}
}

// A prompt whose hook names its id is marked to be held, with the agent
// start after it; one from an older Claude Code is not.
func TestPromptIsMarkedToBeHeld(t *testing.T) {
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	var c common.Collector
	in := `{"session_id":"s1","transcript_path":"/logs/s1.jsonl","cwd":"/work/app","hook_event_name":"UserPromptSubmit","prompt":"fix the failing login test","prompt_id":"p-1"}`
	if _, err := New().HandleHook("UserPromptSubmit", []byte(in), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Events) != 2 || c.Events[0].Kind != event.Prompt || c.Events[1].Kind != event.AgentStart {
		t.Fatalf("events: %+v", c.Events)
	}
	for _, e := range c.Events {
		m, _ := e.Extra[event.HoldKey].(map[string]any)
		if m["log"] != "/logs/s1.jsonl" || m["id"] != "p-1" {
			t.Errorf("%s: hold mark %v", e.Kind, e.Extra)
		}
	}
	c = common.Collector{}
	if _, err := New().HandleHook("UserPromptSubmit", common.Fixture(t, "user_prompt_submit.json"), &c); err != nil {
		t.Fatal(err)
	}
	for _, e := range c.Events {
		if _, has := e.Extra[event.HoldKey]; has {
			t.Errorf("%s from a hook with no prompt id is marked: %v", e.Kind, e.Extra)
		}
	}
}
