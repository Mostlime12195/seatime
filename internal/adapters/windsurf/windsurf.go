// Package windsurf is the Windsurf (Cascade hooks) adapter.
//
// Sources (2026-11-19; the primary docs page now redirects to docs.devin.ai and could not
// be opened, so the shape below comes from third-party packages that mirror it, see
// docs/ADAPTERS.md for the URLs):
//   - user config: ~/.codeium/windsurf/hooks.json, flat: {"hooks":{event:[{command,
//     show_output?, working_directory?}]}} (no matcher, no "type" key);
//   - events used: pre_user_prompt (tool_info.user_prompt), post_cascade_response (turn
//     end), post_write_code (tool_info.file_path), post_run_command (tool_info.command_line,
//     tool_info.cwd);
//   - stdin: {agent_action_name, trajectory_id, execution_id, timestamp, model_name,
//     tool_info:{...}}; exit 0 = proceed (only pre-hooks can block, via exit 2).
//
// Unverified: Windows config path (assumed the same), stdout protocol (we print nothing),
// and the hook's working directory (we fall back to it when the payload has no cwd).
package windsurf

import (
	"os"
	"path/filepath"

	"seatime/internal/adapters/common"
	"seatime/internal/adapters/runclass"
	"seatime/pkg/event"
)

const name = "windsurf"

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return name }

func dir() string { return filepath.Join(common.Home(), ".codeium", "windsurf") }

func hooksPath() string { return filepath.Join(dir(), "hooks.json") }

func (*Adapter) Detect() bool {
	st, err := os.Stat(dir())
	return err == nil && st.IsDir()
}

var specs = []common.HookSpec{
	{Event: "pre_user_prompt"},
	{Event: "post_cascade_response"},
	{Event: "post_write_code"},
	{Event: "post_run_command"},
}

func (*Adapter) Installed() (bool, error) {
	return common.HooksInstalled(hooksPath(), name, common.Flat)
}

func (*Adapter) Install(seatimeBin string) error {
	ss := make([]common.HookSpec, len(specs))
	copy(ss, specs)
	for i := range ss {
		ss[i].Extra = map[string]any{"type": nil, "show_output": false} // Windsurf entries have no "type"
	}
	return common.InstallHooks(hooksPath(), name, common.Flat, ss, func(ev string) string {
		return common.HookCommand(seatimeBin, name, ev)
	}, nil)
}

func (*Adapter) Uninstall() error {
	return common.UninstallHooks(hooksPath(), name, common.Flat)
}

func (a *Adapter) HandleHook(hookEvent string, stdin []byte, sink common.Sink) ([]byte, error) {
	return common.Safe(func() ([]byte, error) {
		if os.Getenv("SEATIME_IGNORE") == "1" {
			return nil, nil
		}
		return nil, handle(hookEvent, stdin, sink)
	})
}

func hookCWD() string {
	if d, err := os.Getwd(); err == nil && d != common.Home() {
		return d
	}
	return ""
}

func handle(hookEvent string, stdin []byte, sink common.Sink) error {
	p := common.Obj(stdin)
	if hookEvent == "" {
		hookEvent = common.Str(p, "agent_action_name")
	}
	ti := common.Map(p, "tool_info")
	cwd := common.Str(ti, "cwd")
	if cwd == "" {
		cwd = hookCWD()
	}
	base := event.Event{
		Harness:   name,
		SessionID: common.Str(p, "trajectory_id"),
		CWD:       cwd,
		Model:     common.Str(p, "model_name"),
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
	case "pre_user_prompt":
		if err := emit(event.Prompt, func(e *event.Event) { e.Text = common.Str(ti, "user_prompt") }); err != nil {
			return err
		}
		return emit(event.AgentStart, nil)
	case "post_cascade_response":
		return emit(event.AgentStop, nil)
	case "post_write_code":
		return emit(event.Tool, func(e *event.Event) {
			e.Tool, e.Write = "write_code", true
			if f := common.Str(ti, "file_path"); f != "" {
				e.Files = []string{f}
			}
		})
	case "post_run_command":
		cmd := common.Str(ti, "command_line")
		if err := emit(event.Tool, func(e *event.Event) { e.Tool, e.Command = "run_command", cmd }); err != nil {
			return err
		}
		if k, ok := runclass.Classify(cmd); ok && cmd != "" {
			return common.EmitRun(sink, base, cmd, k, nil, "")
		}
	}
	return nil
}
