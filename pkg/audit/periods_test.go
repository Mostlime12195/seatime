package audit

import (
	"testing"
	"time"

	"seatime/pkg/event"
	"seatime/pkg/ledger"
	"seatime/pkg/promptcheck"
)

var p0 = time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

func pat(m float64) time.Time { return p0.Add(time.Duration(m * float64(time.Minute))) }

// work is an hour of prompting: a new prompt, the agent running for 8
// minutes, a hand edit; no prompt carries a check hash (recorded while the
// check was off).
func work(start float64) []event.Event {
	yes := true
	var evs []event.Event
	for i := 0; i < 4; i++ {
		m := start + float64(i)*15
		evs = append(evs,
			event.Event{TS: pat(m), Kind: event.Prompt, Project: "p", SessionID: "s", Text: "make the board bigger please", Words: 5, New: &yes},
			event.Event{TS: pat(m + 0.1), Kind: event.AgentStart, Project: "p", SessionID: "s"},
			event.Event{TS: pat(m + 4), Kind: event.Tool, Project: "p", SessionID: "s"},
			event.Event{TS: pat(m + 8), Kind: event.AgentStop, Project: "p", SessionID: "s"},
			event.Event{TS: pat(m + 9), Kind: event.Edit, Project: "p", Harness: "editor:vscode", Category: ledger.Coding})
	}
	for i := range evs {
		evs[i].Seq = int64(i + 1)
	}
	return evs
}

// Turning the check on never recounts work from before the flip, and
// turning it off never changes how work from while it was on counts.
func TestSwitchPeriodsDecideTheRule(t *testing.T) {
	evs := work(0)
	flip := pat(120)
	on := &promptcheck.Set{On: true, Pass: map[string]bool{}, Periods: []promptcheck.Period{{From: time.UnixMilli(0), On: false}, {From: flip, On: true}}}
	before := Credit(evs)
	if before.Total < 20*60 {
		t.Fatalf("heartbeat/1 counted only %.0fs", before.Total)
	}
	applied, _ := on.Apply(evs)
	got := CreditWith(on, false, applied, nil, nil)
	if got.Total != before.Total || on.Version(promptcheck.Span(evs)) != ledger.Version {
		t.Fatalf("switch on later: %.0fs (%s), want %.0fs under heartbeat/1", got.Total, on.Version(promptcheck.Span(evs)), before.Total)
	}
	// the old behaviour (switch on for all time) zeroed it
	all := &promptcheck.Set{On: true, Pass: map[string]bool{}}
	allApplied, _ := all.Apply(evs)
	if z := CreditWith(all, false, allApplied, nil, nil); z.AI != 0 {
		t.Fatalf("control: %.0fs of AI time under the cap rule with no verdicts", z.AI)
	}

	// work while it was on, then the switch goes off: still heartbeat/2,
	// counted the same
	onEvs := work(200)
	for i := range onEvs {
		if onEvs[i].Kind == event.Prompt {
			onEvs[i].CheckHash = "h" + string(rune('a'+i))
			all.Pass[onEvs[i].CheckHash] = true
		}
	}
	off := &promptcheck.Set{On: false, Pass: all.Pass, Periods: []promptcheck.Period{{From: time.UnixMilli(0), On: false}, {From: flip, On: true}, {From: pat(400), On: false}}}
	a1, _ := all.Apply(onEvs)
	a2, _ := off.Apply(onEvs)
	r1, r2 := CreditWith(all, false, a1, nil, nil), CreditWith(off, false, a2, nil, nil)
	if r1.Total != r2.Total || r1.Total == 0 || off.Version(promptcheck.Span(onEvs)) != ledger.CapVersion {
		t.Fatalf("switch off later: %.0fs vs %.0fs (%s)", r1.Total, r2.Total, off.Version(promptcheck.Span(onEvs)))
	}

	// a range across the flip: each side by its own rule, and the device's
	// claim (ClaimWith) matches the server's analysis
	mixed := append(work(60), onEvs[:5]...)
	for i := range mixed {
		mixed[i].Seq = int64(i + 1)
	}
	m, _ := on.Apply(mixed)
	split := CreditWith(on, false, m, nil, nil)
	if split.Total <= before.Total/2 || on.Version(promptcheck.Span(mixed)) != ledger.CapVersion {
		t.Fatalf("mixed: %.0fs %s", split.Total, on.Version(promptcheck.Span(mixed)))
	}
}
