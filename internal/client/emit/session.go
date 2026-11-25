package emit

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"seatime/internal/client/project"
	"seatime/pkg/event"
)

// An AI session belongs to one project: the one it was started in, the git
// repo of the folder its first record names. Every record of the session
// goes to it: prompts, the agent's start and stop, tool calls, and writes
// anywhere. A later `cd`, or a write in another repo, changes nothing and
// creates no project; such a write is a file outside the project (RunFacts).
//
// A session started outside any repo has no such project. Its records go
// by where it writes: the repo of a written file when that repo is inside
// the first folder, else the first folder itself. Started in the home
// folder (or in an AI tool's own folder), it goes by the repo or folder it
// writes in, and to the home folder only while it has written nowhere else.
// An AI tool's own folders (ownFolders) are never a project.
//
// Hand edits from an editor have no session and go by the file's project.

// sessionHome is what a session's first folder decided, kept in the store's
// meta so that every later hook process of the session reads the same.
type sessionHome struct {
	// Key, Name, Remote, Root: the repo the session was started in.
	Key    string `json:"key,omitempty"`
	Name   string `json:"name,omitempty"`
	Remote string `json:"remote,omitempty"`
	Root   string `json:"root,omitempty"`
	// Dir: the first folder, when it was in no repo.
	Dir string `json:"dir,omitempty"`
}

func homeKey(session string) string  { return "session_home:" + session }
func modelKey(session string) string { return "session_model:" + session }

// ownFolders are the AI tools' own folders under the home folder (settings,
// session logs, memory): what a tool writes there is not project work.
var ownFolders = []string{
	".claude", ".codex", ".cursor", ".gemini", ".codeium", ".windsurf", ".copilot", ".aider", ".cline", ".roo", ".kilocode",
	".continue", ".vscode", ".vscode-server", ".cursor-server", ".windsurf-server",
	".config/opencode", ".config/kilo", ".config/amp", ".config/Code", ".config/Cursor", ".config/Windsurf", ".config/github-copilot",
	".local/share/opencode", ".local/share/kilo", ".local/share/amp",
	"Library/Application Support/Claude", "Library/Application Support/Code", "Library/Application Support/Cursor", "Library/Application Support/Windsurf",
	"AppData/Roaming/Claude", "AppData/Roaming/Code", "AppData/Roaming/Cursor", "AppData/Roaming/Windsurf",
}

func under(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ownFolder reports whether path is in one of the AI tools' own folders.
func (p *Pipeline) ownFolder(path string) bool {
	home := p.Res.Home
	if home == "" || path == "" {
		return false
	}
	for _, d := range ownFolders {
		if under(filepath.Join(home, filepath.FromSlash(d)), path) {
			return true
		}
	}
	return false
}

// realFolder reports whether a folder can be a project of its own: not the
// home folder, not the file system's root, not an AI tool's own folder.
func (p *Pipeline) realFolder(dir string) bool {
	dir = filepath.Clean(dir)
	return dir != "" && dir != filepath.Dir(dir) && (p.Res.Home == "" || dir != filepath.Clean(p.Res.Home)) && !p.ownFolder(dir)
}

func (p *Pipeline) loadHome(session string) (sessionHome, bool) {
	var h sessionHome
	v, _ := p.Store.GetMeta(homeKey(session))
	if v == "" || json.Unmarshal([]byte(v), &h) != nil {
		return h, false
	}
	return h, h.Key != "" || h.Dir != ""
}

// sessionProject is the project of one record of an AI session, and what
// is known about that project (to keep its folder on record).
func (p *Pipeline) sessionProject(e event.Event, cwd string, cwdInfo project.Info) (string, []project.Info) {
	h, ok := p.loadHome(e.SessionID)
	if !ok && cwd != "" && cwdInfo.Key != "" {
		// in a repo, or inside a project this device already knows by its
		// folder (the daemon may not look into some folders itself)
		if (cwdInfo.Git || filepath.Clean(cwdInfo.Root) != filepath.Clean(cwd)) && p.realFolder(cwdInfo.Root) {
			h = sessionHome{Key: cwdInfo.Key, Name: cwdInfo.Name, Remote: cwdInfo.Remote, Root: cwdInfo.Root}
		} else {
			h = sessionHome{Dir: filepath.Clean(cwd)}
		}
		if b, err := json.Marshal(h); err == nil {
			_ = p.Store.SetMeta(homeKey(e.SessionID), string(b))
		}
		ok = true
	}
	if !ok {
		// no folder yet (a record with none, or a folder this process may
		// not look into): the project the session has written in most
		key, _ := p.Store.SessionProject(e.SessionID)
		return key, nil
	}
	if h.Key != "" {
		// as its folder is now (its remote may have changed since the start)
		if i := p.Res.ForDir(h.Root); i.Key == h.Key {
			return h.Key, []project.Info{i}
		}
		return h.Key, []project.Info{{Key: h.Key, Name: h.Name, Remote: h.Remote, Root: h.Root, Git: true}}
	}
	// started outside any repo: by where it writes
	first := p.Res.ForDir(h.Dir)
	real := p.realFolder(h.Dir)
	if real && filepath.Clean(first.Root) != filepath.Clean(h.Dir) {
		first = p.Res.Plain(h.Dir) // the folder itself, whatever repo it has come to be in
	}
	lands := func(i project.Info) bool {
		return i.Key != "" && i.Git && p.realFolder(i.Root) && (!real || under(h.Dir, i.Root))
	}
	byKey := map[string]project.Info{first.Key: first}
	if e.Write && len(e.Files) > 0 {
		tally := map[string]int{}
		var order []string
		for _, f := range e.Files {
			if !filepath.IsAbs(f) {
				if cwd == "" {
					continue
				}
				f = filepath.Join(cwd, f)
			}
			if p.ownFolder(f) {
				continue
			}
			i := p.Res.ForPath(f, cwd)
			switch {
			case lands(i):
			case real && under(h.Dir, f):
				i = first
			case !real && i.Key != "" && !i.Git && p.realFolder(i.Root):
				// started in the home folder: a plain folder it writes in
			default:
				continue
			}
			if _, seen := tally[i.Key]; !seen {
				order = append(order, i.Key)
			}
			tally[i.Key]++
			byKey[i.Key] = i
		}
		best := ""
		for _, k := range order {
			_ = p.Store.BumpSessionProject(e.SessionID, k, tally[k])
			if best == "" || tally[k] > tally[best] {
				best = k
			}
		}
		if best != "" {
			return best, []project.Info{byKey[best]}
		}
	}
	if key, _ := p.Store.SessionProject(e.SessionID); key != "" {
		if i, ok := byKey[key]; ok {
			return key, []project.Info{i}
		}
		return key, nil
	}
	if lands(cwdInfo) {
		return cwdInfo.Key, []project.Info{cwdInfo}
	}
	return first.Key, []project.Info{first}
}

// sessionModel carries an AI session's model forward: a record that names
// none takes the last one a record of the session named. Some tools name
// their model only on some events (Claude Code: at the session's start).
func (p *Pipeline) sessionModel(e *event.Event) {
	if e.SessionID == "" {
		return
	}
	if e.Model = strings.TrimSpace(e.Model); len(e.Model) > 120 {
		e.Model = e.Model[:120]
	}
	last, _ := p.Store.GetMeta(modelKey(e.SessionID))
	switch {
	case e.Model == "":
		e.Model = last
	case e.Model != last:
		_ = p.Store.SetMeta(modelKey(e.SessionID), e.Model)
	}
}
