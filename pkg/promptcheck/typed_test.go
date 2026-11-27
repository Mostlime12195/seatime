package promptcheck

import (
	"testing"
	"time"

	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

func prompt(words int, inputS, unwatchedS int64) event.Event {
	ms := inputS * 1000
	return event.Event{Kind: event.Prompt, Words: words, InputMS: &ms, InputUnwatchedMS: unwatchedS * 1000, CheckHash: "h"}
}

// At 200 words a minute, 10 s of input allow 33 words and 45 s allow 150.
// Time the device could not watch counts as input, and a prompt whose
// input was not measured is never "not typed".
func TestNotTyped(t *testing.T) {
	for _, c := range []struct {
		name string
		e    event.Event
		wpm  float64
		want bool
	}{
		{"900 words after 10 s of input", prompt(900, 10, 0), 200, true},
		{"34 words after 10 s", prompt(34, 10, 0), 200, true},
		{"33 words after 10 s", prompt(33, 10, 0), 200, false},
		{"150 words after 45 s", prompt(150, 45, 0), 200, false},
		{"12 words with no input at all (a program sent it)", prompt(12, 0, 0), 200, true},
		{"900 words, 10 s of input and 5 min not watched", prompt(900, 10, 300), 200, false},
		{"900 words, input not measured", event.Event{Kind: event.Prompt, Words: 900}, 200, false},
		{"900 words after 10 s, with the setting off", prompt(900, 10, 0), 0, false},
		{"a record that is not a prompt", event.Event{Kind: event.Edit, Words: 900, InputMS: new(int64)}, 200, false},
	} {
		if got := NotTyped(c.e, c.wpm); got != c.want {
			t.Errorf("%s: not typed %v, want %v", c.name, got, c.want)
		}
	}
}

// A prompt that passed with 15 minutes and was not typed counts for the
// shortest kind's 5; typed, it counts its 15. A prompt that did not pass
// counts nothing either way, and with no shortest kind given (a device's
// own Set) nothing changes.
func TestNotTypedCountsAsTheShortestKind(t *testing.T) {
	pasted, typed := prompt(900, 10, 0), prompt(900, 600, 0)
	set := &Set{On: true, Pass: map[string]bool{"h": true}, Min: map[string]float64{"h": 15}, TypedWPM: 200, Plain: 5}
	if m := set.Minutes(pasted, false); m != 5*time.Minute {
		t.Errorf("not typed: %v", m)
	}
	if m := set.Minutes(typed, false); m != 15*time.Minute {
		t.Errorf("typed: %v", m)
	}
	set.Min["h"] = 3 // a verdict under the shortest kind's minutes keeps its own
	if m := set.Minutes(pasted, false); m != 3*time.Minute {
		t.Errorf("not typed, 3 minutes given: %v", m)
	}
	set.Min["h"] = 15
	set.Pass["h"] = false
	if m := set.Minutes(pasted, false); m != 0 {
		t.Errorf("did not pass: %v", m)
	}
	delete(set.Pass, "h")
	if m := set.Minutes(pasted, true); m != 5*time.Minute { // still being checked, taken as passing
		t.Errorf("waiting: %v", m)
	}
	onDevice := &Set{On: true, Pass: map[string]bool{"h": true}, Min: map[string]float64{"h": 15}}
	if m := onDevice.Minutes(pasted, false); m != 15*time.Minute {
		t.Errorf("a Set without the rule: %v", m)
	}

	// through the count: the agent works for 20 minutes on each prompt
	t0 := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	set.Pass["h"] = true
	count := func(e event.Event) float64 {
		e.TS, e.SessionID, e.Project = t0, "s", "p"
		yes := true
		e.New = &yes
		evs := []event.Event{e, {TS: t0.Add(20 * time.Minute), Kind: event.AgentStop, SessionID: "s", Project: "p"}}
		return ledger.ComputeCap(evs, t0.Add(-time.Hour), t0.Add(time.Hour), set.Cap(false)).Total
	}
	if got := count(pasted); got != 5*60 {
		t.Errorf("not typed, counted %v s, want 300", got)
	}
	if got := count(typed); got != 15*60 {
		t.Errorf("typed, counted %v s, want 900", got)
	}
}

func TestShortestKind(t *testing.T) {
	r := fixture()
	r.Kinds = []KindRule{{Name: "choice", Label: "choice", Minutes: 15}, {Name: "asks", Label: "asks", Minutes: 5}, {Name: "small", Minutes: 10}}
	if r.Shortest() != 5 || DefaultRules().Shortest() != FallbackMinutes.Minutes() {
		t.Fatalf("shortest %v, default %v", r.Shortest(), DefaultRules().Shortest())
	}
}
