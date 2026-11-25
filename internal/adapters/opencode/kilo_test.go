package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"seatime/internal/adapters/common"
)

func TestKiloVariant(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	k := NewKilo()
	if k.Name() != "kilo" || New().Name() != "opencode" {
		t.Fatal("names")
	}
	if k.Detect() {
		t.Fatal("nothing installed")
	}
	// the extension's session store alone proves Kilo is there
	os.MkdirAll(filepath.Join(home, ".local", "share", "kilo"), 0o755)
	if !k.Detect() || New().Detect() {
		t.Fatal("kilo must be detected without being confused with opencode")
	}
	if err := k.Install("/usr/local/bin/seatime"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, ".config", "kilo", "plugin", "seatime.js") // singular dir is Kilo's documented one
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"hook", "kilo"`) || strings.Contains(string(b), `"hook", "opencode"`) {
		t.Fatalf("plugin must call `seatime hook kilo`:\n%s", b)
	}
	if ok, _ := k.Installed(); !ok {
		t.Fatal("installed")
	}
	if ok, _ := New().Installed(); ok {
		t.Fatal("opencode must not see kilo's plugin")
	}
	var col common.Collector
	k.HandleHook("prompt", []byte(`{"session_id":"s1","cwd":"/w","text":"hello kilo"}`), &col)
	if got := col.Summary(); len(got) != 2 || !strings.HasPrefix(got[0], "prompt harness=kilo ") {
		t.Fatalf("%v", got)
	}
	if err := k.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("plugin left behind")
	}
}
