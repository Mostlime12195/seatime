// Package records sends this device's log to the server as it is written
// (POST /api/v1/records): after each sync, the records the server does
// not hold yet, up to the checkpoint that sync carried. The server counts
// each project's time from them on its own clock, so nothing waits for a
// stretch of work to be over or for this computer to stay on.
//
// Every record of a project, and every idle-timer record, goes in its
// private form (chain.Hide: no prompt text, command, file path, folder or
// extras; the project by its server key). Everything else is a bare leaf
// hash. An editor heartbeat on a file inside its project's git repo
// carries that file's repo-relative path beside it (segment.RepoPath).
package records

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/segment"
	"seatime/internal/client/stats"
	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/event"
)

// MetaHead holds the seq up to which the server holds this device's
// records, as it last said ("" until it has taken some).
const MetaHead = "records_head"

// ErrUnsupported: the server does not take records (it is older).
var ErrUnsupported = errors.New("the server does not take records")

// batch is the most records sent in one request when the server does not
// say how far to send. most is the most one request ever carries: a
// longer run between two checkpoints was written while this device sent
// none for a long time, which the server does not count, so its start is
// left out.
const (
	batch = 5000
	most  = 20000
)

// Sent reports whether the server has ever taken this device's records.
func Sent(st *store.Store) bool {
	v, _ := st.GetMeta(MetaHead)
	return v != ""
}

// Push sends the records the server does not hold yet, up to the last
// synced checkpoint. It returns how many went up.
func Push(ctx context.Context, st *store.Store, auth config.Auth, c *syncer.Client) (int, error) {
	ss, err := st.SyncState()
	if err != nil || ss.Seq == 0 {
		return 0, err
	}
	head, known := int64(0), false
	if v, _ := st.GetMeta(MetaHead); v != "" {
		head, _ = strconv.ParseInt(v, 10, 64)
		known = true
	}
	if known && head >= ss.Seq {
		return 0, nil
	}
	lo, next := head+1, ss.Seq
	if !known {
		if lo, err = start(st, ss.Seq); err != nil {
			return 0, err
		}
	}
	// records this log no longer holds (it was lost and started again)
	// cannot be sent: the upload goes on from the first one it has
	base, _, err := st.Base()
	if err != nil {
		return 0, err
	}
	lo = max(lo, base+1)
	if lo > ss.Seq {
		if !known {
			// nothing to send yet; ask once, to learn whether the server
			// takes records at all, and start with the next one
			if _, err := c.Records(ctx, api.RecordsReq{DeviceID: auth.DeviceID, From: lo}); err != nil {
				return 0, unsupported(err)
			}
			return 0, setHead(st, ss.Seq)
		}
		return 0, nil
	}
	if !known || lo != head+1 || ss.Seq-lo >= batch {
		// the first time, after a hole, or with much to send: ask where to
		// go on from and how far to send at once
		resp, err := c.Records(ctx, api.RecordsReq{DeviceID: auth.DeviceID, From: lo})
		if err != nil {
			return 0, unsupported(err)
		}
		if resp.Head >= lo {
			if err := setHead(st, resp.Head); err != nil {
				return 0, err
			}
			lo = resp.Head + 1
		}
		next = resp.Next
	}
	sent := 0
	for tries := 0; next >= lo && next <= ss.Seq && tries < 200; tries++ {
		if next-lo+1 > most {
			lo = next - most + 1
		}
		req, err := build(st, auth.DeviceID, lo, next)
		if err != nil {
			return sent, err
		}
		resp, err := c.Records(ctx, req)
		var he *syncer.HTTPError
		if errors.As(err, &he) && he.Status == 409 && resp.Head >= lo {
			// the server holds more than this device thought: go on from there
			if err := setHead(st, resp.Head); err != nil {
				return sent, err
			}
			lo, next = resp.Head+1, resp.Next
			continue
		}
		if err != nil {
			return sent, unsupported(err)
		}
		if resp.Head < next {
			return sent, errors.New("the server did not take the records")
		}
		sent += int(next - lo + 1)
		if err := setHead(st, resp.Head); err != nil {
			return sent, err
		}
		lo, next = resp.Head+1, resp.Next
	}
	return sent, nil
}

