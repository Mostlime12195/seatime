// Package amp is the Sourcegraph Amp adapter (CLI and editor extension). Amp has no
// shell hooks (its plugin API is in-process TypeScript, https://ampcode.com/docs/customize/plugins,
// payloads not fully documented), so this tails the thread files instead:
//
//	$AMP_DATA_DIR/threads/T-<uuid>.json  (default ~/.local/share/amp/threads; per-thread JSON, rewritten)
//	  {"id":"T-..","created":<ms>,"messages":[{"role":"user"|"assistant","content":[{"type":"text","text":..}|
//	   {"type":"tool_use","name":..,"input":{..}}|{"type":"tool_result"..}], "meta":{"sentAt":<ms>}}],
//	   "env":{"initial":{"trees":[{"uri":"file:///work/app"}]}}}
//
// The layout comes from third-party parsers (lore-cli, ccusage; see docs/ADAPTERS.md), not
// from Amp's docs, so every field is read defensively. secrets.json in that folder is never
// opened. Mapping: user message with text => prompt + agent_start (time = meta.sentAt),
// assistant tool_use of edit_file/create_file/undo_edit => write tool event, a text-only
// assistant message => agent_stop. Same no-backfill rules as the Cline family.
package amp

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"seatime/internal/adapters/common"
	"seatime/internal/adapters/runclass"
	"seatime/pkg/event"
)

const name = "amp"

type Adapter struct {
	common.WatchOnly
	// Dir overrides the threads folder (tests).
	Dir string
}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return name }

func (a *Adapter) threadsDir() string {
	if a.Dir != "" {
		return a.Dir
	}
	base := os.Getenv("AMP_DATA_DIR")
	if base == "" || os.Getenv("SEATIME_FAKE_HOME") != "" {
		base = filepath.Join(common.Home(), ".local", "share", "amp")
	}
	return filepath.Join(base, "threads")
}

func (a *Adapter) Detect() bool {
	st, err := os.Stat(a.threadsDir())
	return err == nil && st.IsDir()
}

func (a *Adapter) Watch(ctx context.Context, stateDir string, sink common.Sink) error {
	tr := a.NewTracker(stateDir)
	return common.PollLoop(ctx, 5*time.Second, func(now time.Time) { tr.Poll(now, sink) })
}

type Tracker struct {
	a       *Adapter
	offsets *common.Offsets
	mtimes  map[string]time.Time
}

func (a *Adapter) NewTracker(stateDir string) *Tracker {
	return &Tracker{a: a, offsets: common.LoadOffsets(stateDir), mtimes: map[string]time.Time{}}
}

type block struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

type thread struct {
	ID       string `json:"id"`
	Created  int64  `json:"created"`
	Messages []struct {
		Role    string  `json:"role"`
		Content []block `json:"content"`
		Meta    struct {
			SentAt int64 `json:"sentAt"`
		} `json:"meta"`
	} `json:"messages"`
	Env struct {
		Initial struct {
			Trees []struct {
				URI string `json:"uri"`
			} `json:"trees"`
		} `json:"initial"`
	} `json:"env"`
}

func (t *Tracker) Poll(now time.Time, sink common.Sink) {
	ents, err := os.ReadDir(t.a.threadsDir())
	if err != nil {
		return
	}
	for _, d := range ents {
		n := d.Name()
		if d.IsDir() || !strings.HasPrefix(n, "T-") || !strings.HasSuffix(n, ".json") {
			continue
		}
		t.thread(filepath.Join(t.a.threadsDir(), n), now, sink)
	}
	t.offsets.Flush()
}

func (t *Tracker) thread(p string, now time.Time, sink common.Sink) {
	st, err := os.Stat(p)
	if err != nil || st.ModTime().Equal(t.mtimes[p]) || st.Size() > 64<<20 {
		return
	}
	t.mtimes[p] = st.ModTime()
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var th thread
	if json.Unmarshal(b, &th) != nil {
		delete(t.mtimes, p)
		return
	}
	key := name + ":" + filepath.Base(p)
	done, known := t.offsets.Get(key)
	if !known {
		done = int64(len(th.Messages))
		if len(th.Messages) > 0 && now.Sub(msgTime(th.Messages[0].Meta.SentAt, th.Created, st.ModTime())) < 2*time.Minute {
			done = 0
		}
	}
	if int(done) > len(th.Messages) {
		done = 0
	}
	id := th.ID
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(p), ".json")
	}
	cwd := ""
	if len(th.Env.Initial.Trees) > 0 {
		cwd = fileURIPath(th.Env.Initial.Trees[0].URI)
	}
	base := event.Event{Harness: name, SessionID: name + ":" + id, CWD: cwd}
	for i := int(done); i < len(th.Messages); i++ {
		m := th.Messages[i]
		ts := msgTime(m.Meta.SentAt, th.Created, st.ModTime())
		if common.Stale(ts, now) {
			continue
		}
		emit := func(k event.Kind, mod func(*event.Event)) {
			e := base
			e.Kind, e.TS = k, ts
			if mod != nil {
				mod(&e)
			}
			_ = sink.Emit(e)
		}
		switch m.Role {
		case "user":
			var text []string
			for _, c := range m.Content {
				if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
					text = append(text, c.Text)
				}
			}
			if len(text) > 0 { // tool results come back as user messages without text
				emit(event.Prompt, func(e *event.Event) { e.Text = strings.Join(text, "\n") })
				emit(event.AgentStart, nil)
			}
		case "assistant":
			tools, hasText := 0, false
			for _, c := range m.Content {
				switch c.Type {
				case "text":
					hasText = hasText || strings.TrimSpace(c.Text) != ""
				case "tool_use":
					tools++
					t.tool(c, base, ts, sink)
				}
			}
			if tools == 0 && hasText {
				emit(event.AgentStop, nil)
			}
		}
	}
	t.offsets.Set(key, int64(len(th.Messages)))
}

var writeTools = map[string]bool{"edit_file": true, "create_file": true, "undo_edit": true, "apply_patch": true}

func (t *Tracker) tool(c block, base event.Event, ts time.Time, sink common.Sink) {
	e := base
	e.Kind, e.TS, e.Tool = event.Tool, ts, c.Name
	lt := strings.ToLower(c.Name)
	var cmd string
	switch {
	case writeTools[lt]:
		e.Write = true
		if f := common.Str(c.Input, "path", "file_path"); f != "" {
			e.Files = []string{f}
		}
	case lt == "bash" || lt == "shell_command":
		cmd = common.Str(c.Input, "cmd", "command")
		e.Command = cmd
	}
	_ = sink.Emit(e)
	if cmd != "" {
		if k, ok := runclass.Classify(cmd); ok {
			b := base
			b.TS = ts
			_ = common.EmitRun(sink, b, cmd, k, nil, "")
		}
	}
}

func msgTime(sentAt, created int64, mtime time.Time) time.Time {
	switch {
	case sentAt > 0:
		return time.UnixMilli(sentAt).UTC()
	case created > 0:
		return time.UnixMilli(created).UTC()
	}
	return mtime.UTC()
}

// fileURIPath turns file:///work/app (or file:///C:/work/app) into a local path.
func fileURIPath(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "file" {
		return ""
	}
	path := p.Path
	if runtime.GOOS == "windows" {
		path = strings.TrimPrefix(path, "/")
	}
	return filepath.FromSlash(path)
}
