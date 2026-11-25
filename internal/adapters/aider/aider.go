// Package aider is the Aider adapter. Aider has no hooks, so this is a Watcher that
// tails each project's chat history, .aider.chat.history.md (default name and location,
// https://aider.chat/docs/config/options.html; format from aider/io.py):
//
//	# aider chat started at 2026-11-19 14:03:11     session header (local time, no zone)
//	#### the user's prompt, every line prefixed      user input
//	assistant reply as plain Markdown               assistant text
//	> Applied edit to src/app.py                    tool/info output, blockquoted
//
// The file has no per-message timestamps, so events are stamped with the time the
// watcher saw them (poll interval 5 s): a new "#### " block = prompt + agent_start; once
// assistant text arrived and the file stayed quiet for 4 s = agent_stop. "> Applied edit
// to <file>" lines become write events (that is how aider reports an applied edit; seen in
// the source, not verified on a live install) so the project follows the files.
//
// Project discovery: aider writes the history into the directory it was started in, so we
// look for the file under the usual code folders (depth <= 3, bounded work, rescanned every
// 3 minutes) plus any directories in SEATIME_AIDER_DIRS. History already on disk is never
// replayed: a newly discovered file starts at its current end.
package aider

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"seatime/internal/adapters/common"
	"seatime/internal/client/protected"
	"seatime/pkg/event"
)

const name = "aider"
const historyFile = ".aider.chat.history.md"

const (
	pollEvery   = 5 * time.Second
	quietFor    = 4 * time.Second
	giveUpAfter = 5 * time.Minute
	rescanEvery = 3 * time.Minute
	maxDirs     = 6000
)

type Adapter struct {
	common.WatchOnly
	// Roots overrides the directories searched (tests); nil = defaults + SEATIME_AIDER_DIRS.
	Roots []string
}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return name }

func (*Adapter) Detect() bool {
	if _, err := exec.LookPath("aider"); err == nil {
		return true
	}
	for _, p := range []string{".aider.conf.yml", ".aider"} {
		if _, err := os.Stat(filepath.Join(common.Home(), p)); err == nil {
			return true
		}
	}
	return false
}

func (a *Adapter) roots() []string {
	if a.Roots != nil {
		return a.Roots
	}
	h := common.Home()
	r := []string{h}
	for _, d := range []string{"code", "projects", "dev", "src", "repos", "work", "github", "git", "Documents", "Desktop", "source"} {
		r = append(r, filepath.Join(h, d))
	}
	for _, d := range filepath.SplitList(os.Getenv("SEATIME_AIDER_DIRS")) {
		if d != "" {
			r = append(r, d)
		}
	}
	return r
}

var skipDir = map[string]bool{"node_modules": true, "vendor": true, "target": true, "dist": true, "build": true,
	"appdata": true, "library": true, "snap": true, "venv": true, "__pycache__": true, "site-packages": true}

