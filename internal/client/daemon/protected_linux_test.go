package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"seatime/internal/adapters"
	"seatime/internal/client/config"
	"seatime/internal/client/emit"
	"seatime/internal/client/gitscan"
	"seatime/internal/client/protected"
	"seatime/internal/client/store"
	"seatime/pkg/event"
)

// watchOpens reports every open or read under dirs (inotify; stat calls are
// not visible, the resolver's Guard covers those).
func watchOpens(t *testing.T, dirs ...string) func() []string {
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Skip("inotify unavailable:", err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	names := map[int32]string{}
	for _, root := range dirs {
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				wd, err := syscall.InotifyAddWatch(fd, p, syscall.IN_OPEN|syscall.IN_ACCESS)
				if err == nil {
					names[int32(wd)] = p
				}
			}
			return nil
		})
	}
	// drain what our own setup (WalkDir) caused
	buf := make([]byte, 64<<10)
	for {
		if n, _ := syscall.Read(fd, buf); n <= 0 {
			break
		}
	}
	return func() []string {
		var out []string
		for {
			n, _ := syscall.Read(fd, buf)
			if n <= 0 {
				return out
			}
			for off := 0; off+syscall.SizeofInotifyEvent <= n; {
				ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
				name := ""
				if ev.Len > 0 {
					b := buf[off+syscall.SizeofInotifyEvent : off+syscall.SizeofInotifyEvent+int(ev.Len)]
					for i, c := range b {
						if c == 0 {
							b = b[:i]
							break
						}
					}
					name = string(b)
				}
				out = append(out, filepath.Join(names[ev.Wd], name))
				off += syscall.SizeofInotifyEvent + int(ev.Len)
			}
		}
	}
}

// The macOS rule, checked on Linux: with the guard on, nothing the daemon
// runs (watchers, commit scan, auto-hook, heartbeat attribution, spool)
// opens or lists a file under Desktop, Documents, Downloads or iCloud Drive,
// because on a Mac that raises a permission dialog for a background process.
func TestDaemonNeverOpensProtectedFolders(t *testing.T) {
	sandbox(t)
	home := os.Getenv("SEATIME_FAKE_HOME")
	t.Setenv("HOME", home)
	cfg := config.Load()
	f := false
	cfg.AutoHook = &f // harness config edits are covered elsewhere; keep this test about reads
	config.Save(cfg)
	proj := filepath.Join(home, "Documents", "app")
	for _, d := range []string{proj, filepath.Join(home, "Desktop", "x"), filepath.Join(home, "Downloads"),
		filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs", "y")} {
		os.MkdirAll(d, 0o755)
	}
	if _, err := exec.LookPath("git"); err == nil {
		exec.Command("git", "-C", proj, "init", "-q").Run()
	} else {
		os.MkdirAll(filepath.Join(proj, ".git"), 0o755)
	}
	os.WriteFile(filepath.Join(proj, "main.go"), []byte("package main\n"), 0o644)
	os.WriteFile(filepath.Join(proj, ".aider.chat.history.md"), []byte("# aider chat started at 2026-11-20 09:00:00\n"), 0o644)
	os.WriteFile(filepath.Join(home, "Desktop", "x", ".aider.chat.history.md"), nil, 0o644)

	st, err := store.Open(config.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// a hook (running in the user's terminal) has already seen the project
	st.TouchProject(store.Project{Key: "github.com/kid/app", Name: "app", Remote: "github.com/kid/app", Root: proj}, time.Now())

	protected.Enable(true)
	defer protected.Enable(false)
	p := emit.New(st, home)
	p.GuardProtected(home)
	d := &Daemon{Store: st, Pipeline: p, Adapters: adapters.All()}

	opens := watchOpens(t, filepath.Join(home, "Documents"), filepath.Join(home, "Desktop"),
		filepath.Join(home, "Downloads"), filepath.Join(home, "Library", "Mobile Documents"))

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	for _, a := range d.Adapters {
		if w, ok := a.(adapters.Watcher); ok {
			go d.watch(ctx, a.Name(), w)
		}
	}
	if _, err := gitscan.Scan(ctx, p, 72*time.Hour); err != nil {
		t.Fatal(err)
	}
	d.AutoHookOnce()
	// an editor heartbeat and a watcher event inside the protected project
	hb, err := p.EmitEvent(event.Event{Kind: event.Edit, Harness: "editor:vscode", Files: []string{filepath.Join(proj, "main.go")},
		CWD: proj, Extra: map[string]any{"editor_project": "app"}})
	if err != nil {
		t.Fatal(err)
	}
	unknown, _ := p.EmitEvent(event.Event{Kind: event.Edit, Harness: "editor:vscode", Files: []string{filepath.Join(home, "Desktop", "x", "a.py")},
		CWD: filepath.Join(home, "Desktop", "x"), Extra: map[string]any{"editor_project": "x"}})
	d.DrainSpool(config.SpoolDir())
	<-ctx.Done()
	time.Sleep(100 * time.Millisecond)

	if got := opens(); len(got) > 0 {
		t.Fatalf("daemon code opened protected paths: %v", got)
	}
	if hb.Project != "github.com/kid/app" {
		t.Fatalf("heartbeat in a protected folder should map to the known project, got %q", hb.Project)
	}
	if unknown.Project != "local:x" {
		t.Fatalf("an unknown protected folder falls back to the editor's project name, got %q", unknown.Project)
	}
}

// The guard is what keeps the daemon out; without it the same calls do read
// the project (so the test above would notice a regression).
func TestOpenWatcherSeesUnguardedReads(t *testing.T) {
	sandbox(t)
	home := os.Getenv("SEATIME_FAKE_HOME")
	proj := filepath.Join(home, "Documents", "app")
	os.MkdirAll(filepath.Join(proj, ".git"), 0o755)
	os.WriteFile(filepath.Join(proj, ".git", "config"), []byte("[remote \"origin\"]\n\turl = https://github.com/kid/app\n"), 0o644)
	st, err := store.Open(config.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	opens := watchOpens(t, filepath.Join(home, "Documents"))
	p := emit.New(st, home)
	if _, err := p.EmitEvent(event.Event{Kind: event.Edit, Files: []string{filepath.Join(proj, "main.go")}, CWD: proj}); err != nil {
		t.Fatal(err)
	}
	if len(opens()) == 0 {
		t.Fatal("expected the unguarded resolver to read .git/config")
	}
}
