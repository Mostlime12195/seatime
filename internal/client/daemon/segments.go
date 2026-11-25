package daemon

import (
	"context"
	"errors"
	"strings"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/logx"
	"seatime/internal/client/segment"
	"seatime/internal/client/syncer"
)

// segmentLoop uploads each project's records once a stretch of work on it
// is over (package segment), so the server checks them against the hash
// chain as they arrive. Nothing needs the person to act.
func (d *Daemon) segmentLoop(ctx context.Context) {
	sleep(ctx, 2*time.Minute)
	for ctx.Err() == nil {
		wait := 10 * time.Minute
		if _, err := d.SegmentsOnce(ctx, time.Now()); err != nil && !errors.Is(err, syncer.ErrNotLoggedIn) {
			var he *syncer.HTTPError
			if errors.As(err, &he) && he.Status == 404 {
				wait = 6 * time.Hour // an older server takes no segments
			} else {
				logx.Printf("segments: %v", err)
			}
		}
		sleepWall(ctx, wait, 30*time.Second)
	}
}

// MetaSegmentUntil holds, per local project key, the end of the last
// segment the server took (RFC 3339).
func MetaSegmentUntil(project string) string { return "segment_until:" + project }

// SegmentsOnce uploads every segment that is ready, up to the last synced
// checkpoint. It returns how many went up.
func (d *Daemon) SegmentsOnce(ctx context.Context, now time.Time) (int, error) {
	return d.segments(ctx, now, false)
}

// SegmentsNow uploads every project's records up to the last synced
// checkpoint without waiting for the stretch of work to be over (`seatime
// send`). The next segment of a project starts at now. A prompt sent just
// before now loses what it would have counted after now, so this is for a
// moment when the work is done and someone wants to see it checked.
func (d *Daemon) SegmentsNow(ctx context.Context, now time.Time) (int, error) {
	return d.segments(ctx, now, true)
}

func (d *Daemon) segments(ctx context.Context, now time.Time, flush bool) (int, error) {
	auth, ok := d.auth()
	if !ok {
		return 0, syncer.ErrNotLoggedIn
	}
	ss, err := d.Store.SyncState()
	if err != nil || ss.Seq == 0 {
		return 0, err
	}
	ps, err := d.Store.Projects(time.Time{})
	if err != nil {
		return 0, err
	}
	c := syncer.NewClient(auth.APIBase(config.Load()), auth.Token, d.Version)
	sent := 0
	for _, p := range ps {
		var since time.Time
		if v, _ := d.Store.GetMeta(MetaSegmentUntil(p.Key)); v != "" {
			since, _ = time.Parse(time.RFC3339, v)
		}
		plan := now
		if flush {
			plan = now.Add(segment.ReadyAfter + time.Minute) // every stretch counts as over
		}
		ranges, end, err := segment.Next(d.Store, p.Key, since, ss.Seq, plan)
		if err != nil {
			return sent, err
		}
		if len(ranges) == 0 {
			continue
		}
		for _, ro := range ranges {
			prep, err := segment.Build(d.Store, auth, ro)
			if err != nil {
				if isNoEvents(err) {
					continue // a part with nothing of this project in it
				}
				return sent, err
			}
			if !prep.Summary.ChainOK {
				logx.Printf("segments: %s does not replay against the local chain (%s); not sent", p.Key, prep.Summary.ChainErr)
				continue
			}
			resp, err := segment.Upload(ctx, c, prep.Bundle, func(w time.Duration) { sleep(ctx, w) })
			if err != nil {
				return sent, err
			}
			sent++
			if resp.Status != "verified" {
				logx.Printf("segments: %s from %s: %s %v", p.Key, ro.Since.Format(time.RFC3339), resp.Status, resp.Problems)
			}
		}
		if flush && end.After(now) {
			end = now // records made from now on go in the next segment
		}
		_ = d.Store.SetMeta(MetaSegmentUntil(p.Key), end.UTC().Format(time.RFC3339))
	}
	return sent, nil
}

func isNoEvents(err error) bool { return err != nil && strings.Contains(err.Error(), "no events") }
