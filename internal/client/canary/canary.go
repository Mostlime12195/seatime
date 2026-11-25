// Package canary notices a harness that is being used while its hooks are
// silent (a harness update dropped or broke them): its own session files
// changed in the last 10 minutes, yet no hook event from it arrived. The
// daemon then repairs the hooks and fills the gap from the session files,
// with every filled event labelled "backfilled".
package canary

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"seatime/internal/client/store"
	"seatime/pkg/event"
)

const (
	// Window: session files changed this recently mean the harness is in use.
	Window = 10 * time.Minute
	// Slack: a hook event this close to the file change counts as heard.
	Slack = 2 * time.Minute
	// maxFiles bounds the stat calls per harness per check.
	maxFiles = 3000
)

// Probe describes where a harness keeps its session files.
type Probe struct {
	Harness string // event.Harness its hooks write
	Label   string // human name
	// Files lists candidate session files under home.
	Files func(home string, now time.Time) []string
	// Backfill turns session-file lines in (from, to] into events; nil = none.
	Backfill func(path string, from, to time.Time) []event.Event
}

// Probes are the harnesses checked: the ones with hooks and a session log.
func Probes() []Probe {
	return []Probe{
		{Harness: "claude-code", Label: "Claude Code", Files: claudeFiles, Backfill: ClaudeBackfill},
		{Harness: "codex", Label: "Codex", Files: codexFiles},
	}
}

func claudeFiles(home string, _ time.Time) []string {
	m, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	return m
}

func codexFiles(home string, now time.Time) []string {
	var out []string
	for _, d := range []time.Time{now, now.AddDate(0, 0, -1)} {
		m, _ := filepath.Glob(filepath.Join(home, ".codex", "sessions", d.Format("2006"), d.Format("01"), d.Format("02"), "*.jsonl"))
		out = append(out, m...)
	}
	return out
}

// Silent is a harness in use whose hooks are not being heard.
type Silent struct {
	Harness  string    `json:"harness"`
	Label    string    `json:"label"`
	Active   time.Time `json:"active"`    // newest session-file change
	LastHook time.Time `json:"last_hook"` // newest hook event (zero = none in a day)
	Files    []string  `json:"-"`         // session files changed in the window
}

// Check returns the harnesses that look silent at now.
func Check(st *store.Store, home string, now time.Time, probes []Probe) ([]Silent, error) {
	var out []Silent
	for _, p := range probes {
		files := p.Files(home, now)
		if len(files) > maxFiles {
			files = files[len(files)-maxFiles:]
		}
		var newest time.Time
		var recent []string
		for _, f := range files {
			fi, err := os.Stat(f)
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			mt := fi.ModTime()
			if now.Sub(mt) <= Window && !mt.After(now.Add(time.Minute)) {
				recent = append(recent, f)
				if mt.After(newest) {
					newest = mt
				}
			}
		}
		if newest.IsZero() {
			continue
		}
		last, err := lastHook(st, p.Harness, now.Add(-24*time.Hour))
		if err != nil {
			return out, err
		}
		if !last.IsZero() && !newest.After(last.Add(Slack)) {
			continue // heard recently enough
		}
		sort.Strings(recent)
		out = append(out, Silent{Harness: p.Harness, Label: p.Label, Active: newest, LastHook: last, Files: recent})
	}
	return out, nil
}

// lastHook is the newest non-backfilled event from harness since.
func lastHook(st *store.Store, harness string, since time.Time) (time.Time, error) {
	evs, err := st.Events(store.Query{Since: since, Desc: true, Limit: 2000})
	if err != nil {
		return time.Time{}, err
	}
	var last time.Time
	for _, e := range evs {
		if e.Harness == harness && !e.Backfilled && e.Extra[ExtraBackfilled] == nil && e.TS.After(last) {
			last = e.TS
		}
	}
	return last, nil
}

// ExtraBackfilled marks an event recovered from session files.
const ExtraBackfilled = "backfilled"

// ClaudeBackfill reads Claude Code transcript lines (one JSON object per line:
// type user|assistant, ISO timestamp, sessionId, cwd, message.content) and
// returns prompt + agent_start for each typed prompt and agent_stop at the
// last assistant line before the next prompt, all within (from, to].
func ClaudeBackfill(path string, from, to time.Time) []event.Event {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var out []event.Event
	var open *event.Event // last assistant line of the current turn
	flush := func() {
		if open != nil {
			out = append(out, *open)
			open = nil
		}
	}
	mark := func(e event.Event) event.Event {
		e.Harness = "claude-code"
		e.Extra = map[string]any{ExtraBackfilled: true}
		e.Backfilled = true
		return e
	}
	for sc.Scan() {
		var l struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			SessionID string          `json:"sessionId"`
			CWD       string          `json:"cwd"`
			Sidechain bool            `json:"isSidechain"`
			Meta      bool            `json:"isMeta"`
			Message   json.RawMessage `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Sidechain || l.Meta {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, l.Timestamp)
		if err != nil || !ts.After(from) || ts.After(to) {
			continue
		}
		base := event.Event{TS: ts, SessionID: l.SessionID, CWD: l.CWD}
		switch l.Type {
		case "user":
			text, ok := typedPrompt(l.Message)
			if !ok {
				continue // a tool result, not the person
			}
			flush()
			p := mark(base)
			p.Kind, p.Text = event.Prompt, text
			out = append(out, p)
			s := mark(base)
			s.Kind = event.AgentStart
			out = append(out, s)
			stop := mark(base)
			stop.Kind = event.AgentStop
			open = &stop
		case "assistant":
			if open != nil {
				open.TS = ts
			}
		}
	}
	flush()
	return out
}

// typedPrompt extracts what the person typed from a user message; tool
// results and command wrappers are not prompts.
func typedPrompt(raw json.RawMessage) (string, bool) {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil || len(m.Content) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		s = strings.TrimSpace(s)
		return s, s != "" && !strings.HasPrefix(s, "<command-") && !strings.HasPrefix(s, "<local-command")
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return "", false
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			return "", false
		case "text":
			parts = append(parts, b.Text)
		}
	}
	s = strings.TrimSpace(strings.Join(parts, "\n"))
	return s, s != ""
}
