// Package antigravity is the Google Antigravity (IDE and CLI) adapter.
//
// Docs (checked 2026-11-19): https://antigravity.google/docs/hooks
//   - config: ~/.gemini/config/hooks.json. The top-level key is a hook *name* (any label)
//     mapping events to handler lists: {"seatime":{"PreInvocation":[{type:"command",command,
//     timeout}],...}}. We own the key "seatime"; every other key is left alone.
//   - stdin JSON (camelCase): conversationId, workspacePaths, transcriptPath, modelName,
//     lastUserInput; PreInvocation adds invocationNum; Stop adds executionNum,
//     terminationReason, fullyIdle.
//   - stdout: JSON; {} means "don't influence the agent". We always print {}.
//
// Mapping: PreInvocation => prompt (lastUserInput) + agent_start, Stop => agent_stop (only
// when fullyIdle is not false), SessionStart => session_start.
//
// The docs do not say whether PreInvocation fires once per user message or once per model
// call, so a prompt is only recorded when invocationNum is 1/absent or lastUserInput differs
// from the one last recorded for that conversation (state file in the SeaTime data dir).
package antigravity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"seatime/internal/adapters/common"
	"seatime/internal/client/config"
	"seatime/pkg/event"
)

const name = "antigravity"
const key = "seatime"

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return name }

func hooksPath() string { return filepath.Join(common.Home(), ".gemini", "config", "hooks.json") }

func (*Adapter) Detect() bool {
	for _, d := range []string{"antigravity", "antigravity-ide", "antigravity-cli"} {
		if st, err := os.Stat(filepath.Join(common.Home(), ".gemini", d)); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

var events = []string{"SessionStart", "PreInvocation", "Stop"}

// ours reports whether v is a hook group made of SeaTime commands.
func ours(v any) bool {
	g, _ := v.(map[string]any)
	if len(g) == 0 {
		return false
	}
	for _, list := range g {
		l, _ := list.([]any)
		for _, h := range l {
			m, _ := h.(map[string]any)
			if c, _ := m["command"].(string); common.IsSeatimeCommand(c, name) {
				return true
			}
		}
	}
	return false
}

func (*Adapter) Installed() (bool, error) {
	if _, err := os.Stat(hooksPath()); os.IsNotExist(err) {
		return false, nil
	}
	root, err := common.ReadJSONObject(hooksPath())
	if err != nil {
		return false, err
	}
	return ours(root[key]), nil
}

func (*Adapter) Install(seatimeBin string) error {
	root, err := common.ReadJSONObject(hooksPath())
	if err != nil {
		return err
	}
	if v, ok := root[key]; ok && !ours(v) {
		return fmt.Errorf("%s: a hook group named %q exists and is not seatime's; refusing to overwrite", hooksPath(), key)
	}
	g := map[string]any{}
	for _, ev := range events {
		g[ev] = []any{map[string]any{"type": "command", "command": common.HookCommand(seatimeBin, name, ev), "timeout": 10}}
	}
	root[key] = g
	return common.WriteJSONObject(hooksPath(), root, 0o600)
}

func (*Adapter) Uninstall() error {
	if _, err := os.Stat(hooksPath()); os.IsNotExist(err) {
		return nil
	}
	root, err := common.ReadJSONObject(hooksPath())
	if err != nil {
		return err
	}
	if !ours(root[key]) {
		return nil
	}
	delete(root, key)
	return common.RestoreOriginal(hooksPath(), root, 0o600)
}

func (a *Adapter) HandleHook(hookEvent string, stdin []byte, sink common.Sink) ([]byte, error) {
	out := []byte("{}") // Stop needs a decision; {} lets the agent stop normally
	_, err := common.Safe(func() ([]byte, error) {
		if os.Getenv("SEATIME_IGNORE") == "1" {
			return nil, nil
		}
		return nil, handle(hookEvent, stdin, sink)
	})
	return out, err
}

func handle(hookEvent string, stdin []byte, sink common.Sink) error {
	p := common.Obj(stdin)
	if hookEvent == "" {
		hookEvent = common.Str(p, "hookEventName", "hook_event_name")
	}
	cwd := ""
	if l, ok := p["workspacePaths"].([]any); ok && len(l) > 0 {
		cwd, _ = l[0].(string)
	}
	base := event.Event{
		Harness:   name,
		SessionID: common.Str(p, "conversationId"),
		CWD:       cwd,
		Model:     common.Str(p, "modelName"),
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
		return emit(event.SessionStart, nil)
	case "PreInvocation":
		text := common.Str(p, "lastUserInput")
		first := true
		if n, ok := p["invocationNum"].(float64); ok {
			first = n <= 1
		}
		if text != "" && (first || newInput(base.SessionID, text)) {
			if err := emit(event.Prompt, func(e *event.Event) { e.Text = text }); err != nil {
				return err
			}
			rememberInput(base.SessionID, text)
			return emit(event.AgentStart, nil)
		}
		return nil
	case "Stop":
		if f, ok := p["fullyIdle"].(bool); ok && !f {
			return nil
		}
		return emit(event.AgentStop, nil)
	}
	return nil
}

// ---- last-prompt memory (per conversation) ----

func statePath() string {
	return filepath.Join(config.StateDir(name), "last_input.json")
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

func readState() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(statePath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func newInput(conv, text string) bool { return readState()[conv] != digest(text) }

func rememberInput(conv, text string) {
	m := readState()
	if len(m) > 200 {
		m = map[string]string{}
	}
	m[conv] = digest(text)
	b, _ := json.Marshal(m)
	_ = common.WriteFile(statePath(), b, 0o600)
}
