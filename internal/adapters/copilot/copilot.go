// Package copilot is the GitHub Copilot adapter: Copilot CLI (`copilot`) and
// VS Code's Copilot agent mode, which since 2026 share one hooks system.
//
// Docs (checked 2026-11-19):
//   - https://docs.github.com/en/copilot/reference/hooks-configuration
//   - https://docs.github.com/en/copilot/reference/hooks-reference
//   - https://code.visualstudio.com/docs/agent-customization/hooks (VS Code's "Copilot
//     harness" reads the same ~/.copilot/hooks; its older "Local" harness is deprecated
//     for removal in Feb 2027 and is NOT supported here)
//
// We own a dedicated file, ~/.copilot/hooks/seatime.json (or $COPILOT_HOME/hooks/), so no
// foreign config is ever edited: {"version":1,"hooks":{event:[{type:"command",command,
// timeoutSec}]}}. Events installed (camelCase => camelCase payload): sessionStart,
// userPromptSubmitted (prompt), postToolUse, agentStop, sessionEnd. The payload is JSON on
// stdin per the VS Code docs; the CLI docs only say "JSON payload", we assume stdin too.
// We accept both camelCase (sessionId, toolName, toolArgs) and the PascalCase/snake_case
// variant (session_id, tool_name, tool_input) so a changed event naming still parses.
//
// Unverified: exact tool names (matched by substring) and the toolResult shape; hooks
// must never write stdout for userPromptSubmitted/agentStop, so we print nothing.
package copilot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"seatime/internal/adapters/common"
	"seatime/internal/adapters/runclass"
	"seatime/pkg/event"
)

const name = "copilot"

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return name }

func copilotHome() string {
	if h := os.Getenv("COPILOT_HOME"); h != "" && os.Getenv("SEATIME_FAKE_HOME") == "" {
		return h
	}
	return filepath.Join(common.Home(), ".copilot")
}

func hooksPath() string { return filepath.Join(copilotHome(), "hooks", "seatime.json") }

func (*Adapter) Detect() bool {
	if st, err := os.Stat(copilotHome()); err == nil && st.IsDir() {
		return true
	}
	// VS Code with the Copilot Chat extension reads ~/.copilot/hooks too.
	m, _ := filepath.Glob(filepath.Join(common.Home(), ".vscode", "extensions", "github.copilot-chat-*"))
	return len(m) > 0
}

var specs = []common.HookSpec{
	{Event: "sessionStart"},
	{Event: "userPromptSubmitted"},
	{Event: "postToolUse"},
	{Event: "agentStop"},
	{Event: "sessionEnd"},
}

func (*Adapter) Installed() (bool, error) {
	return common.HooksInstalled(hooksPath(), name, common.Flat)
}

func (*Adapter) Install(seatimeBin string) error {
	ss := make([]common.HookSpec, len(specs))
	copy(ss, specs)
	for i := range ss {
		ss[i].Extra = map[string]any{"timeoutSec": 10}
		if common.IsWindows && common.NeedsQuote(seatimeBin) {
			// PowerShell needs the call operator for a quoted path.
			ss[i].Extra["powershell"] = "& " + common.HookCommand(seatimeBin, name, ss[i].Event)
		}
	}
	return common.InstallHooks(hooksPath(), name, common.Flat, ss, func(ev string) string {
		return common.HookCommand(seatimeBin, name, ev)
	}, map[string]any{"version": 1})
}

func (*Adapter) Uninstall() error {
	if err := common.UninstallHooks(hooksPath(), name, common.Flat); err != nil {
		return err
	}
	// the file is ours alone: drop it when nothing but our "version" key is left
	if m, err := common.ReadJSONObject(hooksPath()); err == nil && len(m) == 1 {
		if _, ok := m["version"]; ok {
			_ = os.Remove(hooksPath())
		}
	}
	return nil
}

func (a *Adapter) HandleHook(hookEvent string, stdin []byte, sink common.Sink) ([]byte, error) {
	return common.Safe(func() ([]byte, error) {
		if os.Getenv("SEATIME_IGNORE") == "1" {
			return nil, nil
		}
		return nil, handle(hookEvent, stdin, sink)
	})
}

func handle(hookEvent string, stdin []byte, sink common.Sink) error {
	p := common.Obj(stdin)
	if hookEvent == "" {
		hookEvent = common.Str(p, "hook_event_name", "hookEventName")
	}
	base := event.Event{
		Harness:   name,
		SessionID: common.Str(p, "sessionId", "session_id"),
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
	switch strings.ToLower(hookEvent) {
	case "sessionstart":
		return emit(event.SessionStart, func(e *event.Event) {
			if s := common.Str(p, "source"); s != "" {
				e.Extra = map[string]any{"source": s}
			}
		})
	case "sessionend":
		return emit(event.SessionEnd, func(e *event.Event) {
			if s := common.Str(p, "reason"); s != "" {
				e.Extra = map[string]any{"reason": s}
			}
		})
	case "userpromptsubmitted", "userpromptsubmit":
		if err := emit(event.Prompt, func(e *event.Event) { e.Text = common.Str(p, "prompt") }); err != nil {
			return err
		}
		return emit(event.AgentStart, nil)
	case "agentstop", "stop":
		if common.Str(p, "parentAgentId", "agentId", "agent_id") != "" {
			return nil // subagent
		}
		return emit(event.AgentStop, nil)
	case "posttooluse", "posttoolusefailure":
		return handleTool(p, strings.EqualFold(hookEvent, "postToolUseFailure"), base, sink)
	}
	return nil
}

// args decodes toolArgs / tool_input, which may be an object or a JSON string.
func args(p map[string]any) map[string]any {
	for _, k := range []string{"toolArgs", "tool_input", "toolInput"} {
		switch v := p[k].(type) {
		case map[string]any:
			return v
		case string:
			var m map[string]any
			if json.Unmarshal([]byte(v), &m) == nil {
				return m
			}
		}
	}
	return nil
}

func handleTool(p map[string]any, failure bool, base event.Event, sink common.Sink) error {
	tool := common.Str(p, "toolName", "tool_name")
	lt := strings.ToLower(tool)
	in := args(p)
	e := base
	e.Kind = event.Tool
	e.Tool = tool
	var cmd string
	switch {
	case lt == "bash" || lt == "powershell" || strings.Contains(lt, "shell") || strings.Contains(lt, "terminal") || lt == "run_in_terminal":
		cmd = common.Str(in, "command", "cmd")
		e.Command = cmd
		e.ExitCode = exitCode(p, failure)
	case strings.Contains(lt, "edit") || strings.Contains(lt, "create") || strings.Contains(lt, "write") ||
		strings.Contains(lt, "patch") || strings.Contains(lt, "replace") || strings.Contains(lt, "insert"):
		e.Write = true
		if f := common.Str(in, "path", "file_path", "filePath", "file"); f != "" {
			e.Files = []string{f}
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

func exitCode(p map[string]any, failure bool) *int {
	if failure {
		return common.IntPtr(1)
	}
	res := common.Map(p, "toolResult")
	if res == nil {
		res = common.Map(p, "tool_response")
	}
	switch common.Str(res, "resultType") {
	case "success":
		return common.IntPtr(0)
	case "failure", "error", "denied":
		return common.IntPtr(1)
	}
	if n, ok := common.ParseExitCode(common.Str(res, "textResultForLlm", "output", "stdout")); ok {
		return common.IntPtr(n)
	}
	return nil
}
