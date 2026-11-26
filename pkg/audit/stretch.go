package audit

import (
	"time"

	"seatime/pkg/event"
	"seatime/pkg/ledger"
	"seatime/pkg/promptcheck"
)

// A project's records are counted in stretches: ranges of time, back to
// back, cut in the middle of long pauses in the project so that nothing
// the rule credits reaches across a cut. The server cuts the records a
// device sends as they happen (internal/server records.go); a device
// cuts its own for a server that does not take them that way
// (internal/client/segment).
const (
	// SplitPad is kept around the project's records at each end of a
	// range, so the idle-timer records the rule reads for the first and
	// last minutes are inside it. An idle window lasts at most an hour.
	SplitPad = 90 * time.Minute
	// SplitGap: a range is cut in the middle of a pause in the project at
	// least this long.
	SplitGap = 2 * SplitPad
	// splitSearch is how far back from the longest allowed end a cut is
	// looked for before any pause will do.
	splitSearch = 14 * 24 * time.Hour
	// Settle is how long after a stretch's end its records are final: an
	// idle window is written up to an hour after it opens, and the sync
	// after it.
	Settle = 75 * time.Minute
	// ReadyAfter is how long after a stretch's last record it is over.
	ReadyAfter = SplitPad + Settle
)

// Range is [Since, Until).
type Range struct{ Since, Until time.Time }

// Split cuts a project's sorted record times into back-to-back ranges that
// each cover at most MaxSpan, cut in the middle of long pauses, so the
// parts add up to the same hours as one long range would. since and until
// bound the whole; zero means a pad before the first record and after the
// last.
func Split(ts []time.Time, since, until time.Time) []Range {
	return SplitAt(ts, since, until, MaxSpan-time.Hour) // the cap, with room for clock rounding
}

// SplitAt is Split with another longest range.
func SplitAt(ts []time.Time, since, until time.Time, limit time.Duration) []Range {
	first, last := ts[0], ts[len(ts)-1]
	start := since
	if start.IsZero() {
		start = first.Add(-SplitPad)
	}
	end := until
	if end.IsZero() {
		end = last.Add(SplitPad)
	}
	var out []Range
	i := 0 // first record at or after start
	for {
		if end.Sub(start) <= limit {
			return append(out, Range{start, end})
		}
		deadline := start.Add(limit)
		// the longest pause between consecutive records ending by deadline,
		// preferring the last splitSearch before it
		best, bestGap := -1, time.Duration(0)
		bestAny, bestAnyGap := -1, time.Duration(0)
		for j := i; j+1 < len(ts) && !ts[j+1].After(deadline); j++ {
			g := ts[j+1].Sub(ts[j])
			if g > bestAnyGap {
				bestAny, bestAnyGap = j, g
			}
			if g >= SplitGap && !ts[j].Before(deadline.Add(-splitSearch)) && g >= bestGap {
				best, bestGap = j, g
			}
		}
		if best < 0 {
			best, bestGap = bestAny, bestAnyGap
		}
		var cut time.Time
		if best < 0 {
			cut = deadline // a single record fills the window; nothing better exists
		} else {
			cut = ts[best].Add(bestGap / 2).Truncate(time.Minute)
			if cut.After(deadline) {
				cut = deadline
			}
		}
		if !cut.After(start) {
			cut = deadline
		}
		out = append(out, Range{start, cut})
		start = cut
		for i < len(ts) && ts[i].Before(start) {
			i++
		}
	}
}

// Over is the end of the last stretch of work in a project's sorted record
// times that is over at now: SplitPad has passed after its last record
// with no new one, plus Settle; or, when work went on after it, the middle
// of the last pause of at least SplitGap that has settled. Zero when no
// stretch is over yet.
func Over(ts []time.Time, now time.Time) time.Time {
	if len(ts) == 0 {
		return time.Time{}
	}
	ready := now.Add(-Settle)
	if last := ts[len(ts)-1].Add(SplitPad); !last.After(ready) {
		return last.Truncate(time.Minute).Add(time.Minute)
	}
	for j := len(ts) - 2; j >= 0; j-- {
		g := ts[j+1].Sub(ts[j])
		if g < SplitGap {
			continue
		}
		if cut := ts[j].Add(g / 2).Truncate(time.Minute); !cut.After(ready) {
			return cut
		}
	}
	return time.Time{}
}

// Others are the bare times of counted heartbeats that a project's range
// does not reveal: those in other projects from from to to, and the last
// counted heartbeat of any project before from. The project's gaps split
// at them, and its first gap is as long as it is on the device. evs are
// the device's records around the range, every project's, with their
// prompts already decided (promptcheck.Set.Apply).
func Others(evs []event.Event, project string, from, to time.Time) []time.Time {
	if from.IsZero() {
		return nil
	}
	var out []time.Time
	var last time.Time
	for _, h := range ledger.Heartbeats(evs) {
		switch {
		case !h.Kept:
		case h.Time.Before(from):
			last = h.Time
		case h.Project != project && !h.Time.After(to):
			out = append(out, h.Time.UTC().Truncate(time.Millisecond))
		}
	}
	if !last.IsZero() {
		out = append([]time.Time{last.UTC().Truncate(time.Millisecond)}, out...)
	}
	return out
}

// OthersCap is Others under the cap rule (the prompt check on): the bare
// times, and for each how far it counted forward (zero when it did not),
// cut where the count cuts it: at the next counted heartbeat of another
// project.
func OthersCap(evs []event.Event, set *promptcheck.Set, project string, from, to time.Time) (times, until []time.Time) {
	if from.IsZero() {
		return nil, nil
	}
	var kept []ledger.Heartbeat
	for _, h := range ledger.HeartbeatsCap(evs, set.Cap(false)) {
		if h.Kept {
			kept = append(kept, h)
		}
	}
	reach := func(i int) time.Time {
		u := kept[i].Until
		if u.IsZero() {
			return time.Time{}
		}
		for _, n := range kept[i+1:] {
			if n.Project != kept[i].Project {
				if n.Time.Before(u) {
					u = n.Time
				}
				break
			}
		}
		return u.UTC().Truncate(time.Millisecond)
	}
	last := -1
	for i, h := range kept {
		switch {
		case h.Time.Before(from):
			last = i
		case h.Project != project && !h.Time.After(to):
			times = append(times, h.Time.UTC().Truncate(time.Millisecond))
			until = append(until, reach(i))
		}
	}
	if last >= 0 {
		times = append([]time.Time{kept[last].Time.UTC().Truncate(time.Millisecond)}, times...)
		until = append([]time.Time{reach(last)}, until...)
	}
	return times, until
}
