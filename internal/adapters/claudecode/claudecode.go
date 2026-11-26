// Package claudecode is the Claude Code adapter.
//
// Docs (verified 2026-11-19): https://code.claude.com/docs/en/hooks
//   - hooks live in ~/.claude/settings.json under "hooks": {Event: [{matcher?, hooks:[{type:"command", command, timeout}]}]}
//   - stdin JSON: session_id, transcript_path, cwd, hook_event_name, permission_mode;
//     UserPromptSubmit adds prompt; PreToolUse/PostToolUse add tool_name, tool_input,
//     tool_use_id (+tool_response on success); PostToolUseFailure adds error beginning
//     "Exit code N" for a Bash command that ran and failed; SessionStart adds source and
//     (optionally) model; SessionEnd adds reason; Stop/SubagentStop add last_assistant_message.
//   - model (checked 2026-11-24): only SessionStart can carry "model", and not always
//     (not after /clear or a recovered session). No other event names it. The session
//     log does: every assistant entry has message.model. So a tool call and a stop take
//     the model of the log's last assistant entry (ModelInLog), which is the model that
//     made that call, also after /model or a fallback; a prompt takes the session's last
//     named model (the pipeline carries it forward).
//   - exit 0 with empty stdout = no-op for every event we use.
//
// Headless detection: the docs do NOT specify a payload field for `claude -p`.
// Hooks inherit the claude process environment, so we use (not documented, observed
// on 2.1.326): CLAUDE_CODE_ENTRYPOINT=cli for the interactive TUI and "sdk-*" for
// -p/SDK runs, and CLAUDE_CODE_SESSION_ATTENDED=1 for human-attended sessions.
// A run is treated as headless (and dropped) if ENTRYPOINT starts with "sdk" or
// ATTENDED is "0"/"false". SEATIME_IGNORE=1 forces dropping. Unknown values are kept.
package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"seatime/internal/adapters/common"
	"seatime/internal/adapters/runclass"
	"seatime/pkg/event"
	"seatime/pkg/promptcheck"
)

const name = "claude-code"

// Adapter implements common.Adapter.
type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return name }

func settingsPath() string { return common.Home() + "/.claude/settings.json" }

func (*Adapter) Detect() bool {
	st, err := os.Stat(common.Home() + "/.claude")
	return err == nil && st.IsDir()
}

// hooks installed. PreToolUse is not needed (PostToolUse carries the result).
var specs = []common.HookSpec{
	{Event: "SessionStart"},
	{Event: "UserPromptSubmit"},
	{Event: "PostToolUse", Matcher: "*"},
	{Event: "PostToolUseFailure", Matcher: "Bash"},
	{Event: "Stop"},
	{Event: "SessionEnd"},
}

func (*Adapter) Installed() (bool, error) {
	return common.HooksInstalled(settingsPath(), name, common.MatcherGroups)
}

func (*Adapter) Install(seatimeBin string) error {
	winPS := common.IsWindows && common.NeedsQuote(seatimeBin)
	ss := make([]common.HookSpec, len(specs))
	copy(ss, specs)
	for i := range ss {
		ss[i].Extra = map[string]any{"timeout": 10}
		if winPS {
			// Claude Code on Windows runs hooks in PowerShell or Git Bash; a quoted
			// path needs the call operator, so pin the shell to PowerShell.
			ss[i].Extra["shell"] = "powershell"
		}
	}
	return common.InstallHooks(settingsPath(), name, common.MatcherGroups, ss, func(ev string) string {
		c := common.HookCommand(seatimeBin, name, ev)
		if winPS {
			c = "& " + c
		}
		return c
	}, nil)
}

func (*Adapter) Uninstall() error {
	return common.UninstallHooks(settingsPath(), name, common.MatcherGroups)
}

// Headless reports whether the current process environment looks like a
// non-interactive (claude -p / SDK) run. The environment can be unset
// (`env -u CLAUDE_CODE_ENTRYPOINT`), so the harness command line is checked
// too (common.Headless).
func Headless() bool {
	if common.Headless(cliHeadless) {
		return true
	}
	if strings.HasPrefix(os.Getenv("CLAUDE_CODE_ENTRYPOINT"), "sdk") {
		return true
	}
	switch strings.ToLower(os.Getenv("CLAUDE_CODE_SESSION_ATTENDED")) {
	case "0", "false":
		return true
	}
	return false
}

var cliHeadless = common.SubcommandMatcher("claude", "-p", "--print")

