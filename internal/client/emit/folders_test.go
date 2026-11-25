package emit

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"seatime/internal/client/store"
	"seatime/pkg/event"
)

// Which project is this: a folder keeps the project it was first seen as
// (store folders.go). Each test changes what a folder looks like and asks
// again, each time as a new hook process would.

type folders struct {
	t    *testing.T
	st   *store.Store
	home string
	db   string
}

func newFolders(t *testing.T) *folders {
	t.Helper()
	root := t.TempDir()
	f := &folders{t: t, home: filepath.Join(root, "home"), db: filepath.Join(root, "data", "g.db")}
	f.open()
	t.Cleanup(func() { f.st.Close() })
	return f
}

func (f *folders) open() {
	f.t.Helper()
	st, err := store.Open(f.db)
	if err != nil {
		f.t.Fatal(err)
	}
	f.st = st
}

func (f *folders) dir(name string) string { return filepath.Join(f.home, filepath.FromSlash(name)) }

// git makes dir a git repo with the origin remote (none for "").
func (f *folders) git(dir, remote string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	cfg := "[core]\n\tbare = false\n"
	if remote != "" {
		cfg += "[remote \"origin\"]\n\turl = " + remote + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// edit is the project of a hand edit of a file in dir; prompt the project
// of a prompt in a new AI session started in dir.
func (f *folders) edit(dir string) string {
	f.t.Helper()
	os.MkdirAll(dir, 0o755)
	e, err := New(f.st, f.home).EmitEvent(event.Event{Kind: event.Edit, Harness: "editor:vscode", Files: []string{filepath.Join(dir, "main.go")}})
	if err != nil {
		f.t.Fatal(err)
	}
	return e.Project
}

var nSession = 0

func (f *folders) prompt(dir string) string {
	f.t.Helper()
	os.MkdirAll(dir, 0o755)
	nSession++
	e, err := New(f.st, f.home).EmitEvent(event.Event{Kind: event.Prompt, Harness: "claude-code", SessionID: "s" + string(rune('a'+nSession)), CWD: dir, Text: "add the next thing to it please"})
	if err != nil {
		f.t.Fatal(err)
	}
	return e.Project
}

func (f *folders) same(what, got, want string) {
	f.t.Helper()
	if got != want {
		f.t.Errorf("%s: project %q, want %q", what, got, want)
	}
}

func TestFolderKeepsItsProjectWhenItsRemoteChanges(t *testing.T) {
	f := newFolders(t)
	d := f.dir("ysws-cupid")
	f.git(d, "")
	key := f.prompt(d)
	f.same("no remote", key, "local:ysws-cupid")
	f.same("a hand edit", f.edit(d), key)

	f.git(d, "https://github.com/lia/cupid.git") // a remote is added
	f.same("remote added, a prompt", f.prompt(d), key)
	f.same("remote added, a hand edit", f.edit(filepath.Join(d, "src")), key)
	if l := f.st.Label(key); l != "github.com/lia/cupid" {
		t.Errorf("label after the remote was added: %q", l)
	}
	f.git(d, "git@github.com:hackclub/cupid.git") // the repo moves to an org
	f.same("remote changed", f.prompt(d), key)
	if l := f.st.Label(key); l != "github.com/hackclub/cupid" {
		t.Errorf("label after the remote changed: %q", l)
	}
	// the same remote written another way is the same label
	for _, u := range []string{"https://github.com/HackClub/Cupid", "ssh://git@github.com/hackclub/cupid.git", "https://user:pw@github.com/hackclub/cupid.git/"} {
		f.git(d, u)
		f.same(u, f.edit(d), key)
		if l := f.st.Label(key); l != "github.com/hackclub/cupid" {
			t.Errorf("%s: label %q", u, l)
		}
	}
	f.git(d, "") // the remote is removed
	f.same("remote removed", f.edit(d), key)
	ps, _ := f.st.Projects(time.Time{})
	if len(ps) != 1 {
		t.Errorf("projects on the device: %+v", ps)
	}
	keys, _ := f.st.ProjectKeys()
	if len(keys) != 1 {
		t.Errorf("keys in the log: %v", keys)
	}
}

func TestGitInitLaterInAPlainFolder(t *testing.T) {
	f := newFolders(t)
	d := f.dir("notes-app")
	key := f.prompt(d) // an AI session started in a plain folder
	f.same("plain folder", key, "local:notes-app")
	f.git(d, "")
	f.same("after git init", f.prompt(d), key)
	f.same("after git init, a hand edit", f.edit(d), key)
	f.git(d, "https://github.com/lia/notes.git")
	f.same("after a remote", f.prompt(d), key)

	// A plain folder nobody started a session in has no top: a hand edit
	// belongs to the directory of its file. git init gives it one, and
	// the repo's folder is a new project.
	p := f.dir("loose")
	f.same("a hand edit in a plain directory", f.edit(filepath.Join(p, "src")), "local:src")
	f.git(p, "")
	f.same("the same file after git init above it", f.edit(filepath.Join(p, "src")), "local:loose")
}

func TestFolderMovedOrRenamed(t *testing.T) {
	f := newFolders(t)
	// with a remote: a move or a rename is the same project
	a := f.dir("code/app")
	f.git(a, "https://github.com/lia/app.git")
	key := f.edit(a)
	b := f.dir("work/app-renamed")
	os.MkdirAll(filepath.Dir(b), 0o755)
	if err := os.Rename(a, b); err != nil {
		t.Fatal(err)
	}
	f.same("moved and renamed, with a remote", f.edit(b), key)

	// without one: a move keeps the project, a rename starts another
	c := f.dir("code/game")
	f.git(c, "")
	local := f.edit(c)
	d := f.dir("work/game")
	os.Rename(c, d)
	f.same("moved, no remote", f.edit(d), local)
	e := f.dir("work/game2")
	os.Rename(d, e)
	if got := f.edit(e); got != "local:game2" {
		t.Errorf("renamed, no remote: %q (a new project is what the rule gives)", got)
	}
}

func TestSecondCloneAndLaterFolders(t *testing.T) {
	f := newFolders(t)
	a := f.dir("a/site")
	f.git(a, "https://github.com/lia/site.git")
	key := f.edit(a)
	// the same repo cloned into a second folder
	b := f.dir("b/site-copy")
	f.git(b, "https://github.com/lia/site.git")
	f.same("second clone", f.edit(b), key)

	// a folder that started as "app" and went on under a remote: another
	// folder called "app" later is another project
	x := f.dir("x/app")
	f.git(x, "")
	first := f.edit(x)
	f.git(x, "https://github.com/lia/snake.git")
	f.same("app with a remote", f.edit(x), first)
	y := f.dir("y/app")
	f.git(y, "")
	second := f.edit(y)
	if second == first || !regexp.MustCompile(`^local:[0-9a-f]{12}$`).MatchString(second) {
		t.Errorf("a later folder with the first one's old name: %q (first %q)", second, first)
	}
	f.same("the later folder again", f.edit(y), second)
	f.git(y, "https://github.com/lia/chess.git")
	f.same("the later folder with its own remote", f.edit(y), second)
	// a clone of the first one's repo joins the first
	z := f.dir("z/snake")
	f.git(z, "https://github.com/lia/snake.git")
	f.same("a clone of the first", f.edit(z), first)
	f.same("the first", f.edit(x), first)
}

func TestWorktreeAndNestedRepo(t *testing.T) {
	f := newFolders(t)
	main := f.dir("tool")
	f.git(main, "")
	key := f.edit(main)
	// a git worktree of it, inside and outside the repo's folder
	for _, wt := range []string{filepath.Join(main, ".claude", "worktrees", "fix"), f.dir("tool-wt")} {
		os.MkdirAll(wt, 0o755)
		gd := filepath.Join(main, ".git", "worktrees", filepath.Base(wt))
		os.MkdirAll(gd, 0o755)
		os.WriteFile(filepath.Join(gd, "commondir"), []byte("../..\n"), 0o644)
		os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gd+"\n"), 0o644)
		f.same("worktree "+wt, f.edit(wt), key)
		f.same("a session in worktree "+wt, f.prompt(wt), key)
	}
	// a repo of its own inside the folder is its own project
	in := filepath.Join(main, "vendor", "lib")
	f.git(in, "https://github.com/other/lib.git")
	f.same("nested repo", f.edit(in), "github.com/other/lib")
	f.same("the outer repo", f.edit(main), key)
}

