package windsurf

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"seatime/internal/adapters/common"
)

func TestGolden(t *testing.T) {
	const b = "harness=windsurf sid=tr-1 model=swe-1.5"
	cases := []struct {
		ev, fixture string
		want        []string
	}{
		{"pre_user_prompt", "pre_user_prompt.json", []string{"prompt " + b + " text=make the sidebar collapsible", "agent_start " + b}},
		{"post_write_code", "post_write_code.json", []string{"tool " + b + " tool=write_code files=/work/site/src/Sidebar.tsx write"}},
		{"post_run_command", "post_run_command.json", []string{
			"tool harness=windsurf sid=tr-1 cwd=/work/site model=swe-1.5 tool=run_command cmd=npm test",
			"run harness=windsurf sid=tr-1 cwd=/work/site model=swe-1.5 cmd=npm test run=test"}},
		{"post_cascade_response", "post_cascade_response.json", []string{"agent_stop " + b}},
	}
	payloads := map[string][]byte{}
	for _, c := range cases {
		payloads[c.fixture] = common.Fixture(t, c.fixture)
	}
	dir := t.TempDir()
	t.Chdir(dir) // hook cwd fallback: a temp dir that is not $HOME
	for _, c := range cases {
		var col common.Collector
		out, err := New().HandleHook(c.ev, payloads[c.fixture], &col)
		if err != nil || len(out) != 0 {
			t.Fatalf("%s out=%q err=%v", c.fixture, out, err)
		}
		got := col.Summary()
		for i := range got { // the only cwd-less payloads fall back to the hook's cwd
			got[i] = strings.Replace(got[i], " cwd="+dir, "", 1)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.fixture, got, c.want)
		}
	}
}

func TestEventNameFromPayload(t *testing.T) {
	var col common.Collector
	New().HandleHook("", common.Fixture(t, "post_cascade_response.json"), &col)
	if len(col.Events) != 1 || col.Events[0].Kind != "agent_stop" {
		t.Fatalf("%v", col.Summary())
	}
}

func TestInstallRoundTrip(t *testing.T) {
	initial := `{"hooks":{"pre_user_prompt":[{"command":"./audit.sh","show_output":true}]},"other":1}`
	common.HookRoundTrip(t, New(), ".codeium/windsurf/hooks.json", initial)
	common.HookRoundTrip(t, New(), ".codeium/windsurf/hooks.json", "")
}

func TestEntriesHaveNoTypeKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	if err := New().Install("/usr/local/bin/seatime"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(hooksPath())
	if strings.Contains(string(b), `"type"`) || !strings.Contains(string(b), `"show_output": false`) {
		t.Fatalf("windsurf entry shape:\n%s", b)
	}
}
