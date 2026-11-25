package antigravity

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"seatime/internal/adapters/common"
)

func run(t *testing.T, ev, fixture string) ([]string, string) {
	t.Helper()
	var col common.Collector
	out, err := New().HandleHook(ev, common.Fixture(t, fixture), &col)
	if err != nil {
		t.Fatal(err)
	}
	return col.Summary(), string(out)
}

func TestGoldenAndPromptDedupe(t *testing.T) {
	t.Setenv("SEATIME_HOME", t.TempDir())
	const b = "harness=antigravity sid=cv-7 cwd=/work/api model=gemini-3-pro"
	steps := []struct {
		ev, fixture string
		want        []string
	}{
		{"PreInvocation", "pre_invocation_1.json", []string{"prompt " + b + " text=add pagination to /users", "agent_start " + b}},
		// later model calls of the same turn carry the same lastUserInput: no second prompt
		{"PreInvocation", "pre_invocation_4_same.json", nil},
		// a different user message is a new prompt even if the counter did not reset
		{"PreInvocation", "pre_invocation_5_new.json", []string{"prompt " + b + " text=now add a cursor option", "agent_start " + b}},
		{"Stop", "stop_busy.json", nil},
		{"Stop", "stop_idle.json", []string{"agent_stop " + b}},
	}
	for _, s := range steps {
		got, out := run(t, s.ev, s.fixture)
		if out != "{}" {
			t.Fatalf("%s must answer {} for Antigravity's JSON-only stdout, got %q", s.ev, out)
		}
		if !reflect.DeepEqual(got, s.want) {
			t.Errorf("%s %s:\n got %q\nwant %q", s.ev, s.fixture, got, s.want)
		}
	}
}

func TestAlwaysAnswersOnGarbage(t *testing.T) {
	t.Setenv("SEATIME_HOME", t.TempDir())
	var col common.Collector
	out, _ := New().HandleHook("Stop", []byte("\x00"), &col)
	if string(out) != "{}" {
		t.Fatalf("%q", out)
	}
}

func TestInstallRoundTripAndForeignGroup(t *testing.T) {
	initial := `{"audit":{"Stop":[{"type":"command","command":"./audit.sh"}]}}`
	common.HookRoundTrip(t, New(), ".gemini/config/hooks.json", initial)
	common.HookRoundTrip(t, New(), ".gemini/config/hooks.json", "")

	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	p := filepath.Join(home, ".gemini", "config", "hooks.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	foreign := `{"seatime":{"Stop":[{"type":"command","command":"./mine.sh"}]}}`
	os.WriteFile(p, []byte(foreign), 0o644)
	if err := New().Install("/usr/local/bin/seatime"); err == nil {
		t.Fatal("must not overwrite somebody else's group named seatime")
	}
	if err := New().Uninstall(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != foreign {
		t.Fatalf("uninstall touched a foreign group: %s", b)
	}
}

func TestDetect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	if New().Detect() {
		t.Fatal("empty home")
	}
	os.MkdirAll(filepath.Join(home, ".gemini", "antigravity-ide"), 0o755)
	if !New().Detect() {
		t.Fatal("antigravity-ide dir")
	}
}
