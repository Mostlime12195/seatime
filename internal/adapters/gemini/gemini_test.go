package gemini

import (
	"reflect"
	"testing"

	"seatime/internal/adapters/common"
)

func TestGolden(t *testing.T) {
	const b = "harness=gemini-cli sid=g1 cwd=/work/cli"
	cases := []struct {
		ev, fixture string
		want        []string
	}{
		{"BeforeAgent", "before_agent.json", []string{"prompt " + b + " text=why does the parser panic on empty input?", "agent_start " + b}},
		{"AfterTool", "after_shell.json", []string{"tool " + b + " tool=run_shell_command cmd=go test ./... exit=1", "run " + b + " cmd=go test ./... exit=1 run=test"}},
		{"AfterTool", "after_edit.json", []string{"tool " + b + " tool=replace files=/work/cli/parse.go write"}},
		{"AfterAgent", "after_agent.json", []string{"agent_stop " + b}},
	}
	for _, c := range cases {
		var col common.Collector
		out, err := New().HandleHook(c.ev, common.Fixture(t, c.fixture), &col)
		if err != nil || string(out) != "{}" {
			t.Fatalf("out=%q err=%v", out, err)
		}
		if got := col.Summary(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.fixture, got, c.want)
		}
	}
}

func TestTimestampParsed(t *testing.T) {
	var col common.Collector
	New().HandleHook("BeforeAgent", common.Fixture(t, "before_agent.json"), &col)
	if col.Events[0].TS.IsZero() {
		t.Fatal("TS not parsed")
	}
}

func TestInstallRoundTrip(t *testing.T) {
	common.HookRoundTrip(t, New(), ".gemini/settings.json", `{"theme":"dark","mcpServers":{"a":{"command":"x"}},"hooks":{"AfterTool":[{"matcher":"write_file","hooks":[{"type":"command","command":"fmt.sh"}]}]}}`)
	common.HookRoundTrip(t, New(), ".gemini/settings.json", "")
}
