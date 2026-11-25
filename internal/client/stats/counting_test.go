package stats

import (
	"strings"
	"testing"
	"time"

	"seatime/pkg/event"
)

func TestCountingNow(t *testing.T) {
	now := time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)
	ev := func(k event.Kind, ago time.Duration, proj, text string) event.Event {
		e := event.Event{Kind: k, TS: now.Add(-ago), Project: proj, Text: text}
		if k != event.Edit {
			e.SessionID = "s"
		}
		return e
	}
	cases := []struct {
		name string
		in   CountingInput
		want string
	}{
		// a hand edit 1 min ago: under the 2 min of a gap
		{"editing", CountingInput{Now: now, Hooked: true, Recent: []event.Event{ev(event.Edit, time.Minute, "github.com/kid/app", "")}},
			"counting now: yes, on app (your last heartbeat was under 2 minutes ago)"},
		// a new prompt at 15:53, agent still working: its heartbeats count to 15:53 + 10 = 16:03
		{"agent running", CountingInput{Now: now, Hooked: true, Recent: []event.Event{ev(event.Prompt, 7*time.Minute, "local:game", "add a score board to the game")}},
			"counting now: yes, on game (the agent's time counts until 16:03 unless you say something new)"},
		// a new prompt at 15:40: the agent's 10 min ended at 15:50
		{"agent past its 10 minutes", CountingInput{Now: now, Hooked: true, Recent: []event.Event{ev(event.Prompt, 20*time.Minute, "p", "add a score board to the game")}},
			"counting now: no, because your last heartbeat was at 15:40"},
		{"nothing hooked", CountingInput{Now: now}, "counting now: no, because no AI tool or editor is hooked up yet (run `seatime setup`)"},
		{"silent", CountingInput{Now: now, Hooked: true, Silent: []string{"Claude Code"}}, "counting now: no, because Claude Code is running but SeaTime is not hearing from it"},
		{"fresh install", CountingInput{Now: now, Hooked: true}, "counting now: no, because no hand edit or new prompt in the last 30 min"},
	}
	for _, c := range cases {
		if got := CountingNow(c.in).Line(); !strings.HasPrefix(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// An "ai coding" heartbeat in a project where no prompt was heard in the
// last 10 min: status says SeaTime cannot hear the tool. With a prompt
// 5 min before it in that project, nothing is said.
func TestNotHeardNote(t *testing.T) {
	now := time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)
	ai := event.Event{Kind: event.Edit, Category: "ai coding", Project: "github.com/kid/app", TS: now.Add(-time.Minute)}
	c := CountingNow(CountingInput{Now: now, Hooked: true, Recent: []event.Event{ai}})
	if want := NotHeard("app", false); c.Note != want {
		t.Fatalf("note %q, want %q", c.Note, want)
	}
	p := event.Event{Kind: event.Prompt, SessionID: "s", Project: "github.com/kid/app", TS: now.Add(-6 * time.Minute), Text: "make the header sticky please"}
	if c := CountingNow(CountingInput{Now: now, Hooked: true, Recent: []event.Event{p, ai}}); c.Note != "" {
		t.Fatalf("a heard tool got the note: %q", c.Note)
	}
}

// With the prompt check on, an AI tool heard only through the editor gets
// no time of its own, and the note says what does count.
func TestNotHeardWithCheckOn(t *testing.T) {
	on := NotHeard("app", true)
	if !strings.Contains(on, "so its time does not count") || !strings.Contains(on, "Only time after a prompt that SeaTime hears can count") || strings.Contains(on, "minutes") {
		t.Fatal(on)
	}
	if off := NotHeard("app", false); !strings.Contains(off, "counts for 10 minutes after each hand edit") {
		t.Fatal(off)
	}
}
