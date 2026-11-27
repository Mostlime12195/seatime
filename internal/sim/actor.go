package sim

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"seatime/internal/client/emit"
)

const (
	sec = time.Second
	mn  = time.Minute
	hr  = time.Hour
)

// Actor is a person (or a bot) at one device. It turns behavior (think, type
// a prompt, watch the agent, hand-edit, run tests, commit) into the inputs the
// real client receives: Claude Code hook payloads, WakaTime heartbeats,
// `seatime emit` plugin events. Time passes on the
// world clock, so the device syncs as the daemon would.
type Actor struct {
	D   *Device
	rng *rand.Rand

	Root    string // project checkout (cwd for hooks)
	Sess    string // harness session id
	Model   string
	Harness string // "claude-code" (hooks)
	// NoHands: nobody touches the machine (a script, a loop, a farm). A
	// person's prompts, typing, runs and commits are key presses the OS
	// idle timer sees; a script's are not.
	NoHands bool

	// Jit stretches or shrinks every wait by up to that fraction, drawn
	// from jrng (a replayed recording with its gaps "humanised"); Suffix
	// is appended to every prompt (a rewording, so it is not word for word
	// the recording's).
	Jit    float64
	jrng   *rand.Rand
	Suffix string

	typedAt time.Time // the last key press during the agent's run (a queued message)

	lines        map[string]int
	cur          map[string][2]int // file -> cursor line and column, for heartbeats
	crng         *rand.Rand        // cursor moves only: a separate stream, so adding it changed no timing
	lastBeat     time.Time
	lastBeatFile string
	Away         time.Duration // declared breaks (not work)
	started      time.Time
	nSess        int
}

// NewActor binds a person to a device with a seeded RNG.
func NewActor(d *Device, seed uint64) *Actor {
	return &Actor{D: d, rng: rand.New(rand.NewPCG(seed, seed*7+1)), lines: map[string]int{}, cur: map[string][2]int{},
		crng: rand.New(rand.NewPCG(seed^0x6375727330, seed*13+5)), Harness: "claude-code", started: d.W.Now()}
}

// In switches the project checkout and starts a new harness session id.
func (a *Actor) In(root string) *Actor {
	a.Root = root
	a.nSess++
	a.Sess = fmt.Sprintf("%08x-sim-%s-%d", a.rng.Uint32(), filepath.Base(root), a.nSess)
	return a
}

// WorkTime is wall time since the actor was created minus declared breaks.
func (a *Actor) WorkTime() time.Duration { return a.D.W.Now().Sub(a.started) - a.Away }

// ResetWork restarts the work-time stopwatch.
func (a *Actor) ResetWork() { a.started, a.Away = a.D.W.Now(), 0 }

func (a *Actor) j(d time.Duration, frac float64) time.Duration {
	f := 1 + frac*(2*a.rng.Float64()-1)
	return time.Duration(float64(d) * f).Round(time.Millisecond)
}

func (a *Actor) Wait(d time.Duration) { a.D.W.Advance(a.jit(d)) }

// jit is a wait as the actor's Jit stretches or shrinks it.
func (a *Actor) jit(d time.Duration) time.Duration {
	if a.Jit > 0 && a.jrng != nil {
		d = time.Duration(float64(d) * (1 + a.Jit*(2*a.jrng.Float64()-1)))
	}
	return d
}
func (a *Actor) WaitJ(d time.Duration) { a.Wait(a.j(d, 0.25)) }

// AwayFor is a declared break.
func (a *Actor) AwayFor(d time.Duration) { a.Wait(d); a.Away += d }

func (a *Actor) base(ev string) map[string]any {
	p := map[string]any{"session_id": a.Sess, "cwd": a.Root, "hook_event_name": ev, "transcript_path": filepath.Join(a.D.Home, ".claude", "projects", a.Sess+".jsonl")}
	if a.Model != "" {
		p["model"] = a.Model
	}
	return p
}

// touch is a key press or mouse move by the person at the machine.
func (a *Actor) touch() {
	if !a.NoHands {
		a.D.Touch()
	}
}

func (a *Actor) Start() {
	a.touch()
	p := a.base("SessionStart")
	p["source"] = "startup"
	a.D.Hook("SessionStart", p)
}

func (a *Actor) End() {
	p := a.base("SessionEnd")
	p["reason"] = "prompt_input_exit"
	a.D.Hook("SessionEnd", p)
}

