package common

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"seatime/pkg/event"
)

// Collector is a Sink that records events (for tests).
type Collector struct{ Events []event.Event }

func (c *Collector) Emit(e event.Event) error { c.Events = append(c.Events, e); return nil }

// Summary renders events compactly for golden comparisons.
func (c *Collector) Summary() []string {
	var out []string
	for _, e := range c.Events {
		s := string(e.Kind)
		add := func(k, v string) {
			if v != "" {
				s += " " + k + "=" + v
			}
		}
		add("harness", e.Harness)
		add("sid", e.SessionID)
		add("cwd", e.CWD)
		add("model", e.Model)
		add("text", e.Text)
		add("tool", e.Tool)
		add("files", strings.Join(e.Files, ","))
		if e.Write {
			s += " write"
		}
		add("cmd", e.Command)
		if e.ExitCode != nil {
			s += fmt.Sprintf(" exit=%d", *e.ExitCode)
		}
		add("run", e.RunKind)
		out = append(out, s)
	}
	return out
}

// Fixture reads testdata/<name> relative to the calling package.
func Fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// HookRoundTrip checks Install/Uninstall for a JSON-config adapter: backup,
// idempotency, preservation of foreign config, exact removal.
func HookRoundTrip(t *testing.T, a Adapter, relPath, initial string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	path := filepath.Join(home, relPath)
	if initial != "" {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := a.Installed(); err != nil || ok {
		t.Fatalf("Installed before install = %v,%v", ok, err)
	}
	bin := `C:\Program Files\seatime\seatime.exe`
	if err := a.Install(bin); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := a.Install(bin); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatalf("install not idempotent:\n%s\n---\n%s", first, second)
	}
	if ok, err := a.Installed(); err != nil || !ok {
		t.Fatalf("Installed after install = %v,%v", ok, err)
	}
	if !strings.Contains(string(first), "hook "+a.Name()) {
		t.Fatalf("no hook command in config:\n%s", first)
	}
	if initial != "" {
		b, err := os.ReadFile(BackupPath(path))
		if err != nil {
			t.Fatal("no backup written")
		}
		if string(b) != initial {
			t.Fatal("backup differs from original")
		}
	} else if _, err := os.Stat(AbsentPath(path)); err != nil {
		t.Fatal("no marker that SeaTime created the file")
	}
	// installing with a new binary path replaces the old entries
	if err := a.Install("/usr/local/bin/seatime"); err != nil {
		t.Fatal(err)
	}
	third, _ := os.ReadFile(path)
	if strings.Contains(string(third), "Program Files") {
		t.Fatal("stale SeaTime entry left after reinstall")
	}
	if err := a.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.Installed(); ok {
		t.Fatal("still installed after uninstall")
	}
	after, err := os.ReadFile(path)
	for _, p := range []string{BackupPath(path), AbsentPath(path)} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("left behind after a clean uninstall: %s", p)
		}
	}
	if baks, _ := filepath.Glob(path + ".seatime-*"); len(baks) > 0 {
		t.Fatalf("left behind: %v", baks)
	}
	if initial == "" {
		if err == nil {
			t.Fatalf("seatime created this file and must remove it: %s", after)
		}
		return
	}
	if string(after) != initial {
		t.Fatalf("uninstall must restore the user's config byte for byte:\nwant %q\ngot  %q", initial, after)
	}
	var want, got any
	json.Unmarshal([]byte(initial), &want)
	if err := json.Unmarshal(after, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("uninstall did not restore user config:\nwant %s\ngot  %s", initial, after)
	}
}
