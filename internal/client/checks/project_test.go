package checks_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seatime/internal/client/checks"
	"seatime/pkg/event"
	"seatime/pkg/promptcheck"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Facts of a folder without git: file names by a short walk, without
// hidden, vendored and build folders, and the README's first lines with
// secrets blanked. No other file's contents, and no path above the folder.
func TestFactsOfPlainFolder(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "code", "zorblax")
	write(t, root, "README.md", "# Zorblax\nA counter app.\nAPI_KEY=sk-live-0123456789abcdef0123456789\n"+strings.Repeat("more words\n", 600))
	write(t, root, "src/main.py", "CONTENT-CANARY = 1\n")
	write(t, root, "src/deep/a/b/c/too_deep.py", "")
	write(t, root, "node_modules/left-pad/index.js", "")
	write(t, root, ".venv/lib/site.py", "")
	write(t, root, "build/out.bin", "")
	p := checks.Facts(context.Background(), root, "zorblax", home)
	if p.Name != "zorblax" || p.Files != "README.md\nsrc/main.py" || p.Total != 2 {
		t.Fatalf("files %q (%d)", p.Files, p.Total)
	}
	if !strings.HasPrefix(p.Readme, "# Zorblax\nA counter app.") || len(p.Readme) > promptcheck.MaxReadme || strings.Contains(p.Readme, "0123456789abcdef") {
		t.Fatalf("readme %q", p.Readme)
	}
	if all := p.Name + p.Files + p.Readme; strings.Contains(all, "CANARY") || strings.Contains(all, home) || strings.Contains(all, "code/") {
		t.Fatalf("facts carry a file's contents or a path above the folder: %+v", p)
	}
	// a new, empty folder: its name and nothing else
	empty := filepath.Join(home, "new")
	os.MkdirAll(empty, 0o755)
	if p := checks.Facts(context.Background(), empty, "new", home); p.Name != "new" || p.Files != "" || p.Total != 0 || p.Readme != "" {
		t.Fatalf("empty folder: %+v", p)
	}
	// the home folder is no project: nothing in it is listed or read
	write(t, home, "README.md", "HOME-CANARY")
	if p := checks.Facts(context.Background(), home, "~", home); p.Files != "" || p.Readme != "" {
		t.Fatalf("the home folder was read: %+v", p)
	}
	// a README that is a link is not followed
	linked := filepath.Join(home, "linked")
	os.MkdirAll(linked, 0o755)
	write(t, home, "secret.txt", "LINK-CANARY")
	if err := os.Symlink(filepath.Join(home, "secret.txt"), filepath.Join(linked, "README.md")); err == nil {
		if p := checks.Facts(context.Background(), linked, "linked", home); p.Readme != "" {
			t.Fatalf("a linked README was read: %q", p.Readme)
		}
	}
}

// In a git checkout the list is git's: tracked and new files, never the
// ignored ones.
func TestFactsOfGitCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := filepath.Join(t.TempDir(), "zorblax")
	write(t, root, "README", "Zorblax counts things.\n")
	write(t, root, "main.go", "package main\n")
	write(t, root, "new_untracked.go", "package main\n")
	write(t, root, ".gitignore", "secrets/\n*.log\n")
	write(t, root, "secrets/prod.env", "TOKEN=IGNORED-CANARY\n")
	write(t, root, "debug.log", "")
	write(t, root, "vendor/dep/dep.go", "")
	for _, args := range [][]string{{"init", "-q"}, {"add", "README", "main.go", ".gitignore", "vendor"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	p := checks.Facts(context.Background(), root, "zorblax", "")
	if p.Files != ".gitignore\nREADME\nmain.go\nnew_untracked.go" || p.Total != 4 || p.Readme != "Zorblax counts things." {
		t.Fatalf("git facts: %+v", p)
	}
}

// The facts are read when the prompt is sent, kept with its turn and sent
// with its check; a prompt recorded without them (the daemon, an older
// SeaTime) is sent without.
func TestCheckCarriesProjectFacts(t *testing.T) {
	f := newFixture(t)
	read := 0
	f.p.Facts = func(root, name string) *promptcheck.Project {
		read++
		if root != f.proj || name != "zorblax" {
			t.Errorf("facts read for %q %q", root, name)
		}
		return &promptcheck.Project{Name: name, Files: "README.md\nmain.go", Total: 2, Readme: "Zorblax counts things."}
	}
	f.emit(t, 0, event.Event{Kind: event.Prompt, Text: prompt1})
	f.emit(t, time.Minute, event.Event{Kind: event.AgentStop})
	f.p.Facts = nil
	f.emit(t, 5*time.Minute, event.Event{Kind: event.Prompt, Text: "now make the counter count down as well"})
	f.emit(t, 6*time.Minute, event.Event{Kind: event.AgentStop})
	if read != 1 {
		t.Fatalf("facts read %d times, want once, at the prompt", read)
	}
	srv := &fakeServer{}
	if n, err := checks.Drain(context.Background(), f.st, "dev1", srv.send, f.t0.Add(20*time.Minute), 20); err != nil || n != 2 {
		t.Fatalf("sent %d: %v", n, err)
	}
	if p := srv.reqs[0].Project; p == nil || p.Name != "zorblax" || p.Files != "README.md\nmain.go" || p.Total != 2 || p.Readme != "Zorblax counts things." {
		t.Fatalf("request 1 project: %+v", p)
	}
	if srv.reqs[1].Project != nil {
		t.Fatalf("request 2 carries facts nobody read: %+v", srv.reqs[1].Project)
	}
}
