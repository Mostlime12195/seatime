// Package gemini is the Gemini CLI adapter.
//
// Docs (verified 2026-11-19): https://geminicli.com/docs/hooks/ and
// https://geminicli.com/docs/hooks/reference/
//   - ~/.gemini/settings.json: {"hooks":{<Event>:[{matcher?, hooks:[{type:"command", command, name?, timeout(ms)}]}]}}
//   - events: SessionStart, SessionEnd, BeforeAgent, AfterAgent, BeforeModel, AfterModel,
//     BeforeToolSelection, BeforeTool, AfterTool, PreCompress, Notification. We use
//     SessionStart, BeforeAgent{prompt}, AfterTool{tool_name,tool_input,tool_response},
//     AfterAgent{prompt,prompt_response}, SessionEnd.
//   - stdin base: session_id, transcript_path, cwd, hook_event_name, timestamp (ISO 8601).
//   - stdout protocol: stdout MUST contain only a JSON object (anything else is parsed as a
//     systemMessage); exit 0 with {} = allow/no-op. We always print "{}".
//
// Not verified: built-in tool names (we match write_file/replace/edit as writes and
// run_shell_command as shell) and the shell exit-code location (we regex "Exit Code: N"
// out of tool_response.llmContent). Model name is not in the payload (left empty).
package gemini

import (
	"os"

	"seatime/internal/adapters/common"
	"seatime/internal/adapters/runclass"
	"seatime/pkg/event"
)

const name = "gemini-cli"

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return name }

func settingsPath() string { return common.Home() + "/.gemini/settings.json" }

func (*Adapter) Detect() bool {
	st, err := os.Stat(common.Home() + "/.gemini")
	return err == nil && st.IsDir()
}

var specs = []common.HookSpec{
	{Event: "SessionStart"},
	{Event: "BeforeAgent"},
	{Event: "AfterTool", Matcher: "*"},
	{Event: "AfterAgent"},
	{Event: "SessionEnd"},
}

func (*Adapter) Installed() (bool, error) {
	return common.HooksInstalled(settingsPath(), name, common.MatcherGroups)
}

func (*Adapter) Install(seatimeBin string) error {
	ss := make([]common.HookSpec, len(specs))
	copy(ss, specs)
	for i := range ss {
		ss[i].Extra = map[string]any{"name": "seatime", "timeout": 10000}
	}
	return common.InstallHooks(settingsPath(), name, common.MatcherGroups, ss, func(ev string) string {
		return common.HookCommand(seatimeBin, name, ev)
	}, nil)
}

func (*Adapter) Uninstall() error {
	return common.UninstallHooks(settingsPath(), name, common.MatcherGroups)
}

// headless: `gemini -p/--prompt` (non-interactive) runs are dropped.
var headless = common.SubcommandMatcher("gemini", "-p", "--prompt")

func (a *Adapter) HandleHook(hookEvent string, stdin []byte, sink common.Sink) ([]byte, error) {
	_, err := common.Safe(func() ([]byte, error) {
		if common.Headless(headless) {
			return nil, nil
		}
		return nil, handle(hookEvent, stdin, sink)
	})
	return []byte("{}"), err // stdout must be a JSON object, always
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
		return emit(event.SessionEnd, func(e *event.Event) {
			if s := common.Str(p, "reason"); s != "" {
				e.Extra = map[string]any{"reason": s}
			}
		})
	case "BeforeAgent":
		if err := emit(event.Prompt, func(e *event.Event) { e.Text = common.Str(p, "prompt") }); err != nil {
			return err
		}
		return emit(event.AgentStart, nil)
	case "AfterAgent":
		return emit(event.AgentStop, nil)
	case "AfterTool":
		return handleTool(p, base, sink)
	}
	return nil
}

func handleTool(p map[string]any, base event.Event, sink common.Sink) error {
	tool := common.Str(p, "tool_name")
	in := common.Map(p, "tool_input")
	e := base
	e.Kind = event.Tool
	e.Tool = tool
	var cmd string
	switch tool {
	case "write_file", "replace", "edit", "edit_file", "create_file":
		e.Write = true
		if f := common.Str(in, "file_path", "absolute_path", "path"); f != "" {
			e.Files = []string{f}
		}
	case "run_shell_command", "shell":
		cmd = common.Str(in, "command")
		e.Command = cmd
		resp := common.Map(p, "tool_response")
		if s := common.Str(resp, "llmContent", "returnDisplay"); s != "" {
			if n, ok := common.ParseExitCode(s); ok {
				e.ExitCode = common.IntPtr(n)
			}
		}
		if e.ExitCode == nil && common.Str(resp, "error") != "" {
			e.ExitCode = common.IntPtr(1)
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
