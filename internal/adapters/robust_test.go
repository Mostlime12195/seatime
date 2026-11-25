package adapters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"seatime/pkg/event"
)

type nopSink struct{ n int }

func (s *nopSink) Emit(event.Event) error { s.n++; return nil }

var hookEvents = []string{"", "x", "SessionStart", "UserPromptSubmit", "PostToolUse", "PostToolUseFailure", "Stop", "SubagentStop",
	"SessionEnd", "beforeSubmitPrompt", "afterFileEdit", "afterShellExecution", "stop", "BeforeAgent", "AfterAgent", "AfterTool",
	"userPromptSubmitted", "postToolUse", "sessionStart", "sessionEnd", "agentStop", "pre_user_prompt", "post_write_code",
	"post_run_command", "post_cascade_response", "PreInvocation", "prompt", "tool", "idle", "agent_stop", "session_start"}

// renameKeys renames every object key, recursively (a harness update that
// renames all fields).
func renameKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := map[string]any{}
		for k, val := range x {
			m[k+"_v2"] = renameKeys(val)
		}
		return m
	case []any:
		for i := range x {
			x[i] = renameKeys(x[i])
		}
	}
	return v
}

// retype swaps every scalar for a value of another type.
func retype(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = retype(val)
		}
		return x
	case []any:
		for i := range x {
			x[i] = retype(x[i])
		}
		return x
	case string:
		return 42
	case float64:
		return []any{"x"}
	case bool:
		return map[string]any{"a": nil}
	case nil:
		return "null"
	}
	return v
}

func payloads(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{
		"empty":   nil,
		"garbage": []byte("\x00\xffnot json at all"),
		"null":    []byte("null"),
		"array":   []byte(`[1,{"a":2}]`),
		"string":  []byte(`"hello"`),
		"deep":    []byte(strings.Repeat("[", 5000) + strings.Repeat("]", 5000)),
		"huge":    []byte(`{"prompt":"` + strings.Repeat("a", 256<<10) + `","session_id":"s"}`),
	}
	fixtures, _ := filepath.Glob(filepath.Join("*", "testdata", "*.json"))
	for _, f := range fixtures {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		out[f+"/truncated"] = b[:len(b)/2]
		var v any
		if json.Unmarshal(b, &v) != nil {
			continue
		}
		r, _ := json.Marshal(renameKeys(v))
		out[f+"/renamed"] = r
		var w any
		json.Unmarshal(b, &w)
		rt, _ := json.Marshal(retype(w))
		out[f+"/retyped"] = rt
	}
	if len(fixtures) < 20 {
		t.Fatalf("expected the adapters' recorded fixtures, found %d", len(fixtures))
	}
	return out
}

// A harness update can change or drop any field. No adapter may panic or
// return an error for a payload it does not recognise: it records what it
// can and the hook exits 0.
func TestAdaptersSurviveUnknownPayloads(t *testing.T) {
	t.Setenv("SEATIME_FAKE_HOME", t.TempDir())
	ps := payloads(t)
	for _, a := range All() {
		for _, ev := range hookEvents {
			for name, p := range ps {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("%s %s %s: panic: %v", a.Name(), ev, name, r)
						}
					}()
					if _, err := a.HandleHook(ev, p, &nopSink{}); err != nil {
						t.Errorf("%s %s %s: error: %v", a.Name(), ev, name, err)
					}
				}()
			}
		}
	}
}
