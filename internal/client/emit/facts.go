package emit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"seatime/internal/client/presence"
	"seatime/internal/client/store"
	"seatime/pkg/chain"
	"seatime/pkg/event"
)

// The facts below are numbers and yes/no only. None of them changes hours
// on its own; they are part of the person's records.

// promptFacts adds a prompt's session shape and, when the pipeline reads
// presence (the hook does), its presence facts.
func (p *Pipeline) promptFacts(e *event.Event) {
	if e.SessionID != "" {
		_, _, n, _ := p.Store.SeqRange(store.Query{Session: e.SessionID, Kinds: []event.Kind{event.Prompt}})
		e.SessionPrompts = int(n) + 1
	}
	if p.Presence != nil && e.Presence == "" {
		f := p.Presence()
		e.Presence, e.InputBeforeMS, e.TerminalIdleS, e.FrontWindow = f.Presence, f.InputBeforeMS, f.TerminalIdleS, f.FrontWindow
	}
}

// queuedFor sets QueuedMS on a prompt the AI tool held in its queue, typed
// at typed (from the tool's log). The typed time must fall after the
// session's previous prompt, inside the run the person was waiting on, and
// at least a second before the prompt was sent; anything else is a stale
// match in the log and leaves the prompt as sent.
func (p *Pipeline) queuedFor(e *event.Event, typed time.Time) {
	e.QueuedMS = 0
	if typed.IsZero() || e.TS.Sub(typed) < time.Second {
		return
	}
	if e.SessionID != "" {
		prev, err := p.Store.Events(store.Query{Session: e.SessionID, Kinds: []event.Kind{event.Prompt}, Desc: true, Limit: 1})
		if err != nil || len(prev) > 0 && typed.Before(prev[0].TS) {
			return
		}
	}
	e.QueuedMS = e.TS.Sub(typed).Milliseconds()
}

// stopFacts adds what the run that ends at this stop did, from the
// session's records since its last prompt, whether that prompt is in the
// AI tool's own log, and, when the pipeline may look at the repository (the
// hook may; the daemon may not), git's counts.
func (p *Pipeline) stopFacts(e *event.Event, root string) {
	if e.SessionID == "" {
		return
	}
	evs, err := p.Store.Events(store.Query{Session: e.SessionID, Desc: true, Limit: 2000})
	if err != nil {
		return
	}
	var run []event.Event
	var prompt *event.Event
	for i := range evs { // newest first
		if evs[i].Kind == event.Prompt {
			prompt = &evs[i]
			break
		}
		if evs[i].Kind == event.AgentStop {
			return // a stop with no prompt since the last one: no new run
		}
		run = append(run, evs[i])
	}
	if prompt == nil {
		return
	}
	if f := RunFactsOf(run, e.Project); f != (event.RunFacts{}) {
		e.Run = &f
	}
	if path, _ := e.Extra["transcript_path"].(string); path != "" {
		e.PromptInLog = inLog(path, prompt.Text)
	}
	if p.Repo != nil && root != "" {
		e.Git = p.Repo(root)
	}
}

// RunFactsOf counts a run's tool calls by kind and the files and lines it
// wrote. A file is inside the project when the write was attributed to it
// and the path is relative to it; anything else (another project, a path
// under ~ or an absolute one, as the pipeline stores them) is outside.
func RunFactsOf(run []event.Event, project string) event.RunFacts {
	var f event.RunFacts
	in, out := map[string]bool{}, map[string]bool{}
	for _, e := range run {
		if e.Kind != event.Tool {
			continue
		}
		t := strings.ToLower(e.Tool)
		switch {
		case e.Write:
			f.Writes++
			f.LinesWritten += e.Lines
			for _, file := range e.Files {
				if chain.IsCommitted(file) {
					continue
				}
				if e.Project != project || strings.HasPrefix(file, "/") || strings.HasPrefix(file, "~") || strings.Contains(file, ":\\") {
					out[e.Project+"\x00"+file] = true
				} else {
					in[file] = true
				}
			}
		case e.Command != "" || e.ExitCode != nil:
			f.Commands++
			if e.ExitCode != nil && *e.ExitCode != 0 {
				f.CommandsFailed++
			}
		case t == "grep" || t == "glob" || strings.Contains(t, "search") && !strings.Contains(t, "web") || t == "find":
			f.Searches++
		case t == "read" || t == "view" || t == "ls" || strings.HasPrefix(t, "read_") || t == "list_dir" || t == "notebookread":
			f.Reads++
		case strings.HasPrefix(t, "web") || t == "fetch":
			f.Web++
		default:
			f.Other++
		}
	}
	f.FilesInside, f.FilesOutside = len(in), len(out)
	return f
}

// inLog reports whether the prompt's text is in the tool's session log (the
// last 8 MB of it), as the JSON string the log stores it as. nil when the
// text is not kept here, or was changed on the way in (a secret blanked).
func inLog(path, text string) *bool {
	if text == "" || chain.IsCommitted(text) || strings.Contains(text, "[REDACTED") {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > 8<<20 {
		f.Seek(fi.Size()-8<<20, io.SeekStart)
	}
	b, err := io.ReadAll(io.LimitReader(f, 8<<20))
	if err != nil {
		return nil
	}
	head := text
	for len(head) > 0 && utf8.RuneCountInString(head) > 80 {
		_, size := utf8.DecodeLastRuneInString(head)
		head = head[:len(head)-size]
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(head)
	quoted := strings.TrimSuffix(strings.TrimSpace(buf.String()), `"`) // the open string: the log may run on
	found := bytes.Contains(b, []byte(quoted))
	return &found
}

// GitRepo reads git's counts for the uncommitted changes against HEAD and
// HEAD itself (the repository's trail), with a short time
// limit. nil when root is not a git checkout or git is slow or missing.
func GitRepo(ctx context.Context) func(root string) *event.GitFacts {
	return func(root string) *event.GitFacts {
		ctx, cancel := context.WithTimeout(ctx, 600*time.Millisecond)
		defer cancel()
		head, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
		if err != nil {
			return nil
		}
		g := &event.GitFacts{Head: strings.TrimSpace(string(head))}
		if out, err := exec.CommandContext(ctx, "git", "-C", root, "diff", "--numstat", "HEAD").Output(); err == nil {
			*g = ParseNumstat(out, g.Head)
		}
		return g
	}
}

// ParseNumstat sums `git diff --numstat` ("added<TAB>removed<TAB>path";
// binary files show "-" and count as changed with no lines).
func ParseNumstat(out []byte, head string) event.GitFacts {
	g := event.GitFacts{Head: head}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Split(l, "\t")
		if len(f) < 3 {
			continue
		}
		g.FilesChanged++
		a, _ := strconv.Atoi(f[0])
		d, _ := strconv.Atoi(f[1])
		g.Insertions += a
		g.Deletions += d
	}
	return g
}

// PresenceOf adapts a presence.Probe to the pipeline.
func PresenceOf(ctx context.Context, pr presence.Probe) func() presence.Facts {
	return func() presence.Facts {
		ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		return pr.AtPrompt(ctx)
	}
}
