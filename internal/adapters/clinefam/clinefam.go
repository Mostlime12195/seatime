// Package clinefam adapts the Cline family of VS Code agent extensions: Cline, Roo Code
// and Kilo Code (the legacy, Roo-derived v5 line). They share one on-disk task format
// under the editor's globalStorage (verified through third-party parsers, see
// docs/ADAPTERS.md; the extensions themselves expose no stable hook API for the VS Code
// extension):
//
//	<User>/globalStorage/<extension id>/tasks/<taskId>/ui_messages.json   JSON array
//	  {"ts": <ms epoch>, "type":"say"|"ask", "say":"task"|"user_feedback"|"text"|"tool"|...,
//	   "ask":"completion_result"|"followup"|"command"|..., "text": "..."}
//	<...>/tasks/<taskId>/api_conversation_history.json   (cwd is recovered from the
//	  "Current Working Directory (<path>)" line Cline puts in its environment details)
//
// Extension ids: Cline saoudrizwan.claude-dev, Roo rooveterinaryinc.roo-cline (Roo shut down
// in May 2026 but installs keep working), Kilo kilocode.kilo-code. Kilo's rebuilt extension
// is opencode-based and handled by the `kilo` plugin adapter instead.
//
// Mapping (each message has its own ts, which we use as the event time):
//
//	say task | user_feedback | text at index 0   -> prompt + agent_start
//	say tool {tool: edited/created/diff/...}     -> write tool event (+ file)
//	ask command                                  -> tool + run event (no exit code available)
//	ask completion_result | followup | plan/act respond -> agent_stop (control is the human's)
//
// The file is rewritten as a whole, so we poll mtimes (5 s), re-read changed files and
// keep, per task, the newest message ts processed (a restore or an edited message rewrites
// the file shorter: the resent prompt has a new ts and is emitted, older ones are not
// replayed). A task seen for the first time is not replayed unless it was created in the
// last 2 minutes; events older than 15 minutes are never emitted.
package clinefam

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"seatime/internal/adapters/common"
	"seatime/internal/adapters/runclass"
	"seatime/pkg/event"
)

// Variant is one extension of the family.
type Variant struct {
	Name string // adapter / harness name
	Ext  string // VS Code extension id (globalStorage folder)
}

var (
	Cline = Variant{"cline", "saoudrizwan.claude-dev"}
	Roo   = Variant{"roo-code", "rooveterinaryinc.roo-cline"}
	Kilo  = Variant{"kilo-code", "kilocode.kilo-code"}
)

type Adapter struct {
	common.WatchOnly
	V Variant
	// Dirs overrides the globalStorage folders searched (tests).
	Dirs []string
}

func New(v Variant) *Adapter { return &Adapter{V: v} }

func (a *Adapter) Name() string { return a.V.Name }

func (a *Adapter) storageDirs() []string {
	if a.Dirs != nil {
		return a.Dirs
	}
	var out []string
	for _, u := range common.VSCodeUserDirs() {
		out = append(out, filepath.Join(u, "globalStorage", a.V.Ext))
	}
	return out
}