func (a *Adapter) HandleHook(hookEvent string, stdin []byte, sink common.Sink) ([]byte, error) {
	return common.Safe(func() ([]byte, error) {
		if Headless() {
			return nil, nil
		}
		return nil, handle(hookEvent, stdin, sink)
	})
}

func handle(hookEvent string, stdin []byte, sink common.Sink) error {
	p := common.Obj(stdin)
	if hookEvent == "" {
		hookEvent = common.Str(p, "hook_event_name")
	}
	base := event.Event{
		Harness:   name,
		SessionID: common.Str(p, "session_id"),
		CWD:       common.Str(p, "cwd"),
		Model:     common.Str(p, "model"),
		TS:        common.ParseTime(p["timestamp"]),
	}
	// Subagent hooks carry agent_id; their tool calls still count as tools but
	// their lifecycle events must not look like the main agent's.
	sub := common.Str(p, "agent_id") != ""
	if base.Model == "" && !sub {
		switch hookEvent {
		case "Stop", "PostToolUse", "PostToolUseFailure":
			base.Model = ModelInLog(common.Str(p, "transcript_path"), base.SessionID)
		}
	}
	emit := func(k event.Kind, mod func(*event.Event)) error {
		e := base
		e.Kind = k
		if mod != nil {
			mod(&e)
		}
		return sink.Emit(e)
	}
	switch hookEvent {
	case "SessionStart":
		return emit(event.SessionStart, func(e *event.Event) {
			if s := common.Str(p, "source"); s != "" {
				e.Extra = map[string]any{"source": s}
			}
		})
	case "SessionEnd":
		return emit(event.SessionEnd, func(e *event.Event) {
			if s := common.Str(p, "reason"); s != "" {
				e.Extra = map[string]any{"reason": s}
			}
		})
	case "UserPromptSubmit":
		// Claude Code fires this hook for prompts the tool makes itself
		// too: a scheduled task or /loop iteration, a background task or
		// subagent reporting back, a message from another session or a
		// channel. The hook's payload does not say who sent the prompt.
		// Claude Code's session log does (SentBy), but it writes the
		// prompt there only after this hook has returned (checked on
		// 2.1.332). So the prompt and the agent start are marked to be
		// held (event.HoldKey): the pipeline records them once the log
		// says who sent the prompt, and records no prompt when it was not
		// a person. An older Claude Code gives no prompt id: its prompts
		// are recorded at once, except a background task's report, which
		// is known by its text (toolSent).
		if toolSent(common.Str(p, "prompt")) {
			return emit(event.AgentStart, nil)
		}
		var hold map[string]any
		if id, log := common.Str(p, "prompt_id"), common.Str(p, "transcript_path"); id != "" && log != "" {
			hold = map[string]any{"log": log, "id": id}
		}
		if err := emit(event.Prompt, func(e *event.Event) {
			e.Text = common.Str(p, "prompt")
			e.OtherStopHook = otherStopHook(base.CWD)
			e.Extra = map[string]any{}
			if at, ok := TypedAt(common.Str(p, "transcript_path"), base.SessionID, e.Text); ok {
				e.Extra[event.TypedAtKey] = at.Format(time.RFC3339Nano) // the pipeline turns it into QueuedMS
			}
			if hold != nil {
				e.Extra[event.HoldKey] = hold
			}
			if len(e.Extra) == 0 {
				e.Extra = nil
			}
		}); err != nil {
			return err
		}
		return emit(event.AgentStart, func(e *event.Event) {
			if hold != nil {
				e.Extra = map[string]any{event.HoldKey: hold}
			}
		})
	case "Stop":
		if sub {
			return nil
		}
		return emit(event.AgentStop, func(e *event.Event) {
			if t := common.Str(p, "transcript_path"); t != "" {
				e.Extra = map[string]any{"transcript_path": t} // local only: the pipeline looks for the prompt in it
			}
			if m := common.Str(p, "last_assistant_message"); m != "" {
				if e.Extra == nil {
					e.Extra = map[string]any{}
				}
				e.Extra[promptcheck.ReplyKey] = m // for the prompt check; the pipeline takes it out before storing
			}
		})
	case "SubagentStop":
		return nil
	case "PostToolUse", "PostToolUseFailure":
		return handleTool(p, hookEvent == "PostToolUseFailure", base, sink)
	}
	return nil
}

var writeTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}

