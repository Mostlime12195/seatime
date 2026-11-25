// Package emit is the client's adapters.Sink: it normalises events from any
// source (hooks, watchers, plugins, git scan, WakaTime proxy), attributes them
// to projects, decides whether a prompt is new and appends them to the store.
package emit

import (
	"maps"
	"path/filepath"
	"strings"
	"time"

	"seatime/internal/client/checks"
	"seatime/internal/client/logx"
	"seatime/internal/client/presence"
	"seatime/internal/client/project"
	"seatime/internal/client/protected"
	"seatime/internal/client/scrub"
	"seatime/internal/client/store"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
	"seatime/pkg/promptcheck"
)

type Pipeline struct {
	Store *store.Store
	Res   *project.Resolver
	Now   func() time.Time
	// NoText: store only commitments of prompt text and commands (config
	// keep_prompt_text).
	NoText bool
	// Presence, when set, reads the presence facts at a prompt (the hook
	// sets it: it runs in the person's session, at the moment of the prompt).
	Presence func() presence.Facts
	// Repo, when set, reads git's counts at an agent stop. Only the hook
	// sets it: the daemon never opens a project folder (protected folders).
	Repo func(root string) *event.GitFacts
	// Snap, when set, reads the repository for the prompt check's change
	// excerpt at a prompt, a stop and a session's end (package checks).
	// Only the hook sets it, as Repo.
	Snap func(root string) *promptcheck.Snapshot
	// Facts, when set, reads what a prompt check says about the project
	// (its name, file names, README) in its folder at a prompt, before the
	// AI changes anything for it. Only the hook sets it, as Repo.
	Facts func(root, name string) *promptcheck.Project
}

func New(st *store.Store, home string) *Pipeline {
	p := &Pipeline{Store: st, Res: project.New(home), Now: time.Now}
	// a folder keeps the project it was first seen as (store folders.go)
	p.Res.Keep = func(folder, label string) string {
		key, err := st.FolderKey(folder, label)
		if err != nil {
			logx.Printf("project of %s: %v", label, err)
			return label
		}
		return key
	}
	return p
}

// GuardProtected makes the pipeline attribute paths under macOS-protected
// folders (package protected) from projects the store already knows, without
// touching the file system. The daemon uses it; hooks do not need it.
func (p *Pipeline) GuardProtected(home string) {
	p.Res.Guard = func(dir string) bool { return protected.Blocked(home, dir) }
	p.Res.Known = func(dir string) (project.Info, bool) { return KnownRoot(p.Store, dir) }
}

func (p *Pipeline) projectByName(name string) string {
	if ps, err := p.Store.Projects(time.Time{}); err == nil {
		for _, pr := range ps {
			if strings.EqualFold(pr.Name, name) {
				return pr.Key
			}
		}
	}
	return "local:" + name
}

// KnownRoot finds the known folder that holds dir (the deepest one) and
// its project.
func KnownRoot(st *store.Store, dir string) (project.Info, bool) {
	f, ok := st.FolderOf(dir)
	if !ok {
		return project.Info{}, false
	}
	i := project.Info{Key: f.Key, Label: f.Label, Name: nameOf(f.Label), Root: f.Root}
	if pr, ok, _ := st.GetProject(f.Key); ok {
		i.Name, i.Remote, i.Git = pr.Name, pr.Remote, pr.Remote != ""
	}
	return i, true
}

// Emit implements adapters.Sink.
func (p *Pipeline) Emit(e event.Event) error {
	_, err := p.EmitEvent(e)
	return err
}

