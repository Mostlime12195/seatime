package segment

import (
	"context"
	"errors"
	"time"

	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/pkg/api"
	"seatime/pkg/audit"
	"seatime/pkg/event"
)

// Ranges splits a project's records into back-to-back ranges that each
// cover at most audit.MaxSpan (the server's cap on one segment), cut in
// the middle of long pauses in the project (audit.Split). Each range is
// [Since, Until) with both set; a project that fits gets one range trimmed
// to its records (plus a pad), so idle-timer records long after it are not
// sent.
func Ranges(st *store.Store, o Options) ([]Options, error) {
	evs, err := st.Events(store.Query{Project: o.Project, Since: o.Since, Until: o.Until, UpToSeq: o.MaxSeq})
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		return []Options{o}, nil
	}
	return splitTimes(times(evs), o), nil
}

func times(evs []event.Event) []time.Time {
	ts := make([]time.Time, len(evs))
	for i, e := range evs {
		ts[i] = e.TS
	}
	return ts
}

// splitTimes is Ranges over the sorted record times of the project.
func splitTimes(ts []time.Time, o Options) []Options {
	var out []Options
	for _, r := range audit.Split(ts, o.Since, o.Until) {
		ro := o
		ro.Since, ro.Until = r.Since, r.Until
		out = append(out, ro)
	}
	return out
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

// ReadyAfter is how long after a stretch's last record Next takes it as
// over.
const ReadyAfter = audit.ReadyAfter

// MetaUntil holds, per local project key, the end of the last segment the
// server took (RFC 3339).
func MetaUntil(project string) string { return "segment_until:" + project }

// Next plans the segments of one project that are ready to go up: its
// records from since (the end of the last segment sent; zero for the
// first) up to the last stretch of work that is over (audit.Over), cut as
// Ranges cuts. Only records up to maxSeq (the last synced checkpoint) are
// used. It returns the ranges (back to back, each at most audit.MaxSpan)
// and the end of the last one; none when nothing is ready.
func Next(st *store.Store, project string, since time.Time, maxSeq int64, now time.Time) ([]Options, time.Time, error) {
	evs, err := st.Events(store.Query{Project: project, Since: since, UpToSeq: maxSeq})
	if err != nil || len(evs) == 0 {
		return nil, time.Time{}, err
	}
	ts := times(evs)
	end := audit.Over(ts, now)
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
