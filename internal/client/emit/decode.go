package emit

import (
	"bytes"
	"encoding/json"
	"fmt"

	"seatime/pkg/event"
)

var knownKinds = map[event.Kind]bool{
	event.SessionStart: true, event.SessionEnd: true, event.Prompt: true, event.AgentStart: true,
	event.AgentStop: true, event.Tool: true, event.Edit: true, event.Commit: true, event.Run: true, event.Focus: true,
}

// Defaults are filled into decoded events that lack the field.
type Defaults struct {
	Harness, SessionID, Project, CWD string
}

// DecodeEvents parses the plugin protocol payload: one JSON event, a JSON
// array of events, or JSON Lines. Unknown kinds are rejected.
func DecodeEvents(b []byte, d Defaults) ([]event.Event, error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil, fmt.Errorf("empty input")
	}
	var evs []event.Event
	if b[0] == '[' {
		if err := json.Unmarshal(b, &evs); err != nil {
			return nil, err
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(b))
		for dec.More() {
			var e event.Event
			if err := dec.Decode(&e); err != nil {
				return nil, err
			}
			evs = append(evs, e)
		}
	}
	for i := range evs {
		e := &evs[i]
		if !knownKinds[e.Kind] {
			return nil, fmt.Errorf("event %d: unknown kind %q", i, e.Kind)
		}
		e.Seq = 0 // always assigned by the store
		stripDerived(e)
		if e.Harness == "" {
			e.Harness = d.Harness
		}
		if e.Harness == "" {
			e.Harness = "plugin"
		}
		if e.FromBrowserChat() {
			return nil, fmt.Errorf("event %d: browser chats are not tracked (harness %q)", i, e.Harness)
		}
		if e.SessionID == "" {
			e.SessionID = d.SessionID
		}
		if e.Project == "" {
			e.Project = d.Project
		}
		if e.CWD == "" {
			e.CWD = d.CWD
		}
	}
	return evs, nil
}

// stripDerived clears the fields SeaTime computes itself from raw
// observations, so a plugin cannot preset them. Without this,
// `seatime emit '{"kind":"prompt","words":40,"new":true}'` would count as a
// new prompt with no text.
func stripDerived(e *event.Event) {
	e.Novelty, e.SimPrev, e.TextHash = 0, 0, ""
	e.Chars, e.Words, e.New = 0, 0, nil // decided from Text by the pipeline
	// presence and run facts are SeaTime's own measurements
	e.Presence, e.InputBeforeMS, e.TerminalIdleS, e.FrontWindow = "", nil, nil, ""
	e.InputMS, e.InputOverMS, e.InputUnwatchedMS = nil, 0, 0
	e.SessionPrompts, e.OtherStopHook, e.InputSlices, e.QueuedMS = 0, nil, nil, 0
	delete(e.Extra, event.TypedAtKey)
	e.Run, e.Git, e.PromptInLog = nil, nil, nil
}
