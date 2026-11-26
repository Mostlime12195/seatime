// Package event defines seatime's shared event schema. It is the contract between
// adapters, the local store, the scoring engine and the server. Keep it stable:
// add fields, never rename or repurpose them.
package event

import (
	"strings"
	"time"
)

type Kind string

const (
	SessionStart Kind = "session_start"
	SessionEnd   Kind = "session_end"
	Prompt       Kind = "prompt"
	AgentStart   Kind = "agent_start"
	AgentStop    Kind = "agent_stop"
	Tool         Kind = "tool"
	Edit         Kind = "edit"
	Commit       Kind = "commit"
	Run          Kind = "run"
	Focus        Kind = "focus"
	// Idle is written by the daemon once per window of minute ticks: how long
	// the window was (WindowMS, ending at TS), which OS idle timer it read
	// (IdleSrc, "none" when there is none) and the stretches with no keyboard
	// or mouse input (NoInput). It is not activity: scoring paths that count
	// activity drop it (WithoutFrames).
	Idle Kind = "idle"
	// ChainRestart is the first record of a local log that replaced a
	// corrupt one: the chain continues from the last checkpoint sent to the
	// server (PrevSeq), so nothing after the break is rejected. Note says why.
	ChainRestart Kind = "chain_restart"
)

// RunKind classifies a "run" event.
const (
	RunTest  = "test"
	RunBuild = "build"
	RunDev   = "dev"
	RunExec  = "exec"
)

