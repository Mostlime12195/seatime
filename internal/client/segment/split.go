package segment

import (
	"context"
	"errors"
	"time"

	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/pkg/api"
	"seatime/pkg/audit"
)

const (
	// splitPad is kept around the project's records at each end of a range,
	// so the idle-timer records the rule reads for the first and last
	// minutes are inside it. An idle window lasts at most an hour.
	splitPad = 90 * time.Minute
	// splitGap: a range is cut in the middle of a pause in the project at
	// least this long, so nothing the rule credits (a turn, a return, a
	// joined edit, an idle window) reaches across the cut.
	splitGap = 2 * splitPad
	// splitSearch is how far back from the longest allowed end a cut is
	// looked for before any pause will do.
	splitSearch = 14 * 24 * time.Hour
)

// Ranges splits a project's records into back-to-back ranges that each
// cover at most audit.MaxSpan (the server's cap on one segment), cut in
// the middle of long pauses in the project, so the parts add up to the
// same hours as one long range would. Each range is [Since, Until) with both
// set; a project that fits gets one range trimmed to its records (plus a
// pad), so idle-timer records long after it are not sent.
func Ranges(st *store.Store, o Options) ([]Options, error) {
	evs, err := st.Events(store.Query{Project: o.Project, Since: o.Since, Until: o.Until, UpToSeq: o.MaxSeq})
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		return []Options{o}, nil
	}
	ts := make([]time.Time, len(evs))
	for i, e := range evs {
		ts[i] = e.TS
	}
	return splitTimes(ts, o), nil
}

// splitTimes is Ranges over the sorted record times of the project.
func splitTimes(ts []time.Time, o Options) []Options {
	first, last := ts[0], ts[len(ts)-1]
	start := o.Since
	if start.IsZero() {
		start = first.Add(-splitPad)
	}
	end := o.Until
	if end.IsZero() {
		end = last.Add(splitPad)
	}
	limit := audit.MaxSpan - time.Hour // the cap, with room for clock rounding
	var out []Options
	i := 0 // first record at or after start
	for {
		if end.Sub(start) <= limit {
			r := o
			r.Since, r.Until = start, end
			return append(out, r)
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
			if g >= splitGap && !ts[j].Before(deadline.Add(-splitSearch)) && g >= bestGap {
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
		r := o
		r.Since, r.Until = start, cut
		out = append(out, r)
		start = cut
		for i < len(ts) && ts[i].Before(start) {
			i++
		}
	}
}

// Upload sends a segment, waiting out the server's per-device rate limit
// when several go up at once.
func Upload(ctx context.Context, c *syncer.Client, b api.Bundle, wait func(time.Duration)) (api.SegmentResp, error) {
	for tries := 0; ; tries++ {
		resp, err := c.Segment(ctx, b)
		var he *syncer.HTTPError
		if err == nil || !errors.As(err, &he) || he.Status != 429 && he.Status != 503 || tries >= 6 {
			return resp, err
		}
		wait(31 * time.Second)
	}
}

// settle is how long after a stretch's end its records are final: an idle
// window is written up to an hour after it opens, and the sync after it.
const settle = 75 * time.Minute

// Next plans the segments of one project that are ready to go up: its
// records from since (the end of the last segment sent; zero for the
// first) up to the last stretch of work that is over, cut in the middle of
// pauses of at least splitGap, as Ranges cuts. A stretch is over once
// splitPad has passed after its last record with no new one, plus settle.
// Only records up to maxSeq (the last synced checkpoint) are used. It
// returns the ranges (back to back, each at most audit.MaxSpan) and the
// end of the last one; none when nothing is ready.
func Next(st *store.Store, project string, since time.Time, maxSeq int64, now time.Time) ([]Options, time.Time, error) {
	evs, err := st.Events(store.Query{Project: project, Since: since, UpToSeq: maxSeq})
	if err != nil || len(evs) == 0 {
		return nil, time.Time{}, err
	}
	ts := make([]time.Time, len(evs))
	for i, e := range evs {
		ts[i] = e.TS
	}
	ready := now.Add(-settle)
	end := time.Time{}
	if last := ts[len(ts)-1].Add(splitPad); !last.After(ready) {
		end = last.Truncate(time.Minute).Add(time.Minute)
	} else {
		// the last pause long enough to cut in, whose middle has settled
		for j := len(ts) - 2; j >= 0; j-- {
			g := ts[j+1].Sub(ts[j])
			if g < splitGap {
				continue
			}
			if cut := ts[j].Add(g / 2).Truncate(time.Minute); !cut.After(ready) {
				end = cut
				break
			}
		}
	}
	if end.IsZero() {
		return nil, time.Time{}, nil
	}
	var in []time.Time
	for _, t := range ts {
		if t.Before(end) {
			in = append(in, t)
		}
	}
	if len(in) == 0 {
		return nil, time.Time{}, nil
	}
	o := Options{Project: project, Since: since, Until: end, MaxSeq: maxSeq}
	return splitTimes(in, o), end, nil
}