func (a *Adapter) Detect() bool {
	for _, d := range a.storageDirs() {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

func (a *Adapter) Watch(ctx context.Context, stateDir string, sink common.Sink) error {
	tr := a.NewTracker(stateDir)
	return common.PollLoop(ctx, 5*time.Second, func(now time.Time) { tr.Poll(now, sink) })
}

type Tracker struct {
	a       *Adapter
	offsets *common.Offsets
	mtimes  map[string]time.Time
	cwds    map[string]string
}

func (a *Adapter) NewTracker(stateDir string) *Tracker {
	return &Tracker{a: a, offsets: common.LoadOffsets(stateDir), mtimes: map[string]time.Time{}, cwds: map[string]string{}}
}

// Poll scans all task folders for new messages.
func (t *Tracker) Poll(now time.Time, sink common.Sink) {
	for _, root := range t.a.storageDirs() {
		tasks, err := os.ReadDir(filepath.Join(root, "tasks"))
		if err != nil {
			continue
		}
		for _, d := range tasks {
			if !d.IsDir() {
				continue
			}
			dir := filepath.Join(root, "tasks", d.Name())
			t.task(dir, d.Name(), now, sink)
		}
	}
	t.offsets.Flush()
}

type message struct {
	TS   float64 `json:"ts"`
	Type string  `json:"type"`
	Say  string  `json:"say"`
	Ask  string  `json:"ask"`
	Text string  `json:"text"`
}

func (t *Tracker) task(dir, id string, now time.Time, sink common.Sink) {
	p := filepath.Join(dir, "ui_messages.json")
	st, err := os.Stat(p)
	if err != nil || st.ModTime().Equal(t.mtimes[p]) {
		return
	}
	t.mtimes[p] = st.ModTime()
	if st.Size() > 32<<20 {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var msgs []message
	if json.Unmarshal(b, &msgs) != nil { // mid-rewrite: try again on the next mtime change
		delete(t.mtimes, p)
		return
	}
	key := t.a.V.Name + ":" + p
	// Messages are processed by their own ts, not their position: a
	// "Restore Task" or an edited message rewrites the file shorter and
	// appends the resent prompt with a new ts, so a position count would
	// skip the resent prompt or, reset to 0, replay old ones. tsKey holds
	// the newest ts already processed.
	tsKey := key + "#ts"
	doneTS, haveTS := t.offsets.Get(tsKey)
	if !haveTS {
		done, known := t.offsets.Get(key) // a count from before the ts was kept
		switch {
		case !known && now.Sub(firstTS(msgs)) < 2*time.Minute:
			doneTS = 0 // a task that just started
		case known && int(done) <= len(msgs):
			doneTS = maxTS(msgs[:done])
		default: // a task seen for the first time, or a count from before a rewrite: no replay
			doneTS = maxTS(msgs)
		}
	}
	newest := doneTS
	cwd := t.cwd(dir, id)
	base := event.Event{Harness: t.a.V.Name, SessionID: t.a.V.Name + ":" + id, CWD: cwd}
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if int64(m.TS) <= doneTS {
			continue // processed already (also after a restore rewrote the file)
		}
		newest = max(newest, int64(m.TS))
		ts := time.UnixMilli(int64(m.TS)).UTC()
		if m.TS <= 0 || common.Stale(ts, now) || ts.After(now.Add(time.Minute)) {
			continue
		}
		e := base
		e.TS = ts
		emit := func(k event.Kind, mod func(*event.Event)) {
			x := e
			x.Kind = k
			if mod != nil {
				mod(&x)
			}
			_ = sink.Emit(x)
		}
		switch {
		case m.Type == "say" && (m.Say == "task" || m.Say == "user_feedback" || (m.Say == "text" && i == 0)):
			if strings.TrimSpace(m.Text) == "" {
				continue
			}
			emit(event.Prompt, func(x *event.Event) { x.Text = m.Text })
			emit(event.AgentStart, nil)
		case m.Type == "say" && m.Say == "tool":
			var tl struct{ Tool, Path string }
			if json.Unmarshal([]byte(m.Text), &tl) == nil && writeTool(tl.Tool) {
				emit(event.Tool, func(x *event.Event) {
					x.Tool, x.Write = tl.Tool, true
					if tl.Path != "" {
						x.Files = []string{tl.Path}
					}
				})
			}
		case m.Type == "ask" && m.Ask == "command":
			cmd := strings.TrimSpace(m.Text)
			if cmd == "" {
				continue
			}
			emit(event.Tool, func(x *event.Event) { x.Tool, x.Command = "execute_command", cmd })
			if k, ok := runclass.Classify(cmd); ok {
				_ = common.EmitRun(sink, withTS(base, ts), cmd, k, nil, "")
			}
		case m.Type == "ask" && stopAsk[m.Ask]:
			emit(event.AgentStop, nil)
		}
	}
	t.offsets.Set(key, int64(len(msgs)))
	t.offsets.Set(tsKey, newest)
}

// maxTS is the newest message ts (ms) in msgs.
func maxTS(msgs []message) int64 {
	var m int64
	for _, x := range msgs {
		m = max(m, int64(x.TS))
	}
	return m
}

func withTS(e event.Event, ts time.Time) event.Event { e.TS = ts; return e }

var stopAsk = map[string]bool{"completion_result": true, "followup": true, "plan_mode_respond": true, "act_mode_respond": true}

func writeTool(n string) bool {
	switch n {
	case "editedExistingFile", "newFileCreated", "appliedDiff", "insertContent", "searchAndReplace", "fileDeleted":
		return true
	}
	return false
}

func firstTS(msgs []message) time.Time {
	if len(msgs) == 0 || msgs[0].TS <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(msgs[0].TS))
}

var cwdRe = regexp.MustCompile(`Current Working Directory \(([^)\n]+)\)`)

// cwd recovers the task's working directory from its API history (cached).
func (t *Tracker) cwd(dir, id string) string {
	if c, ok := t.cwds[dir]; ok {
		return c
	}
	c := ""
	if f, err := os.Open(filepath.Join(dir, "api_conversation_history.json")); err == nil {
		buf := make([]byte, 256<<10)
		n, _ := f.Read(buf)
		f.Close()
		// the JSON text escapes nothing inside the parenthesised path except backslashes
		if m := cwdRe.FindSubmatch(buf[:n]); m != nil {
			c = strings.ReplaceAll(string(m[1]), `\\`, `\`)
		}
	}
	if c != "" {
		t.cwds[dir] = c
	}
	return c
}