// EmitEvent is Emit that also returns the stored event.
func (p *Pipeline) EmitEvent(e event.Event) (event.Event, error) {
	now := p.Now()
	if e.TS.IsZero() {
		e.TS = now
	}
	cwd := e.CWD
	var cwdInfo project.Info
	if cwd != "" {
		cwdInfo = p.Res.ForDir(cwd)
	}

	// --- project attribution ---
	var infos []project.Info // infos to touch
	key := e.Project
	switch {
	case key != "":
	case e.SessionID != "":
		// an AI session: the project it was started in (session.go)
		key, infos = p.sessionProject(e, cwd, cwdInfo)
	case len(e.Files) > 0 && (e.Write || e.Kind == event.Edit):
		// a hand edit: the file's project. A file in an AI tool's own folder
		// (its memory, its logs) is not project work: an edit of nothing
		// else is not recorded.
		tally := map[string]int{}
		byKey := map[string]project.Info{}
		var order []string
		own := 0
		for _, f := range e.Files {
			abs := f
			if !filepath.IsAbs(abs) && cwd != "" {
				abs = filepath.Join(cwd, abs)
			}
			if p.ownFolder(abs) {
				own++
				continue
			}
			i := p.Res.ForPath(f, cwd)
			if i.Key == "" {
				continue
			}
			if _, ok := tally[i.Key]; !ok {
				order = append(order, i.Key)
			}
			tally[i.Key]++
			byKey[i.Key] = i
		}
		best := ""
		for _, k := range order {
			if best == "" || tally[k] > tally[best] {
				best = k
			}
		}
		if own == len(e.Files) && e.Kind == event.Edit {
			return event.Event{}, nil
		}
		if best != "" {
			key = best
			for _, k := range order {
				infos = append(infos, byKey[k])
			}
		}
	}
	if key == "" && cwdInfo.Key != "" {
		key = cwdInfo.Key
	}
	if key == "" && e.Kind == event.Edit {
		// a guarded folder the daemon may not look into: use the editor's own
		// project name, matched to a known project when one has that name
		if name, _ := e.Extra["editor_project"].(string); name != "" {
			key = p.projectByName(name)
		}
	}
	e.Project = key
	if cwdInfo.Key != "" {
		infos = append(infos, cwdInfo)
	}
	touched := false
	projRoot := "" // the project's folder: paths are stored relative to it, and are inside or outside it
	for _, i := range infos {
		if i.Key == key {
			if !touched {
				_ = p.Store.TouchProject(store.Project{Key: i.Key, Label: i.Label, Name: i.Name, Remote: i.Remote, Root: i.Root}, now)
				projRoot = i.Root
			}
			touched = true
		}
	}
	if !touched && key != "" {
		_ = p.Store.TouchProject(store.Project{Key: key, Name: nameOf(key)}, now)
	}
	p.sessionModel(&e)

	// --- privacy: relative paths, scrubbed text/commands ---
	if cwd != "" {
		e.CWD = p.Res.Rel(projRoot, cwd)
		if e.CWD == "." {
			e.CWD = ""
		}
	}
	if len(e.Files) > 0 {
		files := make([]string, len(e.Files))
		for i, f := range e.Files {
			root := projRoot
			if fi := p.Res.ForPath(f, cwd); e.SessionID == "" && fi.Key == key {
				root = fi.Root
			}
			if !filepath.IsAbs(f) && cwd != "" && e.SessionID != "" {
				f = filepath.Join(cwd, f) // a path the agent gave from where it stood
			}
			files[i] = p.Res.Rel(root, f)
		}
		e.Files = files
	}
	e.Command = scrub.Text(e.Command)

	// --- the AI's last message, for the prompt check only: never stored ---
	var reply string
	if r, ok := e.Extra[promptcheck.ReplyKey].(string); ok {
		reply = r
		e.Extra = maps.Clone(e.Extra)
		delete(e.Extra, promptcheck.ReplyKey)
		if len(e.Extra) == 0 {
			e.Extra = nil
		}
	}
	// --- a prompt the tool held in its queue: when it was typed ---
	var typed time.Time
	if v, ok := e.Extra[event.TypedAtKey].(string); ok {
		typed, _ = time.Parse(time.RFC3339Nano, v)
		e.Extra = maps.Clone(e.Extra)
		delete(e.Extra, event.TypedAtKey)
		if len(e.Extra) == 0 {
			e.Extra = nil
		}
	}
	// --- prompt: words and "new" (docs/RULE.md, definition 2), decided once, here ---
	var kept checks.Text
	if e.Kind == event.Prompt && e.Text != "" && !chain.IsCommitted(e.Text) {
		e.Text = scrub.Text(e.Text)
		norm := ledger.Normalize(e.Text)
		e.Words = len(strings.Fields(norm))
		before, err := p.Store.SentBefore(e.Project, norm)
		if err == nil {
			isNew := ledger.NewPrompt(e.Text, before)
			e.New = &isNew
			// the prompt check: every prompt is kept here and its hash goes
			// to the server, with the check on or off; one that is not an
			// exact repeat, whatever its length, commits to its text in the
			// log too, and is checked (now, or when it is asked for)
			kept = checks.NewText(e.Text, before)
			if !before {
				e.CheckHash = kept.Hash
			}
		}
	}
	root := ""
	if e.Kind == event.Prompt || e.Kind == event.AgentStop || e.Kind == event.SessionEnd {
		for _, i := range infos {
			if i.Key == key {
				root = i.Root
			}
		}
		if root == "" && cwdInfo.Key == key {
			root = cwdInfo.Root
		}
		if root == "" && key != "" {
			if pr, ok, _ := p.Store.GetProject(key); ok {
				root = pr.Root
			}
		}
	}
	switch e.Kind {
	case event.Prompt:
		p.promptFacts(&e)
		p.queuedFor(&e, typed)
	case event.AgentStop:
		p.stopFacts(&e, root)
	}
	if p.NoText {
		p.dropText(&e)
	}
	stored, err := p.Store.Append(e)
	if err == nil {
		var snap *promptcheck.Snapshot
		if p.Snap != nil && root != "" && (e.Kind == event.Prompt || e.Kind == event.AgentStop || e.Kind == event.SessionEnd) {
			snap = p.Snap(root)
		}
		var info *promptcheck.Project
		if p.Facts != nil && root != "" && e.Kind == event.Prompt && kept.Text != "" {
			info = p.Facts(root, projectName(p.Store.Label(key)))
		}
		if terr := checks.Track(p.Store, stored, kept, reply, snap, info, now); terr != nil {
			logx.Printf("prompt check: keeping the turn failed: %v", terr)
		}
	}
	return stored, err
}

// projectName is the name a check gives the project, from the label its
// folder goes by: a public repo's own name, else the folder's (the same
// one a sync sends).
func projectName(key string) string {
	if n := event.FolderName(key); n != "" {
		return n
	}
	if event.IsPublicForge(key) {
		return key[strings.LastIndex(key, "/")+1:]
	}
	return ""
}

// dropText keeps no prompt text or command, only their commitments. The
// chain commits to text only as its hash, so nothing about the chain changes.
func (p *Pipeline) dropText(e *event.Event) {
	if h, err := p.Store.Hide(*e); err == nil {
		*e = h
	} else {
		*e = chain.Hide(*e) // drops what it cannot commit to; never stores text
	}
}

func nameOf(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[i+1:]
	}
	return strings.TrimPrefix(key, "local:")
}
