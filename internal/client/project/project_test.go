package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeRemote(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/User/Repo.git":        "github.com/user/repo",
		"https://tok:x-oauth@github.com/u/r":      "github.com/u/r",
		"git@github.com:u/r.git":                  "github.com/u/r",
		"ssh://git@gitlab.com:2222/grp/sub/r.git": "gitlab.com/grp/sub/r",
		"git://codeberg.org/u/r/":                 "codeberg.org/u/r",
		"/srv/git/r.git":                          "",
		"../r":                                    "",
		"file:///tmp/x":                           "",
		"":                                        "",
	} {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}

func mkRepo(t *testing.T, dir, origin string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	cfg := "[core]\n\tbare = false\n"
	if origin != "" {
		cfg += "[remote \"origin\"]\n\turl = " + origin + "\n"
	}
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o644)
}

func TestAttribution(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	repo := filepath.Join(home, "code", "app")
	mkRepo(t, repo, "git@github.com:me/app.git")
	os.MkdirAll(filepath.Join(repo, "src", "deep"), 0o755)
	noRemote := filepath.Join(home, "code", "toy")
	mkRepo(t, noRemote, "")
	plain := filepath.Join(home, "scratch")
	os.MkdirAll(plain, 0o755)

	r := New(home)
	if i := r.ForDir(filepath.Join(repo, "src", "deep")); i.Key != "github.com/me/app" || i.Root != repo || i.Name != "app" {
		t.Fatalf("%+v", i)
	}
	if i := r.ForPath("src/main.go", repo); i.Key != "github.com/me/app" {
		t.Fatalf("%+v", i)
	}
	if i := r.ForDir(noRemote); i.Key != "local:toy" {
		t.Fatalf("%+v", i)
	}
	if i := r.ForDir(plain); i.Key != "local:scratch" || i.Git {
		t.Fatalf("%+v", i)
	}
	if i := r.ForDir(home); i.Key != "local:~" {
		t.Fatalf("home must not leak the username: %+v", i)
	}
	if got := r.Rel(repo, filepath.Join(repo, "src", "a.go")); got != "src/a.go" {
		t.Fatal(got)
	}
	if got := r.Rel(repo, filepath.Join(home, "other", "b.go")); got != "~/other/b.go" {
		t.Fatal(got)
	}
}

func TestWorktreeUsesCommonConfig(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main")
	mkRepo(t, main, "https://github.com/me/wt.git")
	gd := filepath.Join(main, ".git", "worktrees", "feat")
	os.MkdirAll(gd, 0o755)
	os.WriteFile(filepath.Join(gd, "commondir"), []byte("../.."), 0o644)
	wt := filepath.Join(root, "feat")
	os.MkdirAll(wt, 0o755)
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gd+"\n"), 0o644)
	if i := New("").ForDir(wt); i.Key != "github.com/me/wt" || i.Root != wt {
		t.Fatalf("%+v", i)
	}
}