// Prompt: the human reads/thinks/types for ~think, then submits text. The
// end of that time is the typing, at about a word a second: key presses
// the OS idle timer sees (a script's are not, NoHands).
func (a *Actor) Prompt(think time.Duration, text string) {
	d := a.jit(a.j(think, 0.25))
	typing := min(d, time.Duration(len(strings.Fields(text)))*time.Second)
	a.D.W.Advance(d - typing)
	a.Type(typing)
	a.Send(text)
}

// Type is the person at the keyboard for d: a key press every 2 s.
func (a *Actor) Type(d time.Duration) {
	for d > 0 {
		a.touch()
		step := min(d, 2*time.Second)
		a.D.W.Advance(step)
		d -= step
	}
}

// Paste is a prompt the person did not type: a moment at the keyboard to
// copy it and paste it, then sent.
func (a *Actor) Paste(text string) {
	a.Type(4 * time.Second)
	a.Send(text)
}

// Send submits a prompt right now.
func (a *Actor) Send(text string) {
	a.touch()
	if a.Suffix != "" {
		text += " " + a.Suffix
	}
	p := a.base("UserPromptSubmit")
	p["prompt"] = text
	a.D.Hook("UserPromptSubmit", p)
}

// SendQueued is a message the person typed while the agent ran: the tool
// sends it by itself the moment the agent stops, with no key press then.
//
// Claude Code writes the typing to its session log as a queue-operation
// line; the adapter reads its time from there. The message was typed at
// the last key press during the run (a Touch step), or now if none.
func (a *Actor) SendQueued(text string) {
	typed := a.typedAt
	if typed.IsZero() {
		typed = a.D.Now()
	}
	a.typedAt = time.Time{}
	a.logQueued(typed, text)
	p := a.base("UserPromptSubmit")
	p["prompt"] = text
	a.D.Hook("UserPromptSubmit", p)
}

// Step is one thing the agent does inside a turn.
type Step struct {
	After  time.Duration
	Write  string // file written (relative to Root)
	Cmd    string // shell command (test/build/run)
	Exit   int
	Output string
	Beat   string // an editor heartbeat (no save) on this file, sent mid-turn
	Grow   int    // with Beat: lines the file grew, and a save
	Touch  bool   // the person presses a key while the agent runs (typing a queued message)
}

func W(after time.Duration, file string) Step { return Step{After: after, Write: file} }
func R(after time.Duration) Step              { return Step{After: after} }
func Pass(after time.Duration, cmd string) Step {
	return Step{After: after, Cmd: cmd}
}
func Fail(after time.Duration, cmd, output string) Step {
	return Step{After: after, Cmd: cmd, Exit: 1, Output: output}
}

// Agent runs one turn's tool calls through PostToolUse/PostToolUseFailure
// hooks and ends it with Stop.
func (a *Actor) Agent(steps ...Step) {
	for _, s := range steps {
		a.Wait(a.j(s.After, 0.2))
		switch {
		case s.Touch:
			a.touch()
			a.typedAt = a.D.Now()
			continue
		case s.Beat != "":
			a.lines[s.Beat] += s.Grow
			a.Heartbeat(s.Beat, s.Grow > 0)
			continue
		case s.Cmd != "" && s.Exit != 0:
			p := a.base("PostToolUseFailure")
			p["tool_name"], p["tool_input"] = "Bash", map[string]any{"command": s.Cmd}
			p["error"] = fmt.Sprintf("Exit code %d\n%s", s.Exit, s.Output)
			a.D.Hook("PostToolUseFailure", p)
		case s.Cmd != "":
			p := a.base("PostToolUse")
			p["tool_name"], p["tool_input"] = "Bash", map[string]any{"command": s.Cmd}
			p["tool_response"] = map[string]any{"stdout": s.Output, "stderr": ""}
			a.D.Hook("PostToolUse", p)
		case s.Write != "":
			p := a.base("PostToolUse")
			p["tool_name"] = "Edit"
			p["tool_input"] = map[string]any{"file_path": filepath.Join(a.Root, s.Write), "old_string": "x", "new_string": "y"}
			p["tool_response"] = map[string]any{"filePath": filepath.Join(a.Root, s.Write)}
			a.D.Hook("PostToolUse", p)
		default:
			p := a.base("PostToolUse")
			p["tool_name"], p["tool_input"] = "Read", map[string]any{"file_path": filepath.Join(a.Root, "README.md")}
			a.D.Hook("PostToolUse", p)
		}
	}
	a.Wait(a.j(5*time.Second, 0.5))
	a.D.Hook("Stop", a.base("Stop"))
}

