package ledger

import (
	"testing"
	"time"

	"seatime/pkg/event"
)

var c0 = time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

func at(m float64) time.Time { return c0.Add(time.Duration(m * float64(time.Minute))) }

// prompt is a prompt the check has decided: pass with minutes, or not.
func prompt(m float64, project, hash string, pass bool) event.Event {
	return event.Event{TS: at(m), Kind: event.Prompt, SessionID: "s-" + project, Project: project, CheckHash: hash, New: &pass}
}

func stop(m float64, project string) event.Event {
	return event.Event{TS: at(m), Kind: event.AgentStop, SessionID: "s-" + project, Project: project}
}

func edit(m float64, project string) event.Event {
	return event.Event{TS: at(m), Kind: event.Edit, Project: project, Harness: "editor:vscode", Category: Coding}
}

// idle is an idle record watching [from, to) with no input in the stretches.
func idle(from, to float64, none ...[2]float64) event.Event {
	e := event.Event{TS: at(to), Kind: event.Idle, WindowMS: int64((to - from) * 60000)}
	for _, n := range none {
		e.NoInput = append(e.NoInput, [2]int64{at(n[0]).UnixMilli(), at(n[1]).UnixMilli()})
	}
	return e
}

var minutesOf = map[string]time.Duration{"feat": 10 * time.Minute, "fix": 5 * time.Minute}

var testCap = Cap{Minutes: func(e event.Event) time.Duration {
	if e.Kind != event.Prompt || e.New == nil || !*e.New {
		return 0
	}
	return minutesOf[e.CheckHash]
}}

func capCount(evs ...event.Event) Result {
	for i := range evs {
		evs[i].Seq = int64(i + 1)
	}
	return ComputeCap(evs, at(-60), at(120), testCap)
}

func near(a, b float64) bool { return a-b < 0.5 && b-a < 0.5 }

func TestCapRule(t *testing.T) {
	for _, c := range []struct {
		name          string
		evs           []event.Event
		total, notCnt float64 // seconds
	}{
		{"a feature prompt counts its 10 minutes, the agent's next 20 do not",
			[]event.Event{prompt(0, "p", "feat", true), stop(30, "p"), idle(0, 40)}, 600, 1200},
		{"a fix counts 5",
			[]event.Event{prompt(0, "p", "fix", true), stop(30, "p"), idle(0, 40)}, 300, 1500},
		{"a prompt that did not pass counts nothing",
			[]event.Event{prompt(0, "p", "feat", false), stop(8, "p")}, 0, 480},
		{"the next prompt stops the clock, even one that did not pass",
			[]event.Event{prompt(0, "p", "feat", true), prompt(4, "p", "x", false), stop(9, "p"), idle(0, 20)}, 240, 300},
		{"AI done, then idle 2 minutes: counts up to the last input",
			[]event.Event{prompt(0, "p", "feat", true), stop(2, "p"), idle(0, 20, [2]float64{3, 12})}, 180, 0},
		{"idle for less than 2 minutes does not stop it",
			[]event.Event{prompt(0, "p", "feat", true), stop(2, "p"), idle(0, 20, [2]float64{3, 4.5})}, 600, 0},
		{"idle already when the AI was done: stops at the AI's stop",
			[]event.Event{prompt(0, "p", "feat", true), stop(2, "p"), idle(0, 20, [2]float64{1, 6})}, 120, 0},
		{"no input record after the AI was done: stops at the AI's stop",
			[]event.Event{prompt(0, "p", "feat", true), stop(3, "p")}, 180, 0},
		{"hand edits count as on Hackatime",
			[]event.Event{edit(0, "p"), edit(1, "p"), edit(5, "p")}, 180, 0},
		{"hand edits inside a prompt's minutes are counted once",
			[]event.Event{prompt(0, "p", "feat", true), edit(2, "p"), edit(3, "p"), edit(11, "p")}, 660, 0},
	} {
		r := capCount(c.evs...)
		if !near(r.Total, c.total) || !near(r.AINotCounted, c.notCnt) {
			t.Errorf("%s: counted %.0f s, not counted %.0f s; want %.0f, %.0f", c.name, r.Total, r.AINotCounted, c.total, c.notCnt)
		}
		var sum float64
		for _, s := range r.Segments {
			if s.Counted {
				sum += s.Seconds()
			}
		}
		if !near(sum, r.Total) {
			t.Errorf("%s: segments %.0f, total %.0f", c.name, sum, r.Total)
		}
	}
	// a prompt's minutes are AI time, a hand edit's gap is hand time
	r := capCount(prompt(0, "p", "fix", true), edit(9, "p"), edit(10, "p"))
	if !near(r.AI, 300) || !near(r.Coding, 180) {
		t.Fatalf("AI %.0f, hand %.0f", r.AI, r.Coding)
	}
}

// Another project's counted heartbeat stops the clock, and a submission
// that reveals one project, with the others as bare times and how far they
// reached, counts that project as the whole log does.
func TestCapRuleProjects(t *testing.T) {
	for _, c := range []struct {
		name  string
		evs   []event.Event
		wantP float64
	}{
		{"a hand edit elsewhere stops the clock",
			[]event.Event{prompt(0, "p", "feat", true), edit(4, "q"), edit(6, "p")}, 240 + 120},
		{"a prompt elsewhere stops it and runs its own",
			[]event.Event{prompt(0, "p", "feat", true), prompt(4, "q", "feat", true), edit(6, "p"), edit(20, "q"), prompt(21, "q", "fix", true), edit(22, "p")}, 240},
	} {
		evs := c.evs
		for i := range evs {
			evs[i].Seq = int64(i + 1)
		}
		full := ComputeCap(evs, at(-60), at(120), testCap)
		if !near(full.ByProject["p"], c.wantP) {
			t.Errorf("%s: p %.0f, want %.0f", c.name, full.ByProject["p"], c.wantP)
		}
		var mine []event.Event
		var others, until []time.Time
		hbs := HeartbeatsCap(evs, testCap)
		for i, h := range hbs {
			if h.Project == "p" || !h.Kept {
				continue
			}
			u := h.Until
			if !u.IsZero() {
				for _, n := range hbs[i+1:] {
					if n.Kept && n.Project != h.Project {
						u = minT(u, n.Time)
						break
					}
				}
			}
			others, until = append(others, h.Time), append(until, u)
		}
		for _, e := range evs {
			if e.Project == "p" {
				mine = append(mine, e)
			}
		}
		part := CountCap(MarkCap(HeartbeatsCap(mine, testCap), others, until), at(-60), at(120))
		if !near(part.ByProject["p"], full.ByProject["p"]) {
			t.Errorf("%s: revealed alone %.0f, whole log %.0f", c.name, part.ByProject["p"], full.ByProject["p"])
		}
		if near(full.Total, 0) || full.Total > float64(120*60) {
			t.Errorf("%s: total %.0f", c.name, full.Total)
		}
	}
}

func TestVersionFor(t *testing.T) {
	if VersionFor(false) != Version || VersionFor(true) != CapVersion || CapVersion == Version {
		t.Fatal("rule names")
	}
}
