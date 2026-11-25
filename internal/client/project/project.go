// Package project attributes paths to projects. A project is a folder: the
// top of a git repo, or a plain directory. A folder goes by a label, its
// normalized origin remote ("github.com/user/repo") or else "local:<dirname>",
// and the label can change (a remote is added, a repo is renamed). The
// project's key is the label the folder had when this device first saw it,
// and it stays: Resolver.Keep remembers it. Everything is plain file reads
// (no git subprocess) so hooks stay fast.
package project

import (
	"bufio"
	"os"
	"path/filepath"
	"seatime/pkg/event"
	"strings"
	"sync"
	"time"
)

type Info struct {
	Key string // the project's key on this device: the folder's first label
	// Label is what the folder goes by now: "github.com/u/r" or "local:<name>".
	Label  string
	Name   string // display name
	Remote string // normalized remote or ""
	Root   string // git root, or the directory for non-git projects
	Git    bool
}

type Resolver struct {
	Home string
	// Guard, when set, names directories this process must not touch (the
	// daemon on macOS, see package protected). They are attributed only from
	// Known, never from the file system, and an unknown one gets no project.
	Guard func(dir string) bool
	// Known returns the project whose recorded root contains dir.
	Known func(dir string) (Info, bool)
	// Keep returns the key of the project a folder belongs to, given the
	// label the folder goes by now (the store's FolderKey). Without it the
	// key is the label.
	Keep func(folder, label string) string
	mu   sync.Mutex
	dirs map[string]cached
}

// cached is a resolved directory. It is read again after cacheFor, so a
// process that runs for days (the daemon) notices a remote that changed.
type cached struct {
	Info
	at time.Time
}

const cacheFor = time.Minute

func New(home string) *Resolver { return &Resolver{Home: home, dirs: map[string]cached{}} }

// ForDir attributes a directory (need not exist).
func (r *Resolver) ForDir(dir string) Info {
	if dir == "" {
		return Info{}
	}
	dir = filepath.Clean(dir)
	r.mu.Lock()
	if c, ok := r.dirs[dir]; ok && time.Since(c.at) < cacheFor {
		r.mu.Unlock()
		return c.Info
	}
	r.mu.Unlock()
	if r.Guard != nil && r.Guard(dir) {
		if r.Known != nil {
			if i, ok := r.Known(dir); ok {
				return i
			}
		}
		return Info{} // not cached: a hook may record the root later
	}
	i := r.resolve(dir)
	r.mu.Lock()
	r.dirs[dir] = cached{i, time.Now()}
	r.mu.Unlock()
	return i
}

// Forget drops what the resolver has read, as a new process starts with
// nothing read.
func (r *Resolver) Forget() {
	r.mu.Lock()
	r.dirs = map[string]cached{}
	r.mu.Unlock()
}

// Plain attributes a directory as a folder of its own, whatever repo it is
// in (the folder an AI session was started in, outside any repo).
func (r *Resolver) Plain(dir string) Info {
	dir = filepath.Clean(dir)
	name := filepath.Base(dir)
	return r.keep(dir, Info{Label: "local:" + name, Name: name, Root: dir})
}

// keep gives the folder's project its key.
func (r *Resolver) keep(folder string, i Info) Info {
	i.Key = i.Label
	if r.Keep != nil {
		i.Key = r.Keep(folder, i.Label)
	}
	return i
}

// ForPath attributes a file path (relative paths are joined to cwd).
func (r *Resolver) ForPath(p, cwd string) Info {
	if p == "" {
		return Info{}
	}
	if !filepath.IsAbs(p) {
		if cwd == "" {
			return Info{}
		}
		p = filepath.Join(cwd, p)
	}
	return r.ForDir(filepath.Dir(p))
}