func handleTool(p map[string]any, failure bool, base event.Event, sink common.Sink) error {
	tool := common.Str(p, "tool_name")
	in := common.Map(p, "tool_input")
	e := base
	e.Kind = event.Tool
	e.Tool = tool
	if writeTools[tool] {
		e.Write = true
		if f := common.Str(in, "file_path", "notebook_path", "path"); f != "" {
			e.Files = []string{f}
		}
		e.Lines = linesWritten(in)
	}
	var cmd string
	if tool == "Bash" {
		cmd = common.Str(in, "command")
		e.Command = cmd
		if failure {
			if n, ok := common.ParseExitCode(common.Str(p, "error")); ok {
				e.ExitCode = common.IntPtr(n)
			} else {
				e.ExitCode = common.IntPtr(1)
			}
		} else {
			e.ExitCode = common.IntPtr(0) // PostToolUse only fires on success
		}
		// Best-effort: files changed by a shell command (public beta field).
		if d := common.Map(common.Map(p, "tool_response"), "bashEditDiff"); d != nil {
			if cf, ok := d["changedFiles"].([]any); ok {
				for _, f := range cf {
					if s, ok := f.(string); ok {
						e.Files = append(e.Files, s)
					}
				}
				e.Write = len(e.Files) > 0
			}
		}
	}
	if err := sink.Emit(e); err != nil {
		return err
	}
	if cmd != "" {
		if k, ok := runclass.Classify(cmd); ok {
			return common.EmitRun(sink, base, cmd, k, e.ExitCode, common.OutputText(p))
		}
	}
	return nil
}

// linesWritten counts the lines a write tool call wrote: the new file's
// content, an edit's replacement text, or each of a multi-edit's.
func linesWritten(in map[string]any) int {
	count := func(s string) int {
		if s == "" {
			return 0
		}
		return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
	}
	n := count(common.Str(in, "content", "new_string", "new_source"))
	if edits, ok := in["edits"].([]any); ok {
		for _, x := range edits {
			if m, ok := x.(map[string]any); ok {
				n += count(common.Str(m, "new_string"))
			}
		}
	}
	return n
}

// otherStopHook reports whether a Stop or SubagentStop hook that is not
// SeaTime's is set in Claude Code's user or project settings: such a hook
// can make the agent go on by itself. nil when no settings could be read.
func otherStopHook(cwd string) *bool {
	paths := []string{settingsPath()}
	if cwd != "" {
		paths = append(paths, filepath.Join(cwd, ".claude", "settings.json"), filepath.Join(cwd, ".claude", "settings.local.json"))
	}
	read, found := false, false
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		read = true
		found = found || OtherStopHookIn(b)
	}
	if !read {
		return nil
	}
	return &found
}

