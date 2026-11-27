package idle

import (
	"context"
	"time"
)

// The meter answers one question for a prompt's hook: how much keyboard or
// mouse input came in the time before the prompt (Recorder.InputSince). It
// remembers when the daemon read the idle timer and when the OS said the
// last input was, for the last MeterKeep. It holds times only, in memory:
// nothing of it is written or sent but the numbers a prompt carries.

// MeterKeep is how far back a prompt's input is counted.
const MeterKeep = time.Hour

// meterHole is the most one stretch between two readings can count as not
// watched. The daemon reads at least once a minute while the computer is
// awake, so a longer stretch is the computer asleep, or the daemon before
// it started.
const meterHole = time.Minute

// Input is what the meter saw in a stretch of time.
type Input struct {
	// Input is the time with input: SliceEvery for every reading that
	// found input since the reading before it.
	Input time.Duration
	// Unwatched is time the meter can say nothing about. Between two
	// readings further apart than a slice, the OS gives only the last
	// input: what came before it in that stretch is not known.
	Unwatched time.Duration
	// Over is the length of the stretch.
	Over time.Duration
}

type reading struct{ at, input time.Time } // when it was read, and the last input then

type meter struct{ rs []reading }

func (m *meter) add(at time.Time, idle time.Duration) {
	if n := len(m.rs); n > 0 && !at.After(m.rs[n-1].at) {
		if at.Equal(m.rs[n-1].at) {
			return
		}
		m.rs = m.rs[:0] // the clock went back: what was read no longer lines up
	}
	m.rs = append(m.rs, reading{at, at.Add(-max(idle, 0))})
	i := 0
	for i < len(m.rs) && m.rs[i].at.Before(at.Add(-MeterKeep)) {
		i++
	}
	if i > 0 {
		m.rs = append(m.rs[:0], m.rs[i:]...)
	}
}

// lastInput is the last input the meter knows of (zero: none).
func (m *meter) lastInput() time.Time {
	if len(m.rs) == 0 {
		return time.Time{}
	}
	return m.rs[len(m.rs)-1].input
}

// since counts the input in (from, to], from at most MeterKeep before to.
func (m *meter) since(from, to time.Time) Input {
	if lo := to.Add(-MeterKeep); from.Before(lo) {
		from = lo
	}
	out := Input{Over: max(to.Sub(from), 0)}
	prev := from
	for _, r := range m.rs {
		if !r.at.After(from) {
			continue
		}
		if r.at.After(to) {
			break
		}
		if r.input.After(prev) {
			out.Input += SliceEvery
			if r.at.Sub(prev) > SliceEvery*3/2 {
				out.Unwatched += min(max(r.input.Sub(prev)-SliceEvery, 0), meterHole)
			}
		}
		prev = r.at
	}
	out.Input = min(out.Input, out.Over)
	out.Unwatched = min(out.Unwatched, out.Over-out.Input)
	return out
}

// InputSince reads the idle timer now and returns the input since from (at
// most MeterKeep back). ok is false when there is no idle timer to read.
func (r *Recorder) InputSince(ctx context.Context, from time.Time) (in Input, ok bool) {
	r.defaults()
	if r.Src == nil {
		return in, false
	}
	d, err := r.Src.Idle(ctx)
	if err != nil {
		return in, false
	}
	now := r.Now().Round(0).UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.meter.add(now, d)
	return r.meter.since(from, now), true
}

// near reports whether the last input the meter knows of is under Near old.
func (r *Recorder) near(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	last := r.meter.lastInput()
	return !last.IsZero() && now.Sub(last) < r.Near
}

// Fast is the 5 s tick. It reads the idle timer while a project is active,
// and then also records the slice (Slice), or while someone used the
// computer in the last Near, so that the input before a prompt is counted
// from a session's first prompt on. On a computer nobody is at, it does
// nothing.
func (r *Recorder) Fast(ctx context.Context) {
	r.defaults()
	active := r.Active != nil && r.Active()
	if !active && !r.near(r.Now().Round(0).UTC()) {
		return
	}
	r.sample(ctx, active)
}
