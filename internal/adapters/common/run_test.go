package common

import (
	"testing"

	"seatime/pkg/event"
)

type capSink struct{ evs []event.Event }

func (c *capSink) Emit(e event.Event) error { c.evs = append(c.evs, e); return nil }

// A run keeps its exit code and nothing of its output.
func TestEmitRunKeepsNoOutput(t *testing.T) {
	var s capSink
	p := map[string]any{"tool_response": map[string]any{"stderr": "Traceback (most recent call last):\n  File \"a.py\", line 3\nKeyError: 'foo'\n"}}
	if err := EmitRun(&s, event.Event{}, "python a.py", "exec", IntPtr(1), OutputText(p)); err != nil {
		t.Fatal(err)
	}
	if e := s.evs[0]; e.Kind != event.Run || e.ExitCode == nil || *e.ExitCode != 1 || len(e.Extra) != 0 {
		t.Fatalf("%+v", e)
	}
}