func (r *Resolver) resolve(dir string) Info {
	if root, gitdir, ok := FindGitRoot(dir); ok {
		remote := NormalizeRemote(OriginURL(gitdir))
		// the repo's folder: a git worktree belongs to the folder of the
		// repo it was added from
		folder := root
		if filepath.Base(gitdir) == ".git" {
			folder = filepath.Dir(gitdir)
		}
		name := filepath.Base(folder)
		if remote != "" {
			parts := strings.Split(remote, "/")
			name = parts[len(parts)-1]
			return r.keep(folder, Info{Label: remote, Name: name, Remote: remote, Root: root, Git: true})
		}
		return r.keep(folder, Info{Label: "local:" + name, Name: name, Root: root, Git: true})
	}
	name := filepath.Base(dir)
	if r.Home != "" && samePath(dir, r.Home) {
		name = "~"
	}
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "root"
	}
	return r.keep(dir, Info{Label: "local:" + name, Name: name, Root: dir})
}

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

// Rel renders p relative to the project root (forward slashes); paths outside
// the root are shown with the home directory collapsed to "~" so usernames
// don't leak into stored events.
func (r *Resolver) Rel(root, p string) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		return filepath.ToSlash(p)
	}
	if root != "" {
		if rel, err := filepath.Rel(root, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel)
		}
	}
	return r.Tilde(p)
}

func (r *Resolver) Tilde(p string) string {
	if r.Home != "" {
		if rel, err := filepath.Rel(r.Home, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if rel == "." {
				return "~"
			}
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(p)
}

// FindGitRoot walks up from dir looking for a .git directory or file.
// gitdir is the directory holding the repo's config (the common dir for
// worktrees).
func FindGitRoot(dir string) (root, gitdir string, ok bool) {
	d := dir
	for i := 0; i < 64; i++ {
		g := filepath.Join(d, ".git")
		if st, err := os.Stat(g); err == nil {
			if st.IsDir() {
				return d, g, true
			}
			// worktree / submodule: file containing "gitdir: <path>"
			if b, err := os.ReadFile(g); err == nil {
				line := strings.TrimSpace(string(b))
				if strings.HasPrefix(line, "gitdir:") {
					gd := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
					if !filepath.IsAbs(gd) {
						gd = filepath.Join(d, gd)
					}
					if cb, err := os.ReadFile(filepath.Join(gd, "commondir")); err == nil {
						cd := strings.TrimSpace(string(cb))
						if !filepath.IsAbs(cd) {
							cd = filepath.Join(gd, cd)
						}
						gd = filepath.Clean(cd)
					}
					return d, gd, true
				}
			}
			return d, g, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return "", "", false
}

// OriginURL reads the "origin" remote URL (or the first remote) from the repo config.
func OriginURL(gitdir string) string {
	f, err := os.Open(filepath.Join(gitdir, "config"))
	if err != nil {
		return ""
	}
	defer f.Close()
	var section, first string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(line)
			continue
		}
		if !strings.HasPrefix(section, "[remote ") {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(strings.ToLower(k)) != "url" {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if section == `[remote "origin"]` {
			return v
		}
		if first == "" {
			first = v
		}
	}
	return first
}

// NormalizeRemote turns a git remote URL into "host/owner/repo" (lowercase, no
// credentials, no .git). Local-path remotes yield "".
func NormalizeRemote(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	var host, path string
	if i := strings.Index(u, "://"); i >= 0 {
		scheme := strings.ToLower(u[:i])
		if scheme == "file" {
			return ""
		}
		rest := u[i+3:]
		slash := strings.Index(rest, "/")
		if slash < 0 {
			return ""
		}
		host, path = rest[:slash], rest[slash+1:]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		if c := strings.Index(host, ":"); c >= 0 { // port
			host = host[:c]
		}
	} else if at := strings.Index(u, "@"); at >= 0 && strings.Contains(u[at:], ":") {
		// scp-like: git@github.com:user/repo.git
		rest := u[at+1:]
		c := strings.Index(rest, ":")
		host, path = rest[:c], rest[c+1:]
	} else if c := strings.Index(u, ":"); c > 1 && !strings.ContainsAny(u[:c], `/\`) {
		host, path = u[:c], u[c+1:] // host:user/repo
	} else {
		return ""
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	if host == "" || path == "" || !strings.Contains(host, ".") {
		return ""
	}
	return strings.ToLower(host + "/" + path)
}

// IsPublicForge reports whether a normalized remote is on a well-known public
// forge (only those remotes are sent to the server in the clear).
func IsPublicForge(remote string) bool { return event.IsPublicForge(remote) }