// Discover finds history files below the roots (bounded).
func Discover(roots []string) []string {
	seen := map[string]bool{}
	var out []string
	budget := maxDirs
	var walk func(dir string, depth int)
	home := common.Home()
	walk = func(dir string, depth int) {
		if budget <= 0 || seen[dir] || protected.Blocked(home, dir) {
			return // never list macOS-protected folders from the daemon
		}
		seen[dir] = true
		budget--
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if e.Name() == historyFile && e.Type().IsRegular() {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
		if depth >= 3 {
			return
		}
		for _, e := range ents {
			n := e.Name()
			if !e.IsDir() || strings.HasPrefix(n, ".") || skipDir[strings.ToLower(n)] {
				continue
			}
			walk(filepath.Join(dir, n), depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	return out
}

type file struct {
	off         int64
	header      string
	pending     bool // prompt seen, agent_stop not yet emitted
	assistant   bool // assistant text seen since the prompt
	lastGrowth  time.Time
	partialTail []byte
}

// Tracker holds the watcher state (exported for tests).
type Tracker struct {
	a        *Adapter
	offsets  *common.Offsets
	files    map[string]*file
	lastScan time.Time
}

func (a *Adapter) NewTracker(stateDir string) *Tracker {
	return &Tracker{a: a, offsets: common.LoadOffsets(stateDir), files: map[string]*file{}}
}

func (a *Adapter) Watch(ctx context.Context, stateDir string, sink common.Sink) error {
	tr := a.NewTracker(stateDir)
	return common.PollLoop(ctx, pollEvery, func(now time.Time) { tr.Poll(now, sink) })
}

// Poll processes new history bytes and closes quiet turns.
func (t *Tracker) Poll(now time.Time, sink common.Sink) {
	if now.Sub(t.lastScan) >= rescanEvery {
		t.lastScan = now
		for _, p := range Discover(t.a.roots()) {
			if _, ok := t.files[p]; ok {
				continue
			}
			f := &file{}
			if off, known := t.offsets.Get(p); known {
				f.off = off
			} else if st, err := os.Stat(p); err == nil {
				f.off = st.Size() // never replay history
				// a session that is being written right now; an mtime ahead of now
				// (clock jump, skew) is not "recent" and must not replay history
				if d := now.Sub(st.ModTime()); d >= -2*time.Second && d < 30*time.Second {
					f.off = 0
				}
			}
			t.files[p] = f
		}
	}
	for p, f := range t.files {
		t.poll(p, f, now, sink)
	}
	t.offsets.Flush()
}

var appliedRe = regexp.MustCompile(`^> Applied edit to (.+?)\s*$`)

func (t *Tracker) poll(path string, f *file, now time.Time, sink common.Sink) {
	st, err := os.Stat(path)
	if err != nil {
		delete(t.files, path)
		return
	}
	if st.Size() < f.off { // truncated or replaced
		f.off, f.partialTail = 0, nil
	}
	if st.Size() > f.off {
		if chunk := readFrom(path, f.off, st.Size()); chunk != nil {
			f.off += int64(len(chunk))
			t.offsets.Set(path, f.off)
			t.consume(path, f, append(f.partialTail, chunk...), now, sink)
		}
	}
	if f.pending && f.assistant && now.Sub(f.lastGrowth) >= quietFor || f.pending && now.Sub(f.lastGrowth) >= giveUpAfter {
		f.pending, f.assistant = false, false
		_ = sink.Emit(f.base(path, f.lastGrowth, event.AgentStop))
	}
}

func readFrom(path string, from, to int64) []byte {
	fh, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer fh.Close()
	if to-from > 4<<20 {
		from = to - 4<<20
	}
	buf := make([]byte, to-from)
	n, _ := fh.ReadAt(buf, from)
	return buf[:n]
}

func (f *file) base(path string, ts time.Time, k event.Kind) event.Event {
	sum := sha1.Sum([]byte(path))
	return event.Event{Kind: k, Harness: name, TS: ts, CWD: filepath.Dir(path),
		SessionID: "aider:" + hex.EncodeToString(sum[:4]) + ":" + strings.NewReplacer(" ", "T", ":", "").Replace(f.header)}
}

// consume parses complete lines; an unterminated tail waits for the next poll.
func (t *Tracker) consume(path string, f *file, data []byte, now time.Time, sink common.Sink) {
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		f.partialTail = append([]byte(nil), data[i+1:]...)
		data = data[:i+1]
	} else {
		f.partialTail = append([]byte(nil), data...)
		return
	}
	f.lastGrowth = now
	var prompt []string
	inPrompt := false
	flush := func() {
		if !inPrompt {
			return
		}
		inPrompt = false
		text := strings.TrimSpace(strings.Join(prompt, "\n"))
		prompt = nil
		if text == "" || text == "<blank>" {
			return
		}
		if f.pending { // previous turn never saw its end
			_ = sink.Emit(f.base(path, now, event.AgentStop))
		}
		e := f.base(path, now, event.Prompt)
		e.Text = text
		_ = sink.Emit(e)
		_ = sink.Emit(f.base(path, now, event.AgentStart))
		f.pending, f.assistant = true, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "#### ") || line == "####":
			inPrompt = true
			prompt = append(prompt, strings.TrimPrefix(strings.TrimPrefix(line, "####"), " "))
			continue
		case strings.HasPrefix(line, "# aider chat started at "):
			flush()
			f.header = strings.TrimPrefix(line, "# aider chat started at ")
			continue
		}
		flush()
		if m := appliedRe.FindStringSubmatch(line); m != nil {
			e := f.base(path, now, event.Tool)
			e.Tool, e.Write = "edit", true
			p := m[1]
			if !filepath.IsAbs(p) {
				p = filepath.Join(filepath.Dir(path), p)
			}
			e.Files = []string{p}
			_ = sink.Emit(e)
			continue
		}
		if f.pending && strings.TrimSpace(line) != "" && !strings.HasPrefix(line, ">") {
			f.assistant = true
		}
	}
	flush()
}
