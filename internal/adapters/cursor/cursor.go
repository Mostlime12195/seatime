// Package cursor is the Cursor adapter.
//
// Docs (verified 2026-11-19): https://cursor.com/docs/agent/hooks
//   - ~/.cursor/hooks.json: {"version":1,"hooks":{<event>:[{command,type?,timeout?,matcher?}]}}
//   - stdin common fields: conversation_id, generation_id, model, hook_event_name,
//     cursor_version, workspace_roots, user_email, transcript_path.
//   - events used: sessionStart, beforeSubmitPrompt{prompt}, afterFileEdit{file_path,edits[]},
//     afterShellExecution{command,output,duration}, stop{status}, sessionEnd{session_id,reason}.
//   - stdout protocol: exit 0 => stdout JSON used. beforeSubmitPrompt expects
//     {"continue":true} (anything else could block the prompt); stop may return
//     {"followup_message":...} (we return {} = no follow-up); the after* hooks have no
//     documented output. Exit codes other than 0/2 fail open; we always exit 0.
//
// Known gaps: afterShellExecution has no exit code field (ExitCode stays nil); the shell
// hook output text is not scanned. Tab (inline completion) edits are not recorded here:
// they are human typing and are better covered by the editor/WakaTime path.
package cursor

import (
	"os"
	"strings"

	"seatime/internal/adapters/common"
	"seatime/internal/adapters/runclass"
	"seatime/pkg/event"
)

const name = "cursor"

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return name }

func hooksPath() string { return common.Home() + "/.cursor/hooks.json" }

func (*Adapter) Detect() bool {
	st, err := os.Stat(common.Home() + "/.cursor")
	return err == nil && st.IsDir()
}

var specs = []common.HookSpec{
	{Event: "sessionStart"},
	{Event: "beforeSubmitPrompt"},
	{Event: "afterFileEdit"},
	{Event: "afterShellExecution"},
	{Event: "stop"},
	{Event: "sessionEnd"},
}

func (*Adapter) Installed() (bool, error) {
	return common.HooksInstalled(hooksPath(), name, common.Flat)
}

func (*Adapter) Install(seatimeBin string) error {
	ss := make([]common.HookSpec, len(specs))
	copy(ss, specs)
	for i := range ss {
		ss[i].Extra = map[string]any{"timeout": 10} // never failClosed
	}
	return common.InstallHooks(hooksPath(), name, common.Flat, ss, func(ev string) string {
		return common.HookCommand(seatimeBin, name, ev)
	}, map[string]any{"version": 1})
}

func (*Adapter) Uninstall() error {
	return common.UninstallHooks(hooksPath(), name, common.Flat)
}

func response(hookEvent string) []byte {
	switch hookEvent {
	case "beforeSubmitPrompt":
		return []byte(`{"continue":true}`)
	case "stop":
		return []byte(`{}`)
	}
	return nil
}

// headless: `cursor-agent -p/--print` (non-interactive) runs are dropped.
var headless = common.SubcommandMatcher("cursor-agent", "-p", "--print")

func (a *Adapter) HandleHook(hookEvent string, stdin []byte, sink common.Sink) ([]byte, error) {
	resp := response(hookEvent)
	_, err := common.Safe(func() ([]byte, error) {
		if common.Headless(headless) {
			return nil, nil
		}
		return nil, handle(hookEvent, stdin, sink)
	})
	return resp, err // always answer the protocol, even on error
}

func handle(hookEvent string, stdin []byte, sink common.Sink) error {
	p := common.Obj(stdin)
	if hookEvent == "" {
		hookEvent = common.Str(p, "hook_event_name")
	}
	cwd := common.Str(p, "cwd")
	if wr, ok := p["workspace_roots"].([]any); ok && len(wr) > 0 {
		if s, ok := wr[0].(string); ok {
			cwd = s
		}
	}
	base := event.Event{
		Harness:   name,
		SessionID: common.Str(p, "conversation_id", "session_id"),
		CWD:       cwd,
		Model:     common.Str(p, "model"),
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
	case "sessionStart":
		return emit(event.SessionStart, nil)
	case "sessionEnd":
		return emit(event.SessionEnd, func(e *event.Event) {
			if r := common.Str(p, "reason"); r != "" {
				e.Extra = map[string]any{"reason": r}
			}
		})
	case "beforeSubmitPrompt":
		if err := emit(event.Prompt, func(e *event.Event) { e.Text = common.Str(p, "prompt") }); err != nil {
			return err
		}
		return emit(event.AgentStart, nil)
	case "stop":
		return emit(event.AgentStop, func(e *event.Event) {
			if s := common.Str(p, "status"); s != "" {
				e.Extra = map[string]any{"status": s}
			}
		})
	case "afterFileEdit":
		return emit(event.Tool, func(e *event.Event) {
			e.Tool = "edit"
			e.Write = true
			if f := common.Str(p, "file_path"); f != "" {
				e.Files = []string{f}
			}
			if eds, ok := p["edits"].([]any); ok {
				for _, x := range eds {
					if m, ok := x.(map[string]any); ok {
						if s, ok := m["new_string"].(string); ok && s != "" {
							e.Lines += strings.Count(s, "\n") + 1
						}
					}
				}
			}
		})
	case "afterShellExecution":
		cmd := common.Str(p, "command")
		if err := emit(event.Tool, func(e *event.Event) { e.Tool = "shell"; e.Command = cmd }); err != nil {
			return err
		}
		if k, ok := runclass.Classify(cmd); ok {
			return common.EmitRun(sink, base, cmd, k, nil, common.OutputText(p))
		}
	}
	return nil
}