// Event is one observation. Fields tagged `local:"true"` must never be sent to
// the server except, in its private form, inside a segment (pkg/api Bundle).
type Event struct {
	Seq       int64     `json:"seq"` // per-device, assigned by the store
	TS        time.Time `json:"ts"`  // UTC, ms precision
	Kind      Kind      `json:"kind"`
	Harness   string    `json:"harness,omitempty"` // "claude-code", "codex", "cursor", "gemini-cli", "opencode", "editor:<name>", "git", ...
	SessionID string    `json:"session_id,omitempty"`
	Project   string    `json:"project,omitempty"` // attributed project key, e.g. "github.com/user/repo" or "local:dirname"
	CWD       string    `json:"cwd,omitempty" local:"true"`
	Model     string    `json:"model,omitempty"`

	// prompt
	Text  string `json:"text,omitempty" local:"true"`
	Words int    `json:"words,omitempty"`
	// New says whether a prompt is new (docs/RULE.md, definition 2): 4 words
	// or more, and not sent before in this project. The device decides it
	// once, when the prompt is recorded (emit), from a keyed set that never
	// leaves the machine. nil when not decided (then the rule decides it).
	New *bool `json:"new,omitempty"`
	// CheckHash is set only while the prompt check is switched on
	// (pkg/promptcheck), on a prompt that is not an exact repeat in its
	// project: SHA-256 of the prompt text with a random salt that stays on
	// the device until the text is sent for its check. The server takes a
	// check only for text that matches it, and counts the prompt by the
	// stored verdict for this hash. Empty everywhere else.
	CheckHash string `json:"check_hash,omitempty"`

	// Chars, TextHash, Novelty and SimPrev are only on records written
	// before heartbeat/1. This version never sets them; they stay so that
	// old records keep their chain leaves (wire:"old" keeps them off the
	// allowlist of what a client sends, see pkg/api WirePaths).
	Chars    int     `json:"chars,omitempty" wire:"old"`
	TextHash string  `json:"text_hash,omitempty" wire:"old"` // hex of normalized text: HMAC-SHA-256 under the device key (CanonKeyed), plain SHA-256 before
	Novelty  float64 `json:"novelty,omitempty" wire:"old"`
	SimPrev  float64 `json:"sim_prev,omitempty" wire:"old"`

	// tool / edit
	Tool     string   `json:"tool,omitempty"`
	Files    []string `json:"files,omitempty" local:"true"` // absolute or repo-relative paths
	Write    bool     `json:"write,omitempty"`
	Command  string   `json:"command,omitempty" local:"true"`
	ExitCode *int     `json:"exit_code,omitempty"`
	Lines    int      `json:"lines,omitempty"`
	// LineNo and CursorPos are an editor heartbeat's cursor line and column
	// (both 1-based), as WakaTime plugins report them. Like
	// Lines they are numbers only, never content. Records before them carry
	// neither (omitempty keeps their chain leaves unchanged).
	LineNo    int `json:"lineno,omitempty"`
	CursorPos int `json:"cursorpos,omitempty"`
	// LineAdditions and LineDeletions are the lines the editor plugin says
	// this heartbeat's edit added and removed (WakaTime's line_additions,
	// line_deletions). Numbers only.
	LineAdditions int `json:"line_additions,omitempty"`
	LineDeletions int `json:"line_deletions,omitempty"`
	// Category is an editor heartbeat's WakaTime category as the plugin
	// sent it ("coding", "ai coding", "debugging"...). "ai coding" is the
	// agent's heartbeat (docs/RULE.md, definition 3).
	Category string `json:"category,omitempty"`

	// commit
	SHA          string `json:"sha,omitempty"`
	Insertions   int    `json:"insertions,omitempty"`
	Deletions    int    `json:"deletions,omitempty"`
	FilesChanged int    `json:"files_changed,omitempty"`

	// run
	RunKind string `json:"run_kind,omitempty"`

	// idle (see Idle; additive, omitempty)
	IdleSrc  string     `json:"idle_src,omitempty"`
	WindowMS int64      `json:"window_ms,omitempty"`
	NoInput  [][2]int64 `json:"no_input,omitempty"` // unix ms [from, to)

	// Presence facts (the person's trail). Numbers and
	// yes/no only; all omitempty, so records without them keep their chain
	// leaves. None of them changes hours.
	//
	// Presence is how presence was measured on this device: one of the
	// fixed words in package presence ("input+front window", "input only",
	// "terminal only", "not measured"), on prompts and idle records.
	Presence string `json:"presence,omitempty"`
	// InputBeforeMS, on a prompt: milliseconds from the last keyboard or
	// mouse input to the prompt, by the OS idle timer.
	InputBeforeMS *int64 `json:"input_before_ms,omitempty"`
	// TerminalIdleS, on a prompt from a terminal reached over SSH or on a
	// machine with no desktop: seconds since keys last reached that
	// terminal, rounded down to 8 s steps.
	TerminalIdleS *int `json:"terminal_idle_s,omitempty"`
	// FrontWindow, on a prompt, only when the front-window check is switched
	// on: "yes" (the tool's own window was in front), "no", or "no window
	// attached". Never the name of any program.
	FrontWindow string `json:"front_window,omitempty"`
	// SessionPrompts, on a prompt: how many prompts this session has had,
	// this one included.
	SessionPrompts int `json:"session_prompts,omitempty"`
	// OtherStopHook, on a prompt: whether another hook that can re-prompt
	// the agent (a stop hook that is not SeaTime's) is installed.
	OtherStopHook *bool `json:"other_stop_hook,omitempty"`
	// QueuedMS, on a prompt: the person typed it while the agent was still
	// running and the tool held it until the run ended. It is how long it
	// waited, in milliseconds (typed at TS minus QueuedMS). 0 = not queued,
	// or the tool's log does not say. A number only. The rule counts the
	// prompt at TS like any other; reply-time facts leave it out, because
	// its delay after the stop says nothing about reading.
	QueuedMS int64 `json:"queued_ms,omitempty"`
	// InputSlices, on an idle record: for each minute sampled every 5 s
	// (while a project was active), [the minute's start in unix ms, twelve
	// bits: bit i set when the 5 s slice i saw input].
	InputSlices [][2]int64 `json:"input_slices,omitempty"`
	// FrontSlices, on an idle record, only while the front-window check is
	// on: for each minute sampled, [the minute's start in unix ms, twelve
	// bits: bit i set when the AI tool's own window was in front at 5 s
	// slice i]. A minute that is missing was not measured.
	FrontSlices [][2]int64 `json:"front_slices,omitempty"`

	// Agent run facts, on an agent stop (the tool's and the
	// repository's trails).
	Run *RunFacts `json:"run,omitempty"`
	// Git is the repository at the agent's stop.
	Git *GitFacts `json:"git,omitempty"`
	// PromptInLog, on an agent stop: whether the run's prompt was found in
	// the AI tool's own session log.
	PromptInLog *bool `json:"prompt_in_log,omitempty"`

	// chain_restart (see ChainRestart)
	PrevSeq int64  `json:"prev_seq,omitempty"`
	Note    string `json:"note,omitempty"`

	// Extra is for adapter-specific data; treated as local-only.
	Extra map[string]any `json:"extra,omitempty" local:"true"`
	// Backfilled: recovered from the harness's own log because its hooks
	// were silent. A label; it never changes a number.
	Backfilled bool `json:"backfilled,omitempty"`
	// Canon is the record's chain form (pkg/chain). Form 2: the chain commits to the folder, file paths, extras,
	// prompt text and command only through their hashes and names the
	// project by its server key, so a segment can reveal the record
	// without any of them. 3, what this version writes, is form 2 with
	// keyed commitments (CanonKeyed). 0 is the older form, which committed to text and
	// command only.
	Canon int `json:"canon,omitempty"`
}

