// Package gitscan derives `commit` events by reading git history of recently
// active projects. No global hooks are installed; it only runs `git log`.
package gitscan

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"seatime/internal/client/emit"
	"seatime/internal/client/store"
	"seatime/pkg/event"
)

// Scan looks at projects active within `active` and emits new commits by the
// repo's configured user. Returns the number of commit events emitted.
func Scan(ctx context.Context, p *emit.Pipeline, active time.Duration) (int, error) {
	if !Usable() {
		return 0, nil
	}
	now := p.Now()
	projs, err := p.Store.Projects(now.Add(-active))
	if err != nil {
		return 0, err
	}
	total := 0
	for _, pr := range projs {
		if pr.Root == "" || p.Res.Guard != nil && p.Res.Guard(pr.Root) {
			continue // guarded folder: the hook process scans it (ScanRoot)
		}
		if !ownFolder(p.Store, pr) {
			continue
		}
		if st, err := os.Stat(pr.Root + "/.git"); err != nil || st == nil {
			continue
		}
		n, err := scanOne(ctx, p, pr, now)
		total += n
		if err != nil {
			continue
		}
		if ctx.Err() != nil {
			break
		}
	}
	return total, nil
}

// ScanRoot scans one project for new commits if it was not scanned in the
// last minute. Hooks call it for projects in folders the daemon may not
// open (package protected). It is bounded by ctx.
func ScanRoot(ctx context.Context, p *emit.Pipeline, key string) (int, error) {
	if key == "" || !Usable() {
		return 0, nil
	}
	pr, ok, err := p.Store.GetProject(key)
	if err != nil || !ok || pr.Root == "" || !ownFolder(p.Store, pr) {
		return 0, err
	}
	if v, _ := p.Store.GetMeta("gitscan:" + pr.Key); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil && p.Now().Sub(time.Unix(sec, 0)) < time.Minute {
			return 0, nil
		}
	}
	if st, err := os.Stat(filepath.Join(pr.Root, ".git")); err != nil || st == nil {
		return 0, nil
	}
	return scanOne(ctx, p, pr, p.Now())
}

// ownFolder reports whether the project's folder is its own. A folder has
// one project (store folders.go), and its commits are that project's: an
// older key of the same folder, from before folders kept theirs, is not
// scanned again.
func ownFolder(st *store.Store, pr store.Project) bool {
	f, ok := st.FolderOf(pr.Root)
	return !ok || filepath.Clean(f.Root) != filepath.Clean(pr.Root) || f.Key == pr.Key
}

var (
	usableOnce sync.Once
	usable     bool
)

// Usable reports whether a real git is installed. On macOS /usr/bin/git is a
// shim that, without the Command Line Tools, pops an "install developer
// tools" dialog when run; it only counts once `xcode-select -p` succeeds.
func Usable() bool {
	usableOnce.Do(func() { usable = gitUsable(runtime.GOOS, exec.LookPath, xcodeSelect) })
	return usable
}

func xcodeSelect() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "xcode-select", "-p").Run() == nil
}

func gitUsable(goos string, look func(string) (string, error), devTools func() bool) bool {
	p, err := look("git")
	if err != nil {
		return false
	}
	if goos == "darwin" && filepath.Clean(p) == "/usr/bin/git" {
		return devTools()
	}
	return true
}

func git(ctx context.Context, root string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root, "-c", "core.quotepath=off"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.Bytes(), err
}

func scanOne(ctx context.Context, p *emit.Pipeline, pr store.Project, now time.Time) (int, error) {
	metaKey := "gitscan:" + pr.Key
	since := pr.FirstSeen.Add(-time.Hour)
	if v, _ := p.Store.GetMeta(metaKey); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = time.Unix(sec, 0).Add(-2 * time.Minute)
		}
	}
	email := ""
	if b, err := git(ctx, pr.Root, "config", "user.email"); err == nil {
		email = strings.ToLower(strings.TrimSpace(string(b)))
	}
	out, err := git(ctx, pr.Root, "log", "--no-merges", "--since="+strconv.FormatInt(since.Unix(), 10),
		"--numstat", "--format=%x1e%H%x1f%ct%x1f%ae")
	if err != nil {
		return 0, err
	}
	n := 0
	// oldest first so seq order follows history
	recs := strings.Split(string(out), "\x1e")
	for i := len(recs) - 1; i >= 1; i-- {
		rec := recs[i]
		lines := strings.Split(strings.TrimSpace(rec), "\n")
		hdr := strings.Split(lines[0], "\x1f")
		if len(hdr) < 3 {
			continue
		}
		if email != "" && strings.ToLower(strings.TrimSpace(hdr[2])) != email {
			continue
		}
		sec, _ := strconv.ParseInt(hdr[1], 10, 64)
		ev := event.Event{Kind: event.Commit, Harness: "git", Project: pr.Key, CWD: pr.Root, SHA: hdr[0], TS: time.Unix(sec, 0)}
		for _, l := range lines[1:] {
			f := strings.SplitN(l, "\t", 3)
			if len(f) < 3 {
				continue
			}
			ins, _ := strconv.Atoi(f[0]) // "-" for binary -> 0
			del, _ := strconv.Atoi(f[1])
			ev.Insertions += ins
			ev.Deletions += del
			ev.FilesChanged++
		}
		fresh, err := p.Store.MarkCommit(ev.SHA, pr.Key)
		if err != nil || !fresh {
			continue
		}
		if _, err := p.EmitEvent(ev); err == nil {
			n++
		}
	}
	_ = p.Store.SetMeta(metaKey, strconv.FormatInt(now.Unix(), 10))
	return n, nil
}
