package gitscan

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"seatime/internal/client/emit"
	"seatime/internal/client/store"
	"seatime/pkg/event"
)

func TestScanEmitsCommitsOnce(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.MkdirAll(repo, 0o755)
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("1\n2\n3\n"), 0o644)
	run("add", ".")
	run("commit", "-qm", "one")

	st, _ := store.Open(filepath.Join(root, "g.db"))
	defer st.Close()
	p := emit.New(st, root)
	// make the project known (as a hook would)
	p.EmitEvent(event.Event{Kind: event.Prompt, SessionID: "s", CWD: repo, Text: "hi"})
	n, err := Scan(context.Background(), p, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	n, _ = Scan(context.Background(), p, time.Hour)
	if n != 0 {
		t.Fatalf("rescan emitted %d", n)
	}
	cs, _ := st.Events(store.Query{Kinds: []event.Kind{event.Commit}})
	if len(cs) != 1 || cs[0].Insertions != 3 || cs[0].FilesChanged != 1 || cs[0].Project != "local:repo" || cs[0].CWD != "" {
		t.Fatalf("%+v", cs)
	}
}

// On a Mac without the Command Line Tools, running /usr/bin/git opens an
// "install developer tools" dialog, so the shim only counts with dev tools.
func TestGitUsableAvoidsMacShimDialog(t *testing.T) {
	at := func(p string) func(string) (string, error) { return func(string) (string, error) { return p, nil } }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	yes, no := func() bool { return true }, func() bool { return false }
	cases := []struct {
		goos string
		look func(string) (string, error)
		dev  func() bool
		want bool
	}{
		{"darwin", at("/usr/bin/git"), no, false},
		{"darwin", at("/usr/bin/git"), yes, true},
		{"darwin", at("/opt/homebrew/bin/git"), no, true},
		{"linux", at("/usr/bin/git"), no, true},
		{"darwin", missing, yes, false},
	}
	for i, c := range cases {
		if got := gitUsable(c.goos, c.look, c.dev); got != c.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
}

// gitRepo makes a repo with one commit and returns a runner for git in it.
func gitRepo(t *testing.T, repo string) func(args ...string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	os.MkdirAll(repo, 0o755)
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("1\n"), 0o644)
	run("add", ".")
	run("commit", "-qm", "one")
	return run
}

// A folder that gets a remote, and then another, is one project: each of
// its commits is recorded once, and the scan goes on from where it was.
func TestCommitsOnceWhenTheRemoteChanges(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "ysws-cupid")
	run := gitRepo(t, repo)
	st, _ := store.Open(filepath.Join(root, "g.db"))
	defer st.Close()
	scan := func(want int) {
		t.Helper()
		p := emit.New(st, root) // as a new process: the folder is read again
		p.EmitEvent(event.Event{Kind: event.Prompt, SessionID: "s", CWD: repo, Text: "go on with it please"})
		for i := 0; i < 2; i++ {
			n, err := Scan(context.Background(), p, time.Hour)
			if err != nil || n != want {
				t.Fatalf("scan found %d new commits, want %d (%v)", n, want, err)
			}
			want = 0 // a second scan finds nothing new
		}
	}
	scan(1)
	since, _ := st.GetMeta("gitscan:local:ysws-cupid")
	run("remote", "add", "origin", "https://github.com/lia/cupid.git")
	scan(0)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("1\n2\n"), 0o644)
	run("commit", "-qam", "two")
	scan(1)
	run("remote", "set-url", "origin", "git@github.com:hackclub/cupid.git")
	scan(0)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("1\n2\n3\n"), 0o644)
	run("commit", "-qam", "three")
	scan(1)
	cs, _ := st.Events(store.Query{Kinds: []event.Kind{event.Commit}})
	seen := map[string]bool{}
	for _, c := range cs {
		if c.Project != "local:ysws-cupid" || seen[c.SHA] {
			t.Errorf("commit %s under %s (seen before: %v)", c.SHA, c.Project, seen[c.SHA])
		}
		seen[c.SHA] = true
	}
	if len(cs) != 3 {
		t.Fatalf("%d commit records, want 3", len(cs))
	}
	if v, _ := st.GetMeta("gitscan:local:ysws-cupid"); v == "" || since == "" {
		t.Errorf("the scan's place was not kept under the project's key: %q then %q", since, v)
	}
	for _, k := range []string{"github.com/lia/cupid", "github.com/hackclub/cupid"} {
		if v, _ := st.GetMeta("gitscan:" + k); v != "" {
			t.Errorf("a scan started again under %s", k)
		}
	}
}

// A store an older SeaTime split: the folder is three projects, each with
// the folder's commits. After the update a new commit is recorded once.
func TestCommitsOnceInAFolderAnOlderStoreSplit(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "ysws-cupid")
	run := gitRepo(t, repo)
	run("remote", "add", "origin", "https://github.com/hackclub/cupid.git")
	path := filepath.Join(root, "g.db")
	st, _ := store.Open(path)
	st.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for _, q := range []string{
		`DROP TABLE folders`, `ALTER TABLE projects DROP COLUMN label`,
		`INSERT INTO projects(key,name,remote,root,first_seen,last_seen) VALUES
 ('local:ysws-cupid','ysws-cupid','','` + repo + `',` + strconv.FormatInt(now-3000, 10) + `,` + strconv.FormatInt(now-300, 10) + `),
 ('github.com/lia/cupid','cupid','github.com/lia/cupid','` + repo + `',` + strconv.FormatInt(now-2000, 10) + `,` + strconv.FormatInt(now-200, 10) + `),
 ('github.com/hackclub/cupid','cupid','github.com/hackclub/cupid','` + repo + `',` + strconv.FormatInt(now-1000, 10) + `,` + strconv.FormatInt(now-100, 10) + `)`,
		`PRAGMA user_version=8`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()
	st, _ = store.Open(path)
	defer st.Close()
	p := emit.New(st, root)
	n, err := Scan(context.Background(), p, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("scan found %d new commits, want 1 (%v)", n, err)
	}
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("1\n2\n"), 0o644)
	run("commit", "-qam", "two")
	if n, err = Scan(context.Background(), p, time.Hour); err != nil || n != 1 {
		t.Fatalf("scan found %d new commits, want 1 (%v)", n, err)
	}
	cs, _ := st.Events(store.Query{Kinds: []event.Kind{event.Commit}})
	for _, c := range cs {
		if c.Project != "github.com/hackclub/cupid" {
			t.Errorf("commit under %s", c.Project)
		}
	}
	if len(cs) != 2 {
		t.Fatalf("%d commit records, want 2", len(cs))
	}
}
