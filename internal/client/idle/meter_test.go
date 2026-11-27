package idle

import (
	"context"
	"errors"
	"testing"
	"time"

	"seatime/pkg/event"
)

// desk is a computer under a fake clock: the daemon's recorder ticks every
// minute and runs Fast every 5 s, as Run does, and key presses land at the
// times a test gives.
type desk struct {
	t0, now   time.Time
	lastInput time.Time
	reads     int
	rec       *Recorder
	events    []event.Event
}

func newDesk(active bool) *desk {
	d := &desk{t0: time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)}
	d.now, d.lastInput = d.t0, d.t0.Add(-24*time.Hour)
	d.rec = &Recorder{Now: func() time.Time { return d.now }, Active: func() bool { return active },
		Emit: func(e event.Event) error { d.events = append(d.events, e); return nil },
		Src: Func{N: "fake", F: func(context.Context) (time.Duration, error) {
			d.reads++
			return d.now.Sub(d.lastInput), nil
		}}}
	d.rec.Tick(context.Background())
	return d
}

// run moves the clock to t0+until, with a key press at each of keys (offsets from t0).
func (d *desk) run(until time.Duration, keys ...time.Duration) {
	for d.now.Before(d.t0.Add(until)) {
		d.now = d.now.Add(time.Second)
		off := d.now.Sub(d.t0)
		for _, k := range keys {
			if k == off {
				d.lastInput = d.now
			}
		}
		if off%time.Minute == 0 {
			d.rec.Tick(context.Background())
		}
		if off%SliceEvery == 0 {
			d.rec.Fast(context.Background())
		}
	}
}

func every(from, to, step time.Duration) (ks []time.Duration) {
	for k := from; k <= to; k += step {
		ks = append(ks, k)
	}
	return ks
}

func (d *desk) since(from time.Duration) Input {
	in, ok := d.rec.InputSince(context.Background(), d.t0.Add(from))
	if !ok {
		panic("no idle timer")
	}
	return in
}

// A person reads for four minutes, moving the mouse now and then, and
// types a prompt for a minute. No project is active (it is the session's
// first prompt): the meter counts the typing all the same, and the idle
// record still holds no slices, since those are kept only while a project
// is active.
func TestMeterCountsTyping(t *testing.T) {
	d := newDesk(false)
	keys := append([]time.Duration{20 * time.Second, 95 * time.Second, 170 * time.Second}, every(241*time.Second, 300*time.Second, 2*time.Second)...)
	d.run(5*time.Minute, keys...)
	in := d.since(0)
	// (the first mouse move, at 0:20, came before the daemon read every 5 s: 15 s of it are not watched)
	if in.Input != 75*time.Second || in.Unwatched != 15*time.Second || in.Over != 5*time.Minute {
		t.Fatalf("typed for a minute: %+v", in)
	}
	// only the typing, when the previous prompt was sent at 4:00
	if in := d.since(4 * time.Minute); in.Input < 55*time.Second || in.Input > 60*time.Second || in.Over != time.Minute {
		t.Fatalf("since 4:00: %+v", in)
	}
	d.rec.Flush()
	for _, e := range d.events {
		if len(e.InputSlices) > 0 {
			t.Fatalf("slices recorded with no project active: %+v", e)
		}
	}
}

// A person who is there pastes: one burst of input a few seconds before
// the prompt, and nothing else since the last one.
func TestMeterPasteIsOneSlice(t *testing.T) {
	d := newDesk(true)
	d.run(5*time.Minute, 5*time.Second, 297*time.Second, 298*time.Second)
	if in := d.since(30 * time.Second); in.Input != SliceEvery || in.Unwatched != 0 {
		t.Fatalf("a paste: %+v", in)
	}
}

// Nobody is at the computer: no input, nothing unwatched, and the 5 s tick
// never reads the idle timer.
func TestMeterNobodyThere(t *testing.T) {
	d := newDesk(false)
	d.run(30 * time.Minute)
	if d.reads > 31 {
		t.Fatalf("%d reads of the idle timer in 30 minutes on a computer nobody is at", d.reads)
	}
	if in := d.since(0); in.Input != 0 || in.Unwatched != 0 || in.Over != 30*time.Minute {
		t.Fatalf("nobody there: %+v", in)
	}
}

// Back after half an hour away, a person types for 30 s and sends before
// the next minute tick. The daemon was not reading every 5 s, so the OS
// tells it only the last input: the 30 s count as one slice of input and
// the rest as not watched, never as no input.
func TestMeterBackFromABreak(t *testing.T) {
	d := newDesk(false)
	d.run(30*time.Minute+50*time.Second, every(30*time.Minute+20*time.Second, 30*time.Minute+50*time.Second, 2*time.Second)...)
	in := d.since(10 * time.Minute)
	if in.Input != SliceEvery || in.Unwatched < 40*time.Second || in.Unwatched > 46*time.Second {
		t.Fatalf("back from a break: %+v", in)
	}
}

// The computer slept for two hours (no reading in that time) and a key
// woke it: the sleep is not time the person may have typed in.
func TestMeterSleepIsNotUnwatched(t *testing.T) {
	d := newDesk(false)
	d.run(2 * time.Minute)
	d.now = d.now.Add(2 * time.Hour)
	d.lastInput = d.now.Add(-time.Second)
	in, _ := d.rec.InputSince(context.Background(), d.t0)
	if in.Input != SliceEvery || in.Unwatched > time.Minute || in.Over != MeterKeep {
		t.Fatalf("after sleep: %+v", in)
	}
}

// With no idle timer the meter says nothing, rather than "no input".
func TestMeterWithoutIdleTimer(t *testing.T) {
	r := &Recorder{Src: None{Reason: "wayland"}}
	r.Tick(context.Background())
	if _, ok := r.InputSince(context.Background(), time.Now().Add(-time.Minute)); ok {
		t.Fatal("an answer with no idle timer")
	}
	bad := &Recorder{Src: Func{N: "x", F: func(context.Context) (time.Duration, error) { return 0, errors.New("fail") }}}
	if _, ok := bad.InputSince(context.Background(), time.Time{}); ok {
		t.Fatal("an answer from a reading that failed")
	}
}