// HumanRun is a run the human starts in a terminal; it reaches SeaTime through
// the plugin protocol (a shell hook calling `seatime emit`).
func (a *Actor) HumanRun(after time.Duration, cmd string, exit int, output string) {
	a.WaitJ(after)
	a.touch()
	payload := fmt.Sprintf(`{"kind":"run","harness":"terminal","session_id":"term","command":%q,"run_kind":"test","exit_code":%d}`, cmd, exit)
	_, _ = a.D.EmitCLI(payload, emit.Defaults{CWD: a.Root})
	_ = output // terminal hooks don't capture output; no err_sig
}

// Commit is a hand commit ~20 s from now, seen by the daemon's git scan.
func (a *Actor) Commit() {
	a.WaitJ(20 * sec)
	a.touch()
	a.D.Commit(a.Root, time.Time{}, 40, 2)
}

// Heartbeat sends one WakaTime heartbeat for file.
func (a *Actor) Heartbeat(file string, write bool) {
	if _, ok := a.lines[file]; !ok {
		a.lines[file] = 40
	}
	ln, col := a.cursor(file)
	// an editor edits a file that is there (a submission names a repo file
	// only when it exists in the checkout)
	if f := filepath.Join(a.Root, file); a.Root != "" && !exists(f) {
		_ = os.MkdirAll(filepath.Dir(f), 0o700)
		_ = os.WriteFile(f, []byte("// sim\n"), 0o600)
	}
	a.D.Heartbeat(map[string]any{"entity": filepath.Join(a.Root, file), "type": "file", "category": "coding",
		"is_write": write, "lines": a.lines[file], "lineno": ln, "cursorpos": col,
		"user_agent": "wakatime/v1.90 (linux-x86_64) go1.22 vscode/1.95 vscode-wakatime/24.6"})
	a.lastBeat, a.lastBeatFile = a.D.W.Now(), file
}

// cursor moves the cursor as a person editing does: mostly a few lines
// around where it was, sometimes a jump elsewhere in the file, the column
// along the line. Line and column are 1-based, as WakaTime plugins send them.
func (a *Actor) cursor(file string) (line, col int) {
	n := max(1, a.lines[file])
	c, ok := a.cur[file]
	switch {
	case !ok:
		c[0] = 1 + a.crng.IntN(n)
	case a.crng.Float64() < 0.12:
		c[0] = 1 + a.crng.IntN(n)
	default:
		c[0] += a.crng.IntN(9) - 3
	}
	c[0] = min(max(c[0], 1), n)
	c[1] = 1 + a.crng.IntN(60)
	a.cur[file] = c
	return c[0], c[1]
}

// Typing hand-edits file for dur with the WakaTime plugin's beat rules (a beat
// on save, on file switch, or on the first keystroke ≥2 min after the last).
func (a *Actor) Typing(file string, dur time.Duration, linesPerMin float64, saveEvery time.Duration) {
	if _, ok := a.lines[file]; !ok {
		a.lines[file] = 40
	}
	end := a.D.W.Now().Add(dur)
	nextSave := a.D.W.Now().Add(a.j(saveEvery, 0.4))
	acc := 0.0
	first := true
	for a.D.W.Now().Before(end) {
		a.Wait(time.Duration(2+a.rng.IntN(7)) * time.Second)
		a.touch()
		acc += linesPerMin * (a.rng.Float64() * 2) / 10
		if acc >= 1 {
			n := int(acc)
			acc -= float64(n)
			a.lines[file] += n
		}
		now := a.D.W.Now()
		switch {
		case saveEvery > 0 && !now.Before(nextSave):
			a.Heartbeat(file, true)
			nextSave = now.Add(a.j(saveEvery, 0.4))
		case first || a.lastBeatFile != file || now.Sub(a.lastBeat) >= 2*time.Minute:
			a.Heartbeat(file, false)
		}
		first = false
	}
}

// errOut builds realistic failing-run output with a distinct error line.
func errOut(line string) string {
	return "> app@1.0.0 test\n\n" + line + "\n    at Object.<anonymous> (/home/u/app/src/x.js:12:5)\nexit code 1"
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// logQueued appends Claude Code's enqueue line for a queued message to the
// session's log (the transcript the hooks name).
func (a *Actor) logQueued(at time.Time, text string) {
	path := a.base("")["transcript_path"].(string)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	line, _ := json.Marshal(map[string]any{"type": "queue-operation", "operation": "enqueue", "timestamp": at.UTC().Format(time.RFC3339Nano), "sessionId": a.Sess, "content": text})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}
