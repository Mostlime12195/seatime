package codex

import (
	"reflect"
	"testing"

	"seatime/internal/adapters/common"
)

func TestGolden(t *testing.T) {
	const b = "harness=codex sid=c1 cwd=/work/api"
	cases := []struct {
		ev, fixture string
		want        []string
	}{
		{"SessionStart", "session_start.json", []string{"session_start " + b + " model=gpt-5.2-codex"}},
		{"UserPromptSubmit", "user_prompt_submit.json", []string{
			"prompt " + b + " model=gpt-5.2-codex text=add pagination to /users",
			"agent_start " + b + " model=gpt-5.2-codex"}},
		{"PostToolUse", "post_patch.json", []string{"tool " + b + " model=gpt-5.2-codex tool=apply_patch files=src/users.py,tests/test_users.py write"}},
		{"PostToolUse", "post_shell.json", []string{
			"tool " + b + " model=gpt-5.2-codex tool=shell cmd=pytest -x exit=1",
			"run " + b + " model=gpt-5.2-codex cmd=pytest -x exit=1 run=test"}},
		{"Stop", "stop.json", []string{"agent_stop " + b + " model=gpt-5.2-codex"}},
	}
	for _, c := range cases {
		var col common.Collector
		out, err := New().HandleHook(c.ev, common.Fixture(t, c.fixture), &col)
		if err != nil || out != nil {
			t.Fatal(out, err)
		}
		if got := col.Summary(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.fixture, got, c.want)
		}
	}
}

func TestInstallRoundTrip(t *testing.T) {
	common.HookRoundTrip(t, New(), ".codex/hooks.json", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]},"x":1}`)
	common.HookRoundTrip(t, New(), ".codex/hooks.json", "")
}
