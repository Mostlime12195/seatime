package sim

import (
	"path/filepath"
	"testing"

	"seatime/internal/client/presence"
	clientstore "seatime/internal/client/store"
)

// The simulator emits the new facts, with plausible values: a person types
// each prompt, so the input before it is a few seconds at most, while a
// loop on a machine nobody touches has none for a day (a loop beside a
// person on the same computer shares their input: the front-window check
// is for that); each agent stop carries its
// run's counts; idle records carry 5 s slices while the project is active.
func TestSimulatedPresenceFacts(t *testing.T) {
	dir := sandbox(t)
	w, err := NewWorld(filepath.Join(dir, "w"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	d, err := w.NewDevice("facty", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	a := NewActor(d, 3).In(d.Project("", "app"))
	a.Start()
	a.Prompt(40*sec, "add a settings page with a dark mode toggle")
	a.Agent(R(20*sec), W(40*sec, "settings.go"), Pass(20*sec, "go test"))
	a.Typing("settings.go", 3*mn, 3, 30*sec)
	a.Wait(2 * mn)
	box, err := w.NewDevice("facty", "box") // a machine nobody touches
	if err != nil {
		t.Fatal(err)
	}
	loop := NewActor(box, 4).In(box.Project("", "farm"))
	loop.NoHands = true
	loop.Start()
	loop.Send("continue with the next item on the list")
	loop.Agent(W(40*sec, "todo.go"))
	a.End()
	loop.End()
	w.Advance(15 * mn)
	evs, err := d.Store.Events(storeQueryAll())
	if err != nil {
		t.Fatal(err)
	}
	more, _ := box.Store.Events(storeQueryAll())
	evs = append(evs, more...)
	var person, script, stops, slices int
	for _, e := range evs {
		switch {
		case e.Kind == "prompt" && e.Project == ProjectKey("facty", "app"):
			if e.InputBeforeMS == nil || *e.InputBeforeMS > 5000 || e.Presence != presence.InputOnly || e.SessionPrompts != 1 {
				t.Errorf("the person's prompt: %+v", e)
			}
			person++
		case e.Kind == "prompt":
			if e.InputBeforeMS == nil || *e.InputBeforeMS < 3600*1000 {
				t.Errorf("the loop's prompt: input %v ms before it", e.InputBeforeMS)
			}
			script++
		case e.Kind == "agent_stop" && e.Run != nil:
			if e.Run.Writes == 0 || e.Git == nil {
				t.Errorf("stop without counts: %+v %+v", e.Run, e.Git)
			}
			stops++
		case e.Kind == "idle":
			slices += len(e.InputSlices)
		}
	}
	if person != 1 || script != 1 || stops < 2 || slices == 0 {
		t.Fatalf("person %d, script %d, stops with counts %d, sampled minutes %d", person, script, stops, slices)
	}
}

func storeQueryAll() clientstore.Query { return clientstore.Query{} }
