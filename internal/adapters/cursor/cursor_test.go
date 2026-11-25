package cursor

import (
	"reflect"
	"testing"

	"seatime/internal/adapters/common"
)

func TestGolden(t *testing.T) {
	const b = "harness=cursor sid=cv1 cwd=/work/site model=claude-sonnet-5-5"
	cases := []struct {
		ev, fixture, stdout string
		want                []string
	}{
		{"beforeSubmitPrompt", "before_submit_prompt.json", `{"continue":true}`, []string{
			"prompt " + b + " text=make the navbar sticky", "agent_start " + b}},
		{"afterFileEdit", "after_file_edit.json", "", []string{"tool " + b + " tool=edit files=/work/site/src/Nav.tsx write"}},
		{"afterShellExecution", "after_shell.json", "", []string{
			"tool " + b + " tool=shell cmd=npm run build", "run " + b + " cmd=npm run build run=build"}},
		{"stop", "stop.json", `{}`, []string{"agent_stop " + b}},
	}
	for _, c := range cases {
		var col common.Collector
		out, err := New().HandleHook(c.ev, common.Fixture(t, c.fixture), &col)
		if err != nil || string(out) != c.stdout {
			t.Fatalf("%s out=%q err=%v", c.ev, out, err)
		}
		if got := col.Summary(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.fixture, got, c.want)
		}
	}
}

func TestAlwaysAnswersEvenOnGarbage(t *testing.T) {
	var col common.Collector
	out, _ := New().HandleHook("beforeSubmitPrompt", []byte("\x00garbage"), &col)
	if string(out) != `{"continue":true}` {
		t.Fatalf("out=%q", out)
	}
}

func TestInstallRoundTrip(t *testing.T) {
	common.HookRoundTrip(t, New(), ".cursor/hooks.json", `{"version":1,"hooks":{"stop":[{"command":"./mine.sh"}]},"extra":true}`)
	common.HookRoundTrip(t, New(), ".cursor/hooks.json", "")
}
