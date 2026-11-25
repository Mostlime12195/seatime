package ledger

import (
	"math"
	"testing"

	"seatime/pkg/event"
)

// modelSeconds adds up a result's buckets by model ("" hand, "?" AI with
// no model named) and checks they add up to the counted total.
func modelSeconds(t *testing.T, r Result, evs []event.Event) (map[string]float64, map[string]int) {
	t.Helper()
	sec, prompts := map[string]float64{}, map[string]int{}
	total := 0.0
	for _, b := range Buckets(r, evs) {
		named, np := 0.0, 0
		for _, m := range b.Models {
			sec[m.Model] += m.Seconds
			prompts[m.Model] += m.Prompts
			named += m.Seconds
			np += m.Prompts
		}
		if named > b.AISeconds+1e-9 || np > b.Prompts {
			t.Fatalf("bucket %v names more than it holds: %+v", b.Start, b)
		}
		sec[""] += b.CodingSeconds
		sec["?"] += b.AISeconds - named
		prompts["?"] += b.Prompts - np
		total += b.Seconds()
	}
	if math.Abs(total-r.Total) > 1e-6 {
		t.Fatalf("buckets hold %.1f s, the result %.1f s", total, r.Total)
	}
	return sec, prompts
}

// A stretch belongs to the heartbeat that ends it, and so does its model.
// A prompt and its run take the model the run's records name last.
func TestModelOfAStretch(t *testing.T) {
	var l log
	l.edit("p", hm(9, 0))
	l.edit("p", hm(9, 1)) // 1 min by hand
	// a prompt with no model of its own (Claude Code): its stop names one
	l.prompt("s", "p", hm(9, 2), "add a login page please") // 1 min, the gap before the prompt: the turn's model
	l.add(event.Event{Kind: event.Tool, Harness: "claude-code", SessionID: "s", Project: "p", TS: hm(9, 3), Model: "opus"})
	l.add(event.Event{Kind: event.AgentStop, Harness: "claude-code", SessionID: "s", Project: "p", TS: hm(9, 6), Model: "opus"}) // 4 min
	// the next prompt carries the session's model, but its turn ran on another
	l.add(event.Event{Kind: event.Prompt, Harness: "claude-code", SessionID: "s", Project: "p", TS: hm(9, 7), Text: "now make the button blue", Model: "opus"}) // 1 min
	l.add(event.Event{Kind: event.AgentStop, Harness: "claude-code", SessionID: "s", Project: "p", TS: hm(9, 9), Model: "sonnet"})                              // 2 min
	// a tool that names no model at all
	l.add(event.Event{Kind: event.Prompt, Harness: "gemini-cli", SessionID: "g", Project: "p", TS: hm(9, 10), Text: "write tests for the login"}) // 1 min
	l.add(event.Event{Kind: event.AgentStop, Harness: "gemini-cli", SessionID: "g", Project: "p", TS: hm(9, 12)})                                 // 2 min
	r := count(l)
	sec, prompts := modelSeconds(t, r, l)
	for m, want := range map[string]float64{"": 1, "opus": 5, "sonnet": 3, "?": 3} {
		wantMin(t, "model "+m, sec[m], want)
	}
	if prompts["opus"] != 1 || prompts["sonnet"] != 1 || prompts["?"] != 1 {
		t.Errorf("prompts by model: %v", prompts)
	}
}

// An editor heartbeat during an agent's run counts as the agent's, with the
// run's model; one the plugin marks "ai coding" outside any run has none.
func TestModelOfEditsDuringARun(t *testing.T) {
	var l log
	l.add(event.Event{Kind: event.Prompt, Harness: "cursor", SessionID: "s", Project: "p", TS: hm(9, 0), Text: "refactor the parser module", Model: "gpt"})
	l.edit("p", hm(9, 1))
	l.add(event.Event{Kind: event.AgentStop, Harness: "cursor", SessionID: "s", Project: "p", TS: hm(9, 2), Model: "gpt"})
	l.aiEdit("p", hm(9, 3))
	sec, _ := modelSeconds(t, count(l), l)
	wantMin(t, "gpt", sec["gpt"], 2)
	wantMin(t, "no model", sec["?"], 1)
	wantMin(t, "hand", sec[""], 0)
}

// Under the cap rule a prompt's minutes go to the model that answered it.
func TestModelUnderTheCapRule(t *testing.T) {
	p1 := prompt(0, "a", "feat", true) // counts 0..4 (the next prompt), answered by opus
	s1 := stop(3, "a")
	s1.Model = "opus"
	p2 := prompt(4, "a", "fix", true) // counts 4..5: the AI is done, and no idle timer says the person stayed
	p2.Model = "sonnet"
	e := edit(12, "a") // 2 min by hand
	evs := []event.Event{p1, s1, p2, stop(5, "a"), e}
	r := capCount(evs...)
	sec, prompts := modelSeconds(t, r, evs)
	for m, want := range map[string]float64{"opus": 4, "sonnet": 1, "": 2, "?": 0} {
		wantMin(t, "model "+m, sec[m], want)
	}
	if prompts["opus"] != 1 || prompts["sonnet"] != 1 || prompts["?"] != 0 {
		t.Errorf("prompts by model: %v", prompts)
	}
}

// A total carries no models: a segment's claim is signed over its JSON.
func TestSumCarriesNoModels(t *testing.T) {
	b := Sum([]Bucket{{Project: "p", AISeconds: 30, Models: []ModelUse{{Model: "opus", Seconds: 30}}}}, "p")
	if b.Models != nil {
		t.Fatalf("%+v", b)
	}
	s := Bucket{CodingSeconds: 10, AISeconds: 30, Models: []ModelUse{{Model: "opus", Seconds: 20}}}
	s.ScaleCounted(0.5)
	if s.CodingSeconds != 5 || s.AISeconds != 15 || s.Models[0].Seconds != 10 {
		t.Fatalf("%+v", s)
	}
}
