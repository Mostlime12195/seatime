package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"seatime/internal/adapters/common"
	"seatime/pkg/event"
)

// A prompt typed while the agent ran is held by Claude Code and sent when
// the turn ends. The session log has the typing as an enqueue line; the
// adapter reads its time (testdata/queued_transcript.jsonl).
func TestTypedAt(t *testing.T) {
	path := filepath.Join("testdata", "queued_transcript.jsonl")
	at := func(s string) time.Time { x, _ := time.Parse(time.RFC3339Nano, s); return x.UTC() }
	cases := []struct {
		session, prompt string
		want            time.Time
	}{
		// sent at once: enqueued milliseconds before it went out
		{"s1", "add a dark mode toggle to the settings page", at("2026-11-20T10:00:00.010Z")},
		// typed while the agent ran
		{"s1", "also keep the old colours behind a flag", at("2026-11-20T10:01:40.500Z")},
		// two queued messages sent as one prompt: the first one's time
		{"s1", "then run the tests\nand commit", at("2026-11-20T10:02:30.000Z")},
		// another session's line does not count
		{"s1", "fix the failing login test", time.Time{}},
		{"other", "fix the failing login test", at("2026-11-20T10:01:41.000Z")},
		// not in the log
		{"s1", "something else", time.Time{}},
	}
	for _, c := range cases {
		got, ok := TypedAt(path, c.session, c.prompt)
		if ok != !c.want.IsZero() || !got.Equal(c.want) {
			t.Errorf("%s %q: got %v %v, want %v", c.session, c.prompt, got, ok, c.want)
		}
	}
	if _, ok := TypedAt(filepath.Join(t.TempDir(), "missing.jsonl"), "s1", "x"); ok {
		t.Error("a missing log gave a time")
	}
}

// The hook passes the typed time on to the pipeline in Extra; the text and
// the agent start are unchanged.
func TestQueuedPromptHook(t *testing.T) {
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	log, _ := filepath.Abs(filepath.Join("testdata", "queued_transcript.jsonl"))
	payload := []byte(`{"session_id":"s1","cwd":"/work/app","hook_event_name":"UserPromptSubmit","transcript_path":` + quote(log) + `,"prompt":"also keep the old colours behind a flag"}`)
	var c common.Collector
	if _, err := New().HandleHook("UserPromptSubmit", payload, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Events) != 2 || c.Events[0].Kind != event.Prompt || c.Events[1].Kind != event.AgentStart {
		t.Fatalf("events: %q", c.Summary())
	}
	if got := c.Events[0].Extra[event.TypedAtKey]; got != "2026-11-20T10:01:40.5Z" {
		t.Errorf("typed_at = %v", got)
	}
	// a log without the line: nothing added
	empty := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(empty, []byte("{}\n"), 0o600)
	c = common.Collector{}
	payload = []byte(`{"session_id":"s1","hook_event_name":"UserPromptSubmit","transcript_path":` + quote(empty) + `,"prompt":"hello there agent"}`)
	New().HandleHook("UserPromptSubmit", payload, &c)
	if len(c.Events) == 0 || c.Events[0].Extra != nil {
		t.Errorf("extra on an unqueued prompt: %v", c.Events)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
