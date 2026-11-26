package server

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/audit"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/promptcheck"
)

// A device sends its records as it writes them: after each sync, the part
// of its chain the server does not hold yet, up to the checkpoint that
// sync carried (POST /api/v1/records). The server keeps what replays to
// that checkpoint, so what it holds is the chain the device committed to
// while it worked.
//
// The server counts from those records on its own clock (countLoop): at
// every mark, by default each hour on the hour, it cuts each project's
// records into stretches of work as a device used to (pkg/audit Split,
// Over) and checks each stretch as it checks a segment a device uploads
// (segments.go). A stretch that is not over is counted again at each
// mark, so its AI time shows within the hour, whether or not the computer
// is still on. Nothing about checked time changes between marks.

// recordsStep is how many records the server asks for at a time when a
// device has many to send.
const recordsStep = 5000

func (s *Server) handleRecords(w http.ResponseWriter, r *http.Request) {
	dev, user, ok := s.authDevice(w, r)
	if !ok {
		return
	}
	if !s.lim.Allow("records:"+dev.ID, 1, 60) {
		w.Header().Set("Retry-After", "5")
		jsonErr(w, 429, "too many record uploads; wait a bit")
		return
	}
	var req api.RecordsReq
	if err := decodeBody(w, r, &req, maxSegmentBody, maxSegmentInflated); err != nil {
		jsonErr(w, 400, "bad records: "+err.Error())
		return
	}
	if req.DeviceID != dev.ID {
		jsonErr(w, 403, "records device_id does not match token")
		return
	}
	if s.clientTooOld(dev.ClientVersion) {
		jsonErr(w, http.StatusUpgradeRequired, s.updateClientMsg())
		return
	}
	if len(req.Entries) > api.MaxRecords {
		jsonErr(w, 413, "too many records in one upload")
		return
	}
	ctx := r.Context()
	var head int64
	st, err := s.store.RecordStream(ctx, dev.ID)
	switch {
	case err == nil:
		head = st.Head
	case !errors.Is(err, store.ErrNotFound):
		s.log.Error("record stream", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	answer := func(code int, head int64) {
		jsonOut(w, code, api.RecordsResp{Head: head, Next: s.nextRecords(ctx, dev.ID, max(head, req.From-1))})
	}
	if len(req.Entries) == 0 {
		answer(200, head)
		return
	}
	first, last := req.Entries[0].Seq, req.Entries[len(req.Entries)-1].Seq
	if first <= 0 || last-first != int64(len(req.Entries)-1) {
		jsonErr(w, 400, "bad records: the entries are not one run of the chain")
		return
	}
	if last <= head {
		answer(200, head) // sent before
		return
	}
	// only what ends at a checkpoint is kept: nothing else shows the
	// records are the chain the device committed to
	cps, err := s.store.Checkpoints(ctx, dev.ID, first, last)
	if err != nil {
		s.log.Error("records: checkpoints", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	if len(cps) == 0 || cps[len(cps)-1].Seq <= head {
		req.From = 0
		answer(http.StatusConflict, head)
		return
	}
	entries := req.Entries[:cps[len(cps)-1].Seq-first+1]
	start := req.StartHash
	if first == head+1 {
		// they go on from what the server holds: from its own head
		cp, err := s.store.CheckpointAt(ctx, dev.ID, head)
		if err == nil && cp.Hash != start {
			req.From = 0
			answer(http.StatusConflict, head)
			return
		}
	}
	if !hex64.MatchString(start) {
		jsonErr(w, 400, "bad records: start_hash")
		return
	}
	want := make([]chain.Checkpoint, len(cps))
	for i, c := range cps {
		want[i] = chain.Checkpoint{Seq: c.Seq, Hash: c.Hash}
	}
	recs, dirty, err := s.readRecords(start, entries, want, head, dev.Created)
	if err != nil {
		s.log.Warn("records do not match the chain", "device", dev.ID, "err", err)
		jsonErr(w, 400, "the records do not match the checkpoints this device sent: "+err.Error())
		return
	}
	if first > head+1 && head > 0 {
		// a hole (the device lost part of its log): what was open ends there
		if err := s.store.CloseStreamSegments(ctx, dev.ID); err != nil {
			s.log.Error("records: close segments", "err", err)
			jsonErr(w, 500, "internal error")
			return
		}
	}
	if err := s.store.AddRecords(ctx, dev.ID, user.ID, recs, dirty, s.now()); err != nil {
		s.log.Error("store records", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	req.From = 0
	answer(200, entries[len(entries)-1].Seq)
}

// nextRecords is the seq a device should send up to after base: the last
// checkpoint within recordsStep, else the first one after it; 0 when the
// server holds no checkpoint past base.
func (s *Server) nextRecords(ctx context.Context, deviceID string, base int64) int64 {
	if cps, err := s.store.Checkpoints(ctx, deviceID, base+1, base+recordsStep); err == nil && len(cps) > 0 {
		return cps[len(cps)-1].Seq
	}
	if c, err := s.store.FirstCheckpointFrom(ctx, deviceID, base+1); err == nil {
		return c.Seq
	}
	return 0
}

// readRecords replays entries from start against the checkpoints and
// returns those after head as the server keeps them, with the earliest
// record time among them. A record is kept in full only in its private
// form, of a project or the idle timer's; anything else is kept as its
// leaf hash, so that no text, command or path can be stored here.
func (s *Server) readRecords(start string, entries []chain.Entry, cps []chain.Checkpoint, head int64, created time.Time) ([]store.Record, time.Time, error) {
	// a record's time must be one the count could credit at all: not long
	// before the device signed in, not after now
	from, to := created.Add(-24*time.Hour), s.now().Add(24*time.Hour)
	want := map[int64]string{}
	for _, c := range cps {
		want[c.Seq] = c.Hash
	}
	var out []store.Record
	var dirty time.Time
	h := start
	for i, en := range entries {
		if i > 0 && en.Seq != entries[i-1].Seq+1 {
			return nil, dirty, fmt.Errorf("gap between seq %d and %d", entries[i-1].Seq, en.Seq)
		}
		rec := store.Record{Seq: en.Seq, Leaf: en.LeafHash, Prev: h}
		if e := en.Event; e != nil {
			if e.Seq != en.Seq {
				return nil, dirty, errors.New("entry seq and record seq differ")
			}
			leaf, err := chain.Leaf(*e)
			if err != nil {
				return nil, dirty, err
			}
			rec.Leaf = leaf
			if keepRecord(*e) && !e.TS.Before(from) && !e.TS.After(to) {
				b, err := json.Marshal(e)
				if err != nil {
					return nil, dirty, err
				}
				rec.TS, rec.Project, rec.Kind, rec.Data = e.TS.UTC(), e.Project, string(e.Kind), string(b)
				if repoPath(en.Path) && e.Kind == event.Edit {
					rec.Path = en.Path
				}
				if en.Seq > head && (dirty.IsZero() || rec.TS.Before(dirty)) {
					dirty = rec.TS
				}
			}
		}
		var err error
		if h, err = chain.Link(h, rec.Leaf); err != nil {
			return nil, dirty, err
		}
		if w, ok := want[en.Seq]; ok && w != h {
			return nil, dirty, fmt.Errorf("checkpoint mismatch at seq %d", en.Seq)
		}
		if en.Seq > head {
			out = append(out, rec)
		}
	}
	return out, dirty, nil
}

// keepRecord reports whether a record is one the server keeps in full: in
// the keyed private form, with a time, of a project or the idle timer's.
func keepRecord(e event.Event) bool {
	if e.Canon < event.CanonKeyed || !chain.Private(e) || e.TS.Unix() < 1.5e9 || len(e.Project) > 300 || len(e.Kind) > 40 ||
		strings.ContainsRune(e.Project, 0) || strings.ContainsRune(string(e.Kind), 0) {
		return false
	}
	return e.Project != "" || e.Kind == event.Idle
}

// repoPath reports whether p reads as a file's path inside a repo: never
// absolute, never leading out.
func repoPath(p string) bool {
	return p != "" && len(p) <= 400 && !strings.ContainsAny(p, "\\\x00\n\r") && !strings.HasPrefix(p, "/") &&
		!strings.HasPrefix(p, "~") && path.Clean(p) == p && p != "." && p != ".." && !strings.HasPrefix(p, "../")
}

// ---- counting ----

// countLoop counts at every mark of the clock (Config.CountEvery; the
// hour, on the hour). One process counts at a time, and a mark it did not
// finish (the server stopped) is counted when a server next looks.
func (s *Server) countLoop(ctx context.Context) {
	every := s.countEvery()
	for ctx.Err() == nil {
		mark := time.Now().UTC().Truncate(every)
		wait := time.Until(mark.Add(every)) + time.Second
		if done, err := s.store.CountDone(ctx); err != nil {
			s.log.Error("count cycle", "err", err)
			wait = time.Minute
		} else if done.Before(mark) && !s.countMark(ctx, mark) {
			wait = min(wait, time.Minute) // another server is counting, or this one was stopped
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

func (s *Server) countEvery() time.Duration {
	if s.cfg.CountEvery >= time.Minute {
		return s.cfg.CountEvery
	}
	return time.Hour
}

// cycleWords says when the server counts, for the pages: "on the hour",
// or "every 15m" for another cycle.
func (s *Server) cycleWords() string {
	if every := s.countEvery(); every != time.Hour {
		return "every " + fmtDur(every.Seconds())
	}
	return "on the hour"
}

// countClaim is how long one device's count may take before another
// server may take the cycle over.
const countClaim = 10 * time.Minute

// countMark runs the cycle of one mark. false: another server holds the
// turn, or it did not finish.
func (s *Server) countMark(ctx context.Context, mark time.Time) bool {
	if ok, err := s.store.CountClaim(ctx, time.Now(), countClaim); err != nil || !ok {
		return false
	}
	t0 := time.Now()
	n, err := s.CountStreams(ctx, func() { s.store.CountClaim(ctx, time.Now(), 0) })
	if err != nil {
		s.log.Error("count cycle", "mark", mark, "err", err)
		return false
	}
	if err := s.store.SetCountDone(ctx, mark); err != nil {
		s.log.Error("count cycle", "err", err)
		return false
	}
	s.log.Info("counted", "mark", mark.Format(time.RFC3339), "devices", n, "took", time.Since(t0).Round(time.Millisecond).String())
	return true
}

// CountStreams is one cycle: every device that sent records since the
// last one, or has a stretch that is open or waits for prompt checks, is
// counted again. It returns how many devices it counted. beat, when set,
// is called between devices.
func (s *Server) CountStreams(ctx context.Context, beat func()) (int, error) {
	dirty, err := s.store.DirtyStreams(ctx)
	if err != nil {
		return 0, err
	}
	open, err := s.store.UnsettledStreams(ctx)
	if err != nil {
		return 0, err
	}
	ids := open
	for _, d := range dirty {
		ids = append(ids, d.DeviceID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	n := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		if beat != nil {
			beat()
		}
		if err := s.countDevice(ctx, id, beat); err != nil {
			s.log.Error("count device", "device", id, "err", err)
			continue
		}
		n++
	}
	return n, nil
}

// deviceBudget is the most one cycle spends on one device. What is left
// is counted at the next cycle, so that one device with a great many
// records or projects cannot hold up the others.
const deviceBudget = 2 * time.Minute

// countDevice cuts and counts one device's records: its stretches that are
// open, that wait for prompt checks, or whose time got new records. A
// project that fails is logged and tried again at the next cycle; the
// device's other projects are still counted. beat, when set, is called
// between projects.
func (s *Server) countDevice(ctx context.Context, deviceID string, beat func()) error {
	began := time.Now()
	dev, err := s.store.DeviceByID(ctx, deviceID)
	if err != nil {
		return err
	}
	user, err := s.store.UserByID(ctx, dev.UserID)
	if err != nil {
		return err
	}
	st, err := s.store.RecordStream(ctx, deviceID)
	if err != nil {
		return err
	}
	if !user.Deleted.IsZero() {
		return s.store.ClearDirty(ctx, deviceID, st.Received)
	}
	// prompts the server asked for and that never came are settled first,
	// as before an uploaded segment is counted again (recheckUser)
	if err := s.settlePulls(ctx, user.ID); err != nil {
		s.log.Error("settle pulls", "err", err)
	}
	segs, err := s.store.StreamSegments(ctx, deviceID, false)
	if err != nil {
		return err
	}
	// the projects to look at: those of the records that came since the
	// last count, and those with a stretch that is open, that waits, or
	// whose time those records reach back into (a late idle-timer record
	// names no project)
	var keys []string
	if !st.Dirty.IsZero() {
		if keys, err = s.store.RecordKeys(ctx, deviceID, st.RunLo, st.Dirty); err != nil {
			return err
		}
	}
	byKey := map[string][]*store.Segment{}
	for i := range segs {
		sg := &segs[i]
		byKey[sg.RecKey] = append(byKey[sg.RecKey], sg)
		if (sg.Open || sg.Status == store.SegPending || !st.Dirty.IsZero() && sg.Until.After(st.Dirty)) && !slices.Contains(keys, sg.RecKey) {
			keys = append(keys, sg.RecKey)
		}
	}
	slices.Sort(keys)
	failed := 0
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Since(began) > deviceBudget {
			return fmt.Errorf("stopped after %s with projects left; they are counted at the next cycle", deviceBudget)
		}
		if beat != nil {
			beat()
		}
		if err := s.countKey(ctx, dev, user, st, key, byKey[key]); err != nil {
			s.log.Error("count project", "device", deviceID, "project", key, "err", err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d projects were not counted", failed, len(keys))
	}
	return s.store.ClearDirty(ctx, deviceID, st.Received)
}

// countKey cuts and counts the records of one project key of a device.
// mine are the stretches the server made for it so far, oldest first.
func (s *Server) countKey(ctx context.Context, dev *store.Device, user *store.User, st *store.RecordStream, key string, mine []*store.Segment) error {
	deviceID, now := dev.ID, s.now()
	proj, err := s.store.DeviceProject(ctx, user.ID, dev.ID, api.ProjectMeta{Key: key})
	if err != nil {
		return err
	}
	var open *store.Segment
	var since time.Time
	again := 0
	for _, sg := range slices.Backward(mine) { // newest first
		if sg.Open {
			open = sg
			continue
		}
		since = maxTime(since, sg.Until)
		// a stretch that is over: again when its prompt checks may
		// have come, or records with a time inside it did
		if (sg.Status == store.SegPending || !st.Dirty.IsZero() && sg.Until.After(st.Dirty)) && again < againMax {
			again++
			if err := s.countStretch(ctx, dev, user, proj, sg, key, sg.SeqLo, audit.Range{Since: sg.Since, Until: sg.Until}, 0); err != nil {
				return err
			}
		}
	}
	lo := st.RunLo
	if open != nil {
		since, lo = open.Since, min(lo, open.SeqLo)
	} else if since.IsZero() {
		// after what a device uploaded itself before it sent records
		if since, err = s.uploadedUpTo(ctx, user.ID, proj.ID, dev.ID); err != nil {
			return err
		}
	}
	rts, err := s.store.RecordTimes(ctx, deviceID, key, lo, since, time.Time{})
	if err != nil {
		return err
	}
	if len(rts) == 0 {
		return nil
	}
	ts := make([]time.Time, len(rts))
	for i, r := range rts {
		ts[i] = r.TS
	}
	if since.IsZero() {
		since = ts[0].Add(-audit.SplitPad)
	}
	// the stretches that are over, then the one that is not
	var ranges []audit.Range
	end := audit.Over(ts, now)
	if !end.IsZero() {
		in := ts
		if i := slices.IndexFunc(ts, func(t time.Time) bool { return !t.Before(end) }); i >= 0 {
			in = ts[:i]
		}
		if len(in) > 0 {
			ranges = audit.Split(in, since, end)
			since = end
		}
	}
	if last := ts[len(ts)-1]; end.IsZero() || !last.Before(end) {
		tail := []audit.Range{{Since: since}}
		if last.Sub(since) > openMax {
			// work with no long pause for days: cut at its longest pauses
			rest := ts[slices.IndexFunc(ts, func(t time.Time) bool { return !t.Before(since) }):]
			tail = audit.SplitAt(rest, since, last.Add(audit.SplitPad), openMax)
			tail[len(tail)-1].Until = time.Time{}
		}
		ranges = append(ranges, tail...)
	}
	for i, r := range ranges {
		sg := open
		if i > 0 || sg == nil {
			sg = &store.Segment{UserID: user.ID, DeviceID: dev.ID, Stream: true, RecKey: key, Created: now.Add(time.Duration(i) * time.Millisecond)}
		}
		if err := s.countStretch(ctx, dev, user, proj, sg, key, lo, r, 0); err != nil {
			return err
		}
	}
	return nil
}

// openMax is the longest a stretch stays open. One with no pause of
// audit.SplitGap for longer than this is cut anyway, so that no count
// reads more than this much of a device's records.
const openMax = 48 * time.Hour

// againMax is the most stretches that are over which one cycle counts
// again for one project of one device, newest first.
const againMax = 20

// bundleMax is the most entries one stretch may span.
const bundleMax = 300000

// uploadedUpTo is the time of the last record of a project in the
// segments a device uploaded itself (an older SeaTime did; zero: none).
// The stretches the server cuts begin after it.
func (s *Server) uploadedUpTo(ctx context.Context, userID, projectID int64, deviceID string) (time.Time, error) {
	segs, err := s.store.ListSegments(ctx, userID, projectID)
	if err != nil {
		return time.Time{}, err
	}
	var t time.Time
	for _, sg := range segs {
		if !sg.Stream && sg.DeviceID == deviceID && sg.LastTS.After(t) {
			t = sg.LastTS
		}
	}
	if !t.IsZero() {
		t = t.Add(time.Millisecond)
	}
	return t, nil
}

// countStretch builds one stretch of a project's records as a segment
// (new, or sg stored again) and checks and counts it as a segment a device
// uploads. lo is the seq its records start from at the earliest. Segments
// made after this one that hold the same minutes are then counted again
// (countLater); depth is how many such steps led here.
func (s *Server) countStretch(ctx context.Context, dev *store.Device, user *store.User, proj *store.Project, sg *store.Segment, key string, lo int64, r audit.Range, depth int) error {
	b, err := s.streamBundle(ctx, dev, key, lo, r)
	if err != nil || b == nil {
		return err
	}
	fresh, wasOpen := sg.ID == "", sg.Open
	if fresh {
		sg.ID = randHex(8)
	}
	sg.ProjectID, sg.ProjectKey, sg.ScoreVer = proj.ID, proj.Key, b.ScoreVersion
	sg.Since, sg.Until, sg.Open = r.Since, r.Until, r.Until.IsZero()
	a, minutes, err := s.checkSegment(ctx, dev, b, sg)
	if err != nil {
		return err
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	json.NewEncoder(zw).Encode(b)
	zw.Close()
	if fresh {
		err = s.store.PutSegment(ctx, sg, gz.Bytes(), minutes)
	} else {
		// (a stretch that was closed while this count ran, because the
		// device's records went on after a hole, is left for the next cycle)
		err = s.store.ReplaceSegment(ctx, sg, wasOpen, gz.Bytes(), minutes)
	}
	if err != nil {
		return err
	}
	if err := s.showChecked(ctx, sg, proj.Key, minutes); err != nil {
		return err
	}
	s.countLater(ctx, sg, minutes, depth)
	var cs []store.Commit
	for _, e := range a.Events {
		if e.Kind == event.Commit && e.Project == b.Project && shaRe.MatchString(strings.ToLower(e.SHA)) {
			cs = append(cs, store.Commit{ProjectID: proj.ID, SHA: strings.ToLower(e.SHA), TS: e.TS, Ins: e.Insertions, Del: e.Deletions})
		}
	}
	if err := s.store.UpsertCommits(ctx, cs); err != nil {
		s.log.Error("upsert commits", "err", err)
	}
	s.log.Info("stretch", "id", sg.ID, "user", user.Username, "status", sg.Status, "open", sg.Open, "problems", len(sg.Problems))
	return nil
}

// countLater counts again the account's segments that were made after sg
// and hold a minute in which, with sg's count as it is now, more than 60
// seconds are credited. A segment gives up the minutes that segments made
// before it already credit, at the moment it is counted (claimedMinutes).
// A stretch the server keeps counting can come to hold minutes after a
// later segment took them: another device's stretch, or a segment a
// device uploaded. Without this, such a minute would count twice.
func (s *Server) countLater(ctx context.Context, sg *store.Segment, minutes []store.SegmentMinute, depth int) {
	if sg.Status == store.SegFailed || depth >= 20 {
		return
	}
	mine := map[int64]float64{}
	lo, hi := int64(math.MaxInt64), int64(math.MinInt64)
	for _, m := range minutes {
		if m.Human > 0 {
			mine[m.StartMin] = m.Human
			lo, hi = min(lo, m.StartMin), max(hi, m.StartMin)
		}
	}
	if len(mine) == 0 {
		return
	}
	claims, err := s.store.MinuteClaims(ctx, sg.UserID, lo, hi, time.UnixMilli(math.MaxInt64/2), sg.ID)
	if err != nil {
		s.log.Error("count later segments", "err", err)
		return
	}
	theirs, who := map[int64]float64{}, map[int64][]string{}
	for _, c := range claims {
		if _, ok := mine[c.StartMin]; ok {
			theirs[c.StartMin] += c.Human
			who[c.StartMin] = append(who[c.StartMin], c.SegmentID)
		}
	}
	ids := map[string]bool{}
	for m, sec := range mine {
		if sec+theirs[m] > 60.5 {
			for _, id := range who[m] {
				ids[id] = true
			}
		}
	}
	var later []*store.Segment
	for id := range ids {
		o, err := s.store.Segment(ctx, id)
		if err == nil && o.Status != store.SegFailed && (o.Created.After(sg.Created) || o.Created.Equal(sg.Created) && o.ID > sg.ID) {
			later = append(later, o)
		}
	}
	slices.SortFunc(later, func(a, b *store.Segment) int {
		return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.ID, b.ID))
	})
	for _, o := range later {
		if err := s.countAgain(ctx, o, depth+1); err != nil {
			s.log.Error("count later segment", "id", o.ID, "err", err)
		}
	}
}

// countAgain counts a stored segment again: one the server made from the
// device's records as they are now, one a device uploaded from its bundle.
func (s *Server) countAgain(ctx context.Context, sg *store.Segment, depth int) error {
	if !sg.Stream {
		return s.recheckSegment(ctx, sg)
	}
	dev, err := s.store.DeviceByID(ctx, sg.DeviceID)
	if err != nil {
		return err
	}
	user, err := s.store.UserByID(ctx, dev.UserID)
	if err != nil {
		return err
	}
	proj, err := s.store.DeviceProject(ctx, user.ID, dev.ID, api.ProjectMeta{Key: sg.RecKey})
	if err != nil {
		return err
	}
	return s.countStretch(ctx, dev, user, proj, sg, sg.RecKey, sg.SeqLo, audit.Range{Since: sg.Since, Until: sg.Until}, depth)
}

// streamBundle builds the bundle of one stretch from the records a device
// sent: the project's records with a time in the range and the idle-timer
// records of that time in full, everything between them as leaf hashes,
// from the project's first record to the next checkpoint after its last.
// nil when the range holds no record of the project.
func (s *Server) streamBundle(ctx context.Context, dev *store.Device, key string, lo int64, r audit.Range) (*api.Bundle, error) {
	rts, err := s.store.RecordTimes(ctx, dev.ID, key, lo, r.Since, r.Until)
	if err != nil || len(rts) == 0 {
		return nil, err
	}
	first := rts[0].Seq
	for _, t := range rts {
		first = min(first, t.Seq)
	}
	last, err := s.store.LastRecordSeq(ctx, dev.ID, key, first, r.Until)
	if err != nil {
		return nil, err
	}
	cp, err := s.store.FirstCheckpointFrom(ctx, dev.ID, last)
	if err != nil {
		return nil, err
	}
	if cp.Seq-first >= bundleMax {
		return nil, fmt.Errorf("a stretch of %s spans %d records, more than one count reads", key, cp.Seq-first+1)
	}
	rows, err := s.store.RecordRows(ctx, dev.ID, first, cp.Seq)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	b := &api.Bundle{DeviceID: dev.ID, Project: key, StartHash: rows[0].Prev}
	var from, to time.Time // the project's first and last record
	for i, row := range rows {
		if i > 0 && row.Seq != rows[i-1].Seq+1 {
			break // a hole: the stretch ends with the run
		}
		en := chain.Entry{Seq: row.Seq, LeafHash: row.Leaf}
		mine := row.Data != "" && (row.Project == key || row.Project == "" && row.Kind == string(event.Idle)) &&
			!row.TS.Before(r.Since) && (r.Until.IsZero() || row.TS.Before(r.Until))
		if mine {
			var e event.Event
			if json.Unmarshal([]byte(row.Data), &e) == nil {
				en = chain.Entry{Seq: row.Seq, Event: &e, Path: row.Path}
				if row.Project == key {
					if from.IsZero() || e.TS.Before(from) {
						from = e.TS
					}
					if e.TS.After(to) {
						to = e.TS
					}
				}
			}
		}
		b.Entries = append(b.Entries, en)
	}
	if from.IsZero() {
		return nil, nil
	}
	// the other projects' counted heartbeats, which split this one's gaps
	// (audit.Others), from the same records
	around, err := s.store.RecordsBetween(ctx, dev.ID, from.Add(-othersLead), to.Add(time.Millisecond), true)
	if err != nil {
		return nil, err
	}
	var evs []event.Event
	var hashes []string
	for _, row := range around {
		var e event.Event
		if json.Unmarshal([]byte(row.Data), &e) != nil {
			continue
		}
		evs = append(evs, e)
		if e.Kind == event.Prompt && e.CheckHash != "" {
			hashes = append(hashes, e.CheckHash)
		}
	}
	set, err := s.promptChecksOf(ctx, dev.ID, hashes)
	if err != nil {
		return nil, err
	}
	if set != nil {
		evs, _ = set.Apply(evs)
	}
	b.ScoreVersion = set.Version(promptcheck.Span(audit.Revealed(*b)))
	if set.AnyOn(from, to) {
		b.Others, b.OthersUntil = audit.OthersCap(evs, set, key, from, to)
	} else {
		b.Others = audit.Others(evs, key, from, to)
	}
	return b, nil
}

// othersLead is how much of a device's records before a stretch are read
// for the heartbeats around it (as a device reads its own log).
const othersLead = 24 * time.Hour