// TypedAtKey is the Extra key an adapter puts on a prompt that the AI
// tool's log shows was typed earlier and held in a queue: when it was typed
// (RFC 3339). The pipeline turns it into QueuedMS and drops the key.
const TypedAtKey = "typed_at"

// HoldKey is the Extra key an adapter puts on a prompt, and on the agent
// start that follows it, when the AI tool's own log will say who sent the
// prompt but has not been written yet: {"log": path of the log, "id": the
// prompt's id in it}. The pipeline keeps both back until the log says, and
// drops the key (package emit, hold.go).
const HoldKey = "hold"

// RunFacts count what one agent run did, from its tool calls.
type RunFacts struct {
	Reads          int `json:"reads,omitempty"`
	Writes         int `json:"writes,omitempty"`
	Commands       int `json:"commands,omitempty"`
	CommandsFailed int `json:"commands_failed,omitempty"`
	Searches       int `json:"searches,omitempty"`
	Web            int `json:"web,omitempty"`
	Other          int `json:"other,omitempty"`
	FilesInside    int `json:"files_inside,omitempty"`  // distinct files written inside the project
	FilesOutside   int `json:"files_outside,omitempty"` // and outside it
	LinesWritten   int `json:"lines_written,omitempty"`
}

// GitFacts are the repository's uncommitted changes against HEAD, and HEAD.
type GitFacts struct {
	FilesChanged int    `json:"files_changed,omitempty"`
	Insertions   int    `json:"insertions,omitempty"`
	Deletions    int    `json:"deletions,omitempty"`
	Head         string `json:"head,omitempty"` // the current commit id
}

// CanonPrivate is the chain form that commits to the folder, file paths,
// extras, prompt text and command only through their hashes.
const CanonPrivate = 2

// CanonKeyed is CanonPrivate with those commitments, and the prompt's
// TextHash, keyed by a random per-device key that never leaves the machine
// (HMAC-SHA-256), so short prompts cannot be guessed from a word list.
// Records written by this version are in this form.
const CanonKeyed = 3

// Redacted returns a copy with all local-only fields cleared (what may leave the
// machine outside a segment).
func (e Event) Redacted() Event {
	e.CWD, e.Text, e.Command = "", "", ""
	e.Files = nil
	e.Extra = nil
	return e
}

// ActivityOnly returns evs minus records that are not human or agent
// activity (idle-timer windows, chain restarts). Every activity-scoring
// path filters with it.
func ActivityOnly(evs []Event) []Event {
	n := 0
	for _, e := range evs {
		if !e.Counts() {
			n++
		}
	}
	if n == 0 {
		return evs
	}
	out := make([]Event, 0, len(evs)-n)
	for _, e := range evs {
		if e.Counts() {
			out = append(out, e)
		}
	}
	return out
}

// Counts reports whether e is activity the rule may count.
func (e Event) Counts() bool { return e.Kind.Activity() }

// FromBrowserChat reports whether e claims to come from a browser chat
// (harness "web:<site>"). A browser tab has no working directory, so its
// time cannot be tied to a project, and SeaTime does not record it.
func (e Event) FromBrowserChat() bool { return strings.HasPrefix(e.Harness, "web:") }

// Activity reports whether events of kind k describe human or agent activity.
func (k Kind) Activity() bool {
	switch k {
	case SessionStart, SessionEnd, Prompt, AgentStart, AgentStop, Tool, Edit, Commit, Run, Focus:
		return true
	}
	return false
}
