package common

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsSeatimeCommand(t *testing.T) {
	yes := []string{
		"seatime hook cursor stop",
		`"C:\Program Files\seatime\seatime.exe" hook cursor stop`,
		`& "C:\Program Files\seatime\seatime.exe" hook cursor stop`,
		"/usr/local/bin/seatime hook cursor beforeSubmitPrompt",
		"/Users/kid/Downloads/seatime-darwin-arm64 hook cursor stop", // run under its download name
	}
	no := []string{"seatime hook codex stop", "./mine.sh", "notseatime hook cursor stop", "seatime status", `"unterminated`,
		"seatime-tools hook cursor stop"}
	for _, c := range yes {
		if !IsSeatimeCommand(c, "cursor") {
			t.Errorf("want seatime: %q", c)
		}
	}
	for _, c := range no {
		if IsSeatimeCommand(c, "cursor") {
			t.Errorf("want not seatime: %q", c)
		}
	}
}

func TestQuote(t *testing.T) {
	if Quote("/usr/bin/seatime") != "/usr/bin/seatime" || Quote(`C:\a b\seatime.exe`) != `"C:\a b\seatime.exe"` {
		t.Fatal("quote")
	}
}

func TestInstallRefusesInvalidJSON(t *testing.T) {
	t.Setenv("SEATIME_FAKE_HOME", t.TempDir())
	if _, err := ReadJSONObject("/nonexistent/x.json"); err != nil {
		t.Fatal(err)
	}
}

// A hook config SeaTime cannot parse is never rewritten, with or without trailing junk.
func TestUnparseableConfigIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	for i, body := range []string{"{\"hooks\": {", "// comment\n{}", "{} trailing", "[1,2]"} {
		p := filepath.Join(dir, "c"+string(rune('a'+i))+".json")
		os.WriteFile(p, []byte(body), 0o644)
		err := InstallHooks(p, "cursor", Flat, []HookSpec{{Event: "stop"}}, func(string) string { return "seatime hook cursor stop" }, nil)
		if err == nil {
			t.Errorf("%q: want an error", body)
		}
		if b, _ := os.ReadFile(p); string(b) != body {
			t.Errorf("%q was modified to %q", body, b)
		}
		if _, err := os.Stat(BackupPath(p)); err == nil {
			t.Errorf("%q: no backup for a file we did not touch", body)
		}
	}
}

// Installing twice, or into a config that already has our entry, writes nothing.
func TestNoWriteWhenNothingChanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hooks.json")
	cmd := func(string) string { return "seatime hook cursor stop" }
	specs := []HookSpec{{Event: "stop"}}
	if err := InstallHooks(p, "cursor", Flat, specs, cmd, nil); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	os.Chtimes(p, old, old)
	if err := InstallHooks(p, "cursor", Flat, specs, cmd, nil); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); !st.ModTime().Equal(old) {
		t.Fatal("an unchanged config must not be rewritten")
	}
}

// If the user edited the config after install, uninstall keeps their edit.
func TestUninstallKeepsLaterUserEdits(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(`{"a": 1}`), 0o644)
	cmd := func(string) string { return "seatime hook claude-code Stop" }
	if err := InstallHooks(p, "claude-code", MatcherGroups, []HookSpec{{Event: "Stop"}}, cmd, nil); err != nil {
		t.Fatal(err)
	}
	m, _ := ReadJSONObject(p)
	m["b"] = "mine"
	WriteJSONObject(p, m, 0o644)
	if err := UninstallHooks(p, "claude-code", MatcherGroups); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadJSONObject(p)
	if got["b"] != "mine" || got["hooks"] != nil {
		t.Fatalf("%v", got)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o644 {
		t.Fatalf("permissions changed to %v", st.Mode().Perm())
	}
}