func TestFolderDeletedAndMadeAgain(t *testing.T) {
	f := newFolders(t)
	d := f.dir("my-app")
	f.git(d, "https://github.com/lia/snake.git")
	key := f.edit(d)
	os.RemoveAll(d)
	f.git(d, "https://github.com/lia/snake.git") // cloned again
	f.same("cloned again", f.edit(d), key)
	os.RemoveAll(d)
	f.git(d, "https://github.com/lia/chess.git") // another repo in its place
	f.same("another repo at the same path (the folder is the project)", f.edit(d), key)
}

func TestLocalDatabaseLost(t *testing.T) {
	f := newFolders(t)
	d := f.dir("ysws-cupid")
	f.git(d, "")
	f.same("before", f.edit(d), "local:ysws-cupid")
	f.git(d, "https://github.com/hackclub/cupid.git")
	f.same("with a remote", f.edit(d), "local:ysws-cupid")
	f.st.Close()
	for _, x := range []string{"", "-wal", "-shm"} {
		os.Remove(f.db + x)
	}
	f.open()
	// the folder starts again under what it goes by now, which is the
	// key the server knows the project by
	f.same("after the database was lost", f.edit(d), "github.com/hackclub/cupid")
}

// A store from before folders kept their project: a folder that went by
// three keys goes on under the newest, and the older two take no new
// folder.
func TestFoldersFromAnOlderStore(t *testing.T) {
	f := newFolders(t)
	d := f.dir("ysws-cupid")
	f.git(d, "git@github.com:hackclub/cupid.git")
	other := f.dir("plain")
	f.st.Close()
	db, err := sql.Open("sqlite", f.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP TABLE folders`, `ALTER TABLE projects DROP COLUMN label`,
		`INSERT INTO projects(key,name,remote,root,first_seen,last_seen) VALUES
 ('local:ysws-cupid','ysws-cupid','','` + d + `',1000,5000),
 ('github.com/lia/cupid','cupid','github.com/lia/cupid','` + d + `',2000,6000),
 ('github.com/hackclub/cupid','cupid','github.com/hackclub/cupid','` + d + `',3000,7000),
 ('local:plain','plain','','` + other + `',1000,2000),
 ('local:byname','byname','','',1000,2000)`,
		`PRAGMA user_version=8`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()
	f.open()
	f.same("the folder after the update", f.edit(d), "github.com/hackclub/cupid")
	f.same("a session in it", f.prompt(d), "github.com/hackclub/cupid")
	f.same("another folder", f.edit(other), "local:plain")
	for key, want := range map[string]string{"local:ysws-cupid": "local:ysws-cupid", "github.com/hackclub/cupid": "github.com/hackclub/cupid", "local:byname": "local:byname"} {
		if l := f.st.Label(key); l != want {
			t.Errorf("label of %s: %q", key, l)
		}
	}
	// a new folder with the old name is not the old key's project
	n := f.dir("elsewhere/ysws-cupid")
	f.git(n, "")
	if got := f.edit(n); got == "local:ysws-cupid" || got == "github.com/hackclub/cupid" {
		t.Errorf("a new folder joined an older key of another folder: %q", got)
	}
}
