package copilot

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"seatime/internal/adapters/common"
)

func TestGolden(t *testing.T) {
	const b = "harness=copilot sid=cs-1 cwd=/work/app"
	cases := []struct {
		ev, fixture string
		want        []string
	}{
		{"userPromptSubmitted", "prompt.json", []string{"prompt " + b + " text=add retry logic to the fetcher", "agent_start " + b}},
		{"postToolUse", "tool_edit.json", []string{"tool " + b + " tool=edit files=/work/app/fetch.go write"}},
		{"postToolUse", "tool_bash.json", []string{
			"tool " + b + " tool=bash cmd=go test ./... exit=1", "run " + b + " cmd=go test ./... exit=1 run=test"}},
		{"agentStop", "agent_stop.json", []string{"agent_stop " + b}},
		// VS Code's PascalCase payload (event name taken from the payload when the arg is empty)
		{"", "vscode_prompt.json", []string{
			"prompt harness=copilot sid=vs-9 cwd=/work/vsapp text=why is this test flaky?", "agent_start harness=copilot sid=vs-9 cwd=/work/vsapp"}},
	}
	for _, c := range cases {
		var col common.Collector
		out, err := New().HandleHook(c.ev, common.Fixture(t, c.fixture), &col)
		if err != nil || len(out) != 0 {
			t.Fatalf("%s out=%q err=%v", c.fixture, out, err)
		}
		if got := col.Summary(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.fixture, got, c.want)
		}
	}
	var col common.Collector
	col.Events = nil
	New().HandleHook("userPromptSubmitted", common.Fixture(t, "prompt.json"), &col)
	if ts := col.Events[0].TS; ts.UnixMilli() != 1790000000000 {
		t.Fatalf("timestamp ms not honoured: %v", ts)
	}
}

func TestSubagentStopIgnoredAndGarbageSafe(t *testing.T) {
	var col common.Collector
	New().HandleHook("agentStop", []byte(`{"sessionId":"s","parentAgentId":"main","agentId":"x"}`), &col)
	New().HandleHook("userPromptSubmitted", []byte("\x00not json"), &col)
	for _, e := range col.Events {
		if e.Kind == "agent_stop" {
			t.Fatal("subagent stop recorded")
		}
	}
}

func TestInstallRoundTripOwnFile(t *testing.T) {
	common.HookRoundTrip(t, New(), ".copilot/hooks/seatime.json", "")
	// a user's own entry in the same file survives
	common.HookRoundTrip(t, New(), ".copilot/hooks/seatime.json", `{"version":1,"hooks":{"sessionStart":[{"type":"command","command":"./mine.sh"}]}}`)
}

func TestUninstallRemovesOurFileAndDetect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	a := New()
	if a.Detect() {
		t.Fatal("nothing installed yet")
	}
	// VS Code with Copilot Chat but no ~/.copilot is enough
	os.MkdirAll(filepath.Join(home, ".vscode", "extensions", "github.copilot-chat-0.40.0"), 0o755)
	if !a.Detect() {
		t.Fatal("VS Code Copilot Chat extension should count")
	}
	if err := a.Install("/usr/local/bin/seatime"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hooksPath()); err != nil {
		t.Fatal(err)
	}
	a.Uninstall()
	if _, err := os.Stat(hooksPath()); err == nil {
		t.Fatal("empty seatime.json should be deleted on uninstall")
	}
}