func unsupported(err error) error {
	var he *syncer.HTTPError
	if errors.As(err, &he) && (he.Status == 404 || he.Status == 405) {
		return ErrUnsupported
	}
	return err
}

func setHead(st *store.Store, seq int64) error {
	return st.SetMeta(MetaHead, strconv.FormatInt(seq, 10))
}

// start is the first record to send when the server holds none of this
// device's: the first one that no segment this device uploaded covers
// (an older SeaTime uploaded each project's stretches itself), or the
// first of the log.
func start(st *store.Store, head int64) (int64, error) {
	ps, err := st.Projects(time.Time{})
	if err != nil {
		return 0, err
	}
	lo := head + 1
	for _, p := range ps {
		var since time.Time
		if v, _ := st.GetMeta(segment.MetaUntil(p.Key)); v != "" {
			since, _ = time.Parse(time.RFC3339, v)
		}
		first, _, n, err := st.SeqRange(store.Query{Project: p.Key, Since: since, UpToSeq: head})
		if err != nil {
			return 0, err
		}
		if n > 0 && first < lo {
			lo = first
		}
	}
	// and the last edit or prompt before it, when it is under a day old:
	// the first gap of the work that follows counts from there, as it does
	// on this device
	if lo > 1 && lo <= head { // (a seq of 0 in a query means no bound)
		evs, err := st.Events(store.Query{AfterSeq: lo - 1, UpToSeq: lo})
		if err != nil || len(evs) == 0 {
			return lo, err
		}
		prev, err := st.Events(store.Query{UpToSeq: lo - 1, Since: evs[0].TS.Add(-stats.Lead), Kinds: []event.Kind{event.Edit, event.Prompt}, Desc: true, Limit: 1})
		if err != nil {
			return lo, err
		}
		if len(prev) == 1 {
			lo = prev[0].Seq
		}
	}
	return lo, nil
}

// build is the request for the records lo..hi.
func build(st *store.Store, deviceID string, lo, hi int64) (api.RecordsReq, error) {
	req := api.RecordsReq{DeviceID: deviceID}
	rows, err := st.Rows(lo, hi)
	if err != nil {
		return req, err
	}
	if len(rows) == 0 || rows[0].Seq != lo || rows[len(rows)-1].Seq != hi {
		return req, errors.New("the local log does not hold records " + strconv.FormatInt(lo, 10) + " to " + strconv.FormatInt(hi, 10))
	}
	if req.StartHash, err = st.HashAt(lo - 1); err != nil {
		return req, err
	}
	repos := map[string]string{}
	for _, r := range rows {
		en, err := Entry(st, r, repos)
		if err != nil {
			return req, err
		}
		req.Entries = append(req.Entries, en)
	}
	return req, nil
}

// Entry is one record as it is sent: in its private form when it is a
// project's or the idle timer's, else as its leaf hash. A record written
// before the keyed form commits to paths or guessable hashes, and a
// damaged one cannot be read: both go as leaf hashes. repos caches each
// project's repo root.
func Entry(st *store.Store, r store.Row, repos map[string]string) (chain.Entry, error) {
	leaf := chain.Entry{Seq: r.Seq, LeafHash: r.Leaf}
	var e event.Event
	if json.Unmarshal(r.Data, &e) != nil || e.Canon < event.CanonKeyed || r.Project == "" && e.Kind != event.Idle {
		return leaf, nil
	}
	repo, ok := repos[r.Project]
	if !ok && r.Project != "" {
		repo = segment.RepoRoot(st, r.Project)
		repos[r.Project] = repo
	}
	named := segment.RepoPath(e, repo)
	h, err := st.Hide(e)
	if err != nil {
		return leaf, err
	}
	return chain.Entry{Seq: r.Seq, Event: &h, Path: named}, nil
}
