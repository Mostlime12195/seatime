// Package opencode is the opencode adapter: a JS plugin (embedded) that shells out to
// `seatime hook opencode <event>` with a small normalized JSON object on stdin.
//
// Docs consulted 2026-11-19: https://opencode.ai/docs/plugins/ (global plugins live in
// ~/.config/opencode/plugins/ and project ones in .opencode/plugins/) and
// https://opencode.ai/docs/build/plugins/server/ . The docs now describe a V2 API
// (Plugin.define / ctx.tool.hook / ctx.session.hook("prompt")) and mention a V1
// compatibility path (default export with server() returning {event, "chat.message",
// "tool.execute.before/after"}); older releases use named function exports. We could NOT
// verify the exact V1/V2 hook signatures against a running opencode, so plugin.js uses the
// widely-deployed V1 hook keys defensively (try/catch everywhere, exports for both styles,
// double-load guard). Treat opencode support as best-effort until tested on a real install.
//
// Hook event names (our own protocol): session_start, session_end, prompt, agent_stop, tool.
// stdin JSON: {ts, session_id, cwd, model, text, tool, args, metadata}.
package opencode

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"seatime/internal/adapters/common"
	"seatime/internal/adapters/runclass"
	"seatime/pkg/event"
)

const name = "opencode"
const marker = "// seatime-managed:"

//go:embed plugin.js
var pluginSource string

// Adapter integrates opencode, and (via NewKilo) Kilo Code's rebuilt, opencode-based
// extension/CLI, which exposes the same plugin hook keys under ~/.config/kilo/plugin/
// (https://kilo.ai/docs/automate/extending/plugins, checked 2026-11-19).
type Adapter struct {
	name     string
	dir      string // folder under ~/.config
	singular bool   // default to plugin/ rather than plugins/ when neither exists
}

func New() *Adapter { return &Adapter{name: name, dir: "opencode"} }

// NewKilo is the Kilo Code (new generation) variant.
func NewKilo() *Adapter { return &Adapter{name: "kilo", dir: "kilo", singular: true} }

func (a *Adapter) Name() string { return a.name }

func (a *Adapter) configDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && os.Getenv("SEATIME_FAKE_HOME") == "" {
		return filepath.Join(x, a.dir)
	}
	return filepath.Join(common.Home(), ".config", a.dir)
}

func (a *Adapter) pluginDirs() (primary string, all []string) {
	c := a.configDir()
	plural, singular := filepath.Join(c, "plugins"), filepath.Join(c, "plugin")
	primary = plural
	if a.singular {
		primary = singular
	}
	if st, err := os.Stat(primary); err != nil || !st.IsDir() {
		other := singular
		if a.singular {
			other = plural
		}
		if st, err := os.Stat(other); err == nil && st.IsDir() {
			primary = other
		}
	}
	return primary, []string{plural, singular}
}

func (a *Adapter) Detect() bool {
	if st, err := os.Stat(a.configDir()); err == nil && st.IsDir() {
		return true
	}
	if a.name == "kilo" { // the extension keeps its sessions here even before any plugin dir exists
		st, err := os.Stat(filepath.Join(common.Home(), ".local", "share", "kilo"))
		return err == nil && st.IsDir()
	}
	return false
}

func isOurs(path string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.HasPrefix(string(b), marker)
}

func (a *Adapter) Installed() (bool, error) {
	_, all := a.pluginDirs()
	for _, d := range all {
		if isOurs(filepath.Join(d, "seatime.js")) {
			return true, nil
		}
	}
	return false, nil
}

func render(seatimeBin, adapter string) string {
	q, _ := json.Marshal(seatimeBin)
	src := strings.Replace(pluginSource, "__SEATIME_BIN__", string(q), 1)
	return strings.ReplaceAll(src, `"hook", "opencode"`, `"hook", "`+adapter+`"`)
}

func (a *Adapter) Install(seatimeBin string) error {
	primary, all := a.pluginDirs()
	target := filepath.Join(primary, "seatime.js")
	if _, err := os.Stat(target); err == nil && !isOurs(target) {
		return fmt.Errorf("%s exists and is not managed by SeaTime; refusing to overwrite", target)
	}
	if err := common.WriteFile(target, []byte(render(seatimeBin, a.name)), 0o644); err != nil {
		return err
	}
	for _, d := range all { // never leave a second copy (double-load)
		if p := filepath.Join(d, "seatime.js"); p != target && isOurs(p) {
			os.Remove(p)
		}
	}
	return nil
}

func (a *Adapter) Uninstall() error {
	_, all := a.pluginDirs()
	for _, d := range all {
		if p := filepath.Join(d, "seatime.js"); isOurs(p) {
			if err := os.Remove(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// headless: `opencode run` / `kilo run` (non-interactive) runs are dropped.
func headless(args []string) bool {
	return common.SubcommandMatcher("opencode", "run")(args) || common.SubcommandMatcher("kilo", "run")(args)
}

func (a *Adapter) HandleHook(hookEvent string, stdin []byte, sink common.Sink) ([]byte, error) {
	return common.Safe(func() ([]byte, error) {
		if common.Headless(headless) {
			return nil, nil
		}
		return nil, handle(a.name, hookEvent, stdin, sink)
	})
}

var writeTools = map[string]bool{"edit": true, "write": true, "patch": true, "multiedit": true, "apply_patch": true}

func handle(name, hookEvent string, stdin []byte, sink common.Sink) error {
	p := common.Obj(stdin)
	base := event.Event{
		Harness:   name,
		SessionID: common.Str(p, "session_id"),
		CWD:       common.Str(p, "cwd"),
		Model:     common.Str(p, "model"),
		TS:        common.ParseTime(p["ts"]),
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
	case "session_start":
		return emit(event.SessionStart, nil)
	case "session_end":
		return emit(event.SessionEnd, nil)
	case "agent_stop":
		return emit(event.AgentStop, nil)
	case "prompt":
		if err := emit(event.Prompt, func(e *event.Event) { e.Text = common.Str(p, "text") }); err != nil {
			return err
		}
		return emit(event.AgentStart, nil)
	case "tool":
		tool := common.Str(p, "tool")
		args := common.Map(p, "args")
		e := base
		e.Kind = event.Tool
		e.Tool = tool
		var cmd string
		if writeTools[strings.ToLower(tool)] {
			e.Write = true
			if f := common.Str(args, "filePath", "file_path", "path"); f != "" {
				e.Files = []string{f}
			}
		}
		if strings.ToLower(tool) == "bash" {
			cmd = common.Str(args, "command")
			e.Command = cmd
			md := common.Map(p, "metadata")
			for _, k := range []string{"exit", "exit_code", "exitCode"} {
				if f, ok := md[k].(float64); ok {
					e.ExitCode = common.IntPtr(int(f))
					break
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
	}
	return nil
}
