package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"seatime/internal/adapters/common"
)

func run(t *testing.T, ev, fixture string) []string {
	t.Helper()
	var c common.Collector
	out, err := New().HandleHook(ev, common.Fixture(t, fixture), &c)
	if err != nil || out != nil {
		t.Fatalf("out=%q err=%v", out, err)
	}
	return c.Summary()
}

func TestGolden(t *testing.T) {
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	cases := []struct {
		ev, fixture string
		want        []string
	}{
		{"SessionStart", "session_start.json", []string{"session_start harness=claude-code sid=s1 cwd=/work/app model=claude-sonnet-5-5"}},
		{"UserPromptSubmit", "user_prompt_submit.json", []string{
			"prompt harness=claude-code sid=s1 cwd=/work/app text=fix the failing login test",
			"agent_start harness=claude-code sid=s1 cwd=/work/app"}},
		// a background task reporting back: the tool's own message, so no prompt
		{"UserPromptSubmit", "task_notification.json", []string{"agent_start harness=claude-code sid=s1 cwd=/work/app"}},
		{"PostToolUse", "post_edit.json", []string{"tool harness=claude-code sid=s1 cwd=/work/app tool=Edit files=/work/app/src/login.ts write"}},
		{"PostToolUse", "post_bash_test.json", []string{
			"tool harness=claude-code sid=s1 cwd=/work/app tool=Bash cmd=cd app && npm test exit=0",
			"run harness=claude-code sid=s1 cwd=/work/app cmd=cd app && npm test exit=0 run=test"}},
		{"PostToolUseFailure", "post_bash_fail.json", []string{
			"tool harness=claude-code sid=s1 cwd=/work/app tool=Bash cmd=go test ./... exit=1",
			"run harness=claude-code sid=s1 cwd=/work/app cmd=go test ./... exit=1 run=test"}},
		{"Stop", "stop.json", []string{"agent_stop harness=claude-code sid=s1 cwd=/work/app"}},
		{"SessionEnd", "session_end.json", []string{"session_end harness=claude-code sid=s1 cwd=/work/app"}},
	}
	for _, c := range cases {
		if got := run(t, c.ev, c.fixture); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.fixture, got, c.want)
		}
	}
}

func TestHeadlessDropped(t *testing.T) {
	for _, env := range [][2]string{{"CLAUDE_CODE_ENTRYPOINT", "sdk-cli"}, {"CLAUDE_CODE_SESSION_ATTENDED", "0"}, {"SEATIME_IGNORE", "1"}} {
		t.Setenv(env[0], env[1])
		if got := run(t, "UserPromptSubmit", "user_prompt_submit.json"); len(got) != 0 {
			t.Errorf("%v: expected drop, got %q", env, got)
		}
		t.Setenv(env[0], "")
	}
}

func TestGarbageStdin(t *testing.T) {
	var c common.Collector
	if _, err := New().HandleHook("PostToolUse", []byte("not json"), &c); err != nil {
		t.Fatal(err)
	}
}

const existing = `{
  "model": "opus",
  "permissions": {"allow": ["Bash(ls)"]},
  "hooks": {
    "PostToolUse": [{"matcher": "Edit", "hooks": [{"type": "command", "command": "prettier --write"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "notify-send done"}]}]
  }
}`

func TestInstallRoundTrip(t *testing.T) {
	common.HookRoundTrip(t, New(), ".claude/settings.json", existing)
	common.HookRoundTrip(t, New(), ".claude/settings.json", "")
}

// A Stop hook that is not SeaTime's can re-prompt the agent: it is noted.
// SeaTime's own and none at all are not.
func TestOtherStopHookIn(t *testing.T) {
	ours := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/home/u/.local/bin/seatime hook claude-code Stop"}]}]}}`
	theirs := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/home/u/.local/bin/seatime hook claude-code Stop"},{"type":"command","command":"./keep-going.sh"}]}]}}`
	sub := `{"hooks":{"SubagentStop":[{"hooks":[{"type":"prompt","prompt":"continue"}]}]}}`
	for s, want := range map[string]bool{ours: false, theirs: true, sub: true, `{}`: false, `not json`: false} {
		if got := OtherStopHookIn([]byte(s)); got != want {
			t.Errorf("%s: %v, want %v", s, got, want)
		}
	}
}

// Lines a write tool call wrote: a 3-line file; an edit's 2-line
// replacement; a multi-edit of 1 + 2 lines.
func TestLinesWritten(t *testing.T) {
	for in, want := range map[string]int{
		`{"file_path":"/p/a.go","content":"a\nb\nc\n"}`:                              3,
		`{"file_path":"/p/a.go","old_string":"x","new_string":"y\nz"}`:               2,
		`{"file_path":"/p/a.go","edits":[{"new_string":"q"},{"new_string":"r\ns"}]}`: 3,
	} {
		var m map[string]any
		json.Unmarshal([]byte(in), &m)
		if got := linesWritten(m); got != want {
			t.Errorf("%s: %d, want %d", in, got, want)
		}
	}
}

func TestModelInLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	log := `{"type":"user","sessionId":"s1","message":{"role":"user","content":"hi"}}
{"type":"assistant","sessionId":"s1","message":{"model":"claude-opus-5","role":"assistant"}}
{"type":"assistant","sessionId":"s1","message":{"model":"claude-sonnet-5-5","role":"assistant"}}
{"type":"assistant","sessionId":"s1","isSidechain":true,"message":{"model":"claude-haiku-5","role":"assistant"}}
{"type":"assistant","sessionId":"s1","message":{"model":"<synthetic>","role":"assistant"}}
{"type":"assistant","sessionId":"other","message":{"model":"claude-fable-5","role":"assistant"}}
{"type":"user","sessionId":"s1","message":{"role":"user","content":"the word \"assistant\" in a prompt"}}
`
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ModelInLog(path, "s1"); got != "claude-sonnet-5-5" {
		t.Fatalf("model %q, want the last assistant entry of the session", got)
	}
	if got := ModelInLog(filepath.Join(t.TempDir(), "none.jsonl"), "s1"); got != "" {
		t.Fatalf("no log: %q", got)
	}
	// a stop and a tool call take it; a prompt does not (the log is one turn behind)
	var sink common.Collector
	for ev, want := range map[string]string{"Stop": "claude-sonnet-5-5", "PostToolUse": "claude-sonnet-5-5", "UserPromptSubmit": ""} {
		sink.Events = nil
		in := fmt.Sprintf(`{"session_id":"s1","cwd":"/work/app","transcript_path":%q,"hook_event_name":%q,"tool_name":"Read","prompt":"p"}`, path, ev)
		if err := handle(ev, []byte(in), &sink); err != nil || len(sink.Events) == 0 || sink.Events[0].Model != want {
			t.Fatalf("%s: %+v %v, want model %q", ev, sink.Events, err, want)
		}
	}
}
