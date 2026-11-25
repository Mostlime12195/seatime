package opencode

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"seatime/internal/adapters/common"
)

func TestGolden(t *testing.T) {
	const b = "harness=opencode sid=ses_1 cwd=/work/game"
	cases := []struct {
		ev, fixture string
		want        []string
	}{
		{"prompt", "prompt.json", []string{"prompt " + b + " model=anthropic/claude-sonnet-5-5 text=add a pause menu", "agent_start " + b + " model=anthropic/claude-sonnet-5-5"}},
		{"tool", "tool_bash.json", []string{"tool " + b + " tool=bash cmd=cargo test exit=101", "run " + b + " cmd=cargo test exit=101 run=test"}},
		{"tool", "tool_edit.json", []string{"tool " + b + " tool=edit files=/work/game/src/pause.rs write"}},
		{"agent_stop", "idle.json", []string{"agent_stop " + b}},
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

func TestInstallUninstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	dir := filepath.Join(home, ".config", "opencode", "plugins")
	os.MkdirAll(dir, 0o755)
	mine := filepath.Join(dir, "mine.js")
	os.WriteFile(mine, []byte("export default {}"), 0o644)
	a := New()
	if err := a.Install(`C:\Program Files\seatime\seatime.exe`); err != nil {
		t.Fatal(err)
	}
	if err := a.Install(`C:\Program Files\seatime\seatime.exe`); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "seatime.js"))
	if err != nil || !strings.Contains(string(b), `"C:\\Program Files\\seatime\\seatime.exe"`) || strings.Contains(string(b), "__SEATIME_BIN__") {
		t.Fatalf("bad plugin: %v\n%s", err, b)
	}
	if ok, _ := a.Installed(); !ok {
		t.Fatal("not installed")
	}
	if err := a.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "seatime.js")); err == nil {
		t.Fatal("plugin left behind")
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("user plugin removed")
	}
}

func TestRefusesForeignFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	dir := filepath.Join(home, ".config", "opencode", "plugins")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "seatime.js"), []byte("// not ours"), 0o644)
	if err := New().Install("/bin/seatime"); err == nil {
		t.Fatal("expected refusal")
	}
	if err := New().Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "seatime.js")); err != nil {
		t.Fatal("foreign file removed")
	}
}
