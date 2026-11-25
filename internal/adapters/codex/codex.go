// Package codex is the OpenAI Codex CLI adapter.
//
// Docs (verified 2026-11-19): https://learn.chatgpt.com/docs/hooks
// (redirected from https://developers.openai.com/codex/hooks)
//   - user hooks: $CODEX_HOME/hooks.json (default ~/.codex/hooks.json), same shape as
//     Claude Code: {"hooks": {Event: [{matcher?, hooks:[{type:"command", command, timeout}]}]}}.
//     We use hooks.json rather than the [hooks] TOML table so we can edit structurally
//     with encoding/json and never rewrite the user's config.toml.
//   - events used: SessionStart, UserPromptSubmit, PostToolUse, Stop, SessionEnd.
//   - stdin: session_id, transcript_path, cwd, hook_event_name, model (+turn_id; prompt;
//     tool_name, tool_input, tool_response; last_assistant_message).
//   - stdout: empty + exit 0 means "continue". We never print.
//
// KNOWN GAP: non-managed hooks must be reviewed/trusted once via `/hooks` in Codex before
// they run; `seatime setup` should tell the user. We could not verify (a) exact tool names
// ("shell"/"local_shell"/"exec_command"/"apply_patch" are handled by substring) or (b)
// tool_response shape, so the tool parsing is defensive. The legacy `notify` setting
// (argv JSON, agent-turn-complete only) is intentionally unused: it cannot see prompts.
// Session-log tailing (~/.codex/sessions/*.jsonl) is NOT implemented (format unverified).
package codex

import (
	"os"
	"regexp"
	"strings"

	"seatime/internal/adapters/common"
	"seatime/internal/adapters/runclass"
	"seatime/pkg/event"
	"seatime/pkg/promptcheck"
)

const name = "codex"

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return name }

func codexDir() string {
	if h := os.Getenv("CODEX_HOME"); h != "" && os.Getenv("SEATIME_FAKE_HOME") == "" {
		return h
	}
	return common.Home() + "/.codex"
}

func hooksPath() string { return codexDir() + "/hooks.json" }

func (*Adapter) Detect() bool {
	st, err := os.Stat(codexDir())
	return err == nil && st.IsDir()
}

var specs = []common.HookSpec{
	{Event: "SessionStart"},
	{Event: "UserPromptSubmit"},
	{Event: "PostToolUse", Matcher: "*"},
	{Event: "Stop"},
	{Event: "SessionEnd"},
}

func (*Adapter) Installed() (bool, error) {
	return common.HooksInstalled(hooksPath(), name, common.MatcherGroups)
}

func (*Adapter) Install(seatimeBin string) error {
	ss := make([]common.HookSpec, len(specs))
	copy(ss, specs)
	for i := range ss {
		ss[i].Extra = map[string]any{"timeout": 5}
		if common.IsWindows {
			ss[i].Extra["commandWindows"] = common.HookCommand(seatimeBin, name, ss[i].Event)
		}
	}
	return common.InstallHooks(hooksPath(), name, common.MatcherGroups, ss, func(ev string) string {
		return common.HookCommand(seatimeBin, name, ev)
	}, nil)
}

func (*Adapter) Uninstall() error {
	return common.UninstallHooks(hooksPath(), name, common.MatcherGroups)
}

// headless: `codex exec` (non-interactive) runs are dropped, like `claude -p`.
var headless = common.SubcommandMatcher("codex", "exec")

func (a *Adapter) HandleHook(hookEvent string, stdin []byte, sink common.Sink) ([]byte, error) {
	return common.Safe(func() ([]byte, error) {
		if common.Headless(headless) {
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
		return emit(event.SessionEnd, nil)
	case "UserPromptSubmit":
		if err := emit(event.Prompt, func(e *event.Event) { e.Text = common.Str(p, "prompt") }); err != nil {
			return err
		}
		return emit(event.AgentStart, nil)
	case "Stop":
		return emit(event.AgentStop, func(e *event.Event) {
			if m := common.Str(p, "last_assistant_message"); m != "" {
				e.Extra = map[string]any{promptcheck.ReplyKey: m} // for the prompt check; the pipeline takes it out before storing
			}
		})
	case "PostToolUse":
		return handleTool(p, base, sink)
	}
	return nil
}

var patchFileRe = regexp.MustCompile(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+?)\s*$|^\*\*\* Move to: (.+?)\s*$`)

func commandString(in map[string]any) string {
	for _, k := range []string{"command", "cmd"} {
		switch v := in[k].(type) {
		case string:
			return v
		case []any:
			parts := make([]string, 0, len(v))
			for _, x := range v {
				if s, ok := x.(string); ok {
					parts = append(parts, s)
				}
			}
			// ["bash","-lc","cmd"] -> cmd
			if n := len(parts); n >= 3 && (parts[n-2] == "-lc" || parts[n-2] == "-c") {
				return parts[n-1]
			}
			return strings.Join(parts, " ")
		}
	}
	return ""
}

func exitCode(resp any) *int {
	switch r := resp.(type) {
	case map[string]any:
		for _, k := range []string{"exit_code", "exitCode"} {
			if f, ok := r[k].(float64); ok {
				return common.IntPtr(int(f))
			}
		}
		if m := common.Map(r, "metadata"); m != nil {
			if p := exitCode(m); p != nil {
				return p
			}
		}
		for _, k := range []string{"output", "stdout", "text"} {
			if s, ok := r[k].(string); ok {
				if n, ok := common.ParseExitCode(s); ok {
					return common.IntPtr(n)
				}
			}
		}
	case string:
		if n, ok := common.ParseExitCode(r); ok {
			return common.IntPtr(n)
		}
	}
	return nil
}

func handleTool(p map[string]any, base event.Event, sink common.Sink) error {
	tool := common.Str(p, "tool_name")
	lt := strings.ToLower(tool)
	in := common.Map(p, "tool_input")
	e := base
	e.Kind = event.Tool
	e.Tool = tool
	var cmd string
	switch {
	case strings.Contains(lt, "apply_patch") || lt == "edit" || lt == "write" || strings.Contains(lt, "write_file"):
		e.Write = true
		patch := common.Str(in, "input", "patch", "command", "diff")
		if patch == "" {
			if s, ok := p["tool_input"].(string); ok {
				patch = s
			}
		}
		for _, m := range patchFileRe.FindAllStringSubmatch(patch, -1) {
			f := m[1]
			if f == "" {
				f = m[2]
			}
			e.Files = append(e.Files, f)
		}
		if f := common.Str(in, "file_path", "path"); f != "" {
			e.Files = append(e.Files, f)
		}
	case strings.Contains(lt, "shell") || strings.Contains(lt, "bash") || strings.Contains(lt, "exec"):
		cmd = commandString(in)
		e.Command = cmd
		e.ExitCode = exitCode(p["tool_response"])
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