// OtherStopHookIn reports whether settings JSON has a Stop or SubagentStop
// hook whose command is not SeaTime's.
func OtherStopHookIn(settings []byte) bool {
	var s struct {
		Hooks map[string][]struct {
			Hooks []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(settings, &s) != nil {
		return false
	}
	for _, ev := range []string{"Stop", "SubagentStop"} {
		for _, g := range s.Hooks[ev] {
			for _, h := range g.Hooks {
				if c, _ := h["command"].(string); !strings.Contains(c, "seatime") {
					return true
				}
			}
		}
	}
	return false
}

// toolSent reports whether the text of a "prompt" is a message Claude Code
// sent itself: a background task or subagent reporting back arrives as a
// <task-notification> block (seen in its session logs, 2026-11-25).
func toolSent(prompt string) bool {
	return strings.HasPrefix(strings.TrimSpace(prompt), "<task-notification>")
}

// Human is the sender Claude Code's log gives a prompt a person sent.
const Human = "human"

// SentBy reads from Claude Code's session log who sent a prompt. Every
// line the log writes for a prompt carries its id, and the line of the
// prompt itself says where it came from:
//
//	{"type":"user","promptId":"...","origin":{"kind":"human"},"promptSource":"typed",...}
//
// kind is "human" for a prompt a person sent. Anything else is a message
// the tool made: "task-notification" (a background task or subagent
// reporting back), "channel", "peer", "coordinator" (other sessions),
// "auto-continuation", "plugin" and more. seen is false while the log has
// no line of that prompt yet (Claude Code writes it once the prompt's
// hooks have returned). kind is "" when the prompt is there and the log
// names no sender: an older Claude Code, or `claude -p`. Only the last
// 8 MB of the log are read.
func SentBy(path, promptID string) (kind string, seen bool) {
	if path == "" || promptID == "" {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > 8<<20 {
		f.Seek(fi.Size()-8<<20, io.SeekStart)
	}
	return sentByIn(io.LimitReader(f, 8<<20), promptID)
}

func sentByIn(r io.Reader, promptID string) (kind string, seen bool) {
	id := []byte(promptID)
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n') // a line of any length: the reader is capped
		if len(line) == 0 && err != nil {
			break
		}
		if !bytes.Contains(line, id) {
			if err != nil {
				break
			}
			continue
		}
		var l struct {
			Type     string
			PromptID string
			Origin   *struct{ Kind string }
		}
		if json.Unmarshal(line, &l) != nil || l.Type != "user" || l.PromptID != promptID {
			continue
		}
		seen = true
		if l.Origin != nil && l.Origin.Kind != "" {
			return l.Origin.Kind, true
		}
	}
	return "", seen
}

// TypedAt reads when a prompt was typed, if Claude Code held it in its
// queue. A message typed while the agent is still running is queued and
// sent when the turn ends; the hook fires then. The session log (the
// transcript) records the typing as its own line:
//
//	{"type":"queue-operation","operation":"enqueue","timestamp":"...","sessionId":"...","content":"..."}
//
// and the message itself appears later as the normal user entry. Recent
// versions write an enqueue line for every prompt, a few milliseconds
// before it is sent; the pipeline only calls a prompt queued when it waited
// a second or more (emit.queuedFor).
//
// The match is the newest enqueue line of the session whose content is the
// prompt, or, when several queued messages went out as one prompt, the
// first of them. Only the last 8 MB of the log are read. ok is false when
// there is no log or no such line.
func TypedAt(path, session, prompt string) (time.Time, bool) {
	prompt = strings.TrimSpace(prompt)
	if path == "" || prompt == "" {
		return time.Time{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > 8<<20 {
		f.Seek(fi.Size()-8<<20, io.SeekStart)
	}
	return typedAtIn(io.LimitReader(f, 8<<20), session, prompt)
}

func typedAtIn(r io.Reader, session, prompt string) (time.Time, bool) {
	var exact, first time.Time
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"queue-operation"`)) || !bytes.Contains(line, []byte(`"enqueue"`)) {
			continue
		}
		var q struct {
			Type, Operation, Timestamp, SessionID string
			Content                               any
		}
		if json.Unmarshal(line, &q) != nil || q.Type != "queue-operation" || q.Operation != "enqueue" {
			continue
		}
		if session != "" && q.SessionID != "" && q.SessionID != session {
			continue
		}
		c, _ := q.Content.(string)
		c = strings.TrimSpace(c)
		at := common.ParseTime(q.Timestamp)
		if c == "" || at.IsZero() {
			continue
		}
		switch {
		case c == prompt:
			exact, first = at, time.Time{}
		case strings.HasPrefix(prompt, c+"\n"):
			first = at
		}
	}
	if !first.IsZero() && first.After(exact) {
		return first, true
	}
	return exact, !exact.IsZero()
}

// ModelInLog reads the model of the session log's last assistant entry:
//
//	{"type":"assistant","sessionId":"...","message":{"model":"claude-opus-5",...}}
//
// Entries of subagents (isSidechain) and ones Claude Code wrote itself
// (model "<synthetic>") are skipped. Only the last 1 MB of the log is read.
// "" when there is no log or no such entry.
func ModelInLog(path, session string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 1 << 20
	if fi, err := f.Stat(); err == nil && fi.Size() > tail {
		f.Seek(fi.Size()-tail, io.SeekStart)
	}
	b, err := io.ReadAll(io.LimitReader(f, tail))
	if err != nil {
		return ""
	}
	for len(b) > 0 {
		line := b
		if i := bytes.LastIndexByte(bytes.TrimRight(b, "\n"), '\n'); i >= 0 {
			line, b = b[i+1:], b[:i]
		} else {
			b = nil
		}
		if !bytes.Contains(line, []byte(`"assistant"`)) {
			continue
		}
		var e struct {
			Type        string
			IsSidechain bool
			SessionID   string
			Message     struct{ Model string }
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.IsSidechain {
			continue
		}
		if session != "" && e.SessionID != "" && e.SessionID != session {
			continue
		}
		if m := strings.TrimSpace(e.Message.Model); m != "" && !strings.HasPrefix(m, "<") {
			return m
		}
	}
	return ""
}
