package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/audit"
	"seatime/pkg/ledger"
)

// Continuous verification. A segment is one stretch of one project's
// records in their private form (pkg/api Bundle). The server cuts it from
// the records a device sends as it writes them (records.go); a device
// older than that uploads it itself once the stretch is over. Either way
// the server checks it against the checkpoints the device synced in real
// time and counts it with the one-page rule. An uploaded segment whose
// prompts wait for their check is counted again when the checks come
// back. Records that do not match the chain do not count, and the project
// shows the reason (apps.go).

// Segment body limits: a segment is one stretch of work; a month of heavy
// use is a few MB.
const (
	maxSegmentBody     = 32 << 20
	maxSegmentInflated = 64 << 20
)

// handleSubmissionsGone answers clients from before segments.
func (s *Server) handleSubmissionsGone(w http.ResponseWriter, r *http.Request) {
	jsonErr(w, http.StatusGone, "SeaTime no longer takes submissions. Applications such as YSWS program sites now read your hours from SeaTime directly; there is nothing to send. Update SeaTime.")
}

func (s *Server) handleSegment(w http.ResponseWriter, r *http.Request) {
	dev, user, ok := s.authDevice(w, r)
	if !ok {
		return
	}
	if !s.lim.Allow("segment:"+dev.ID, 1.0/5, 30) {
		w.Header().Set("Retry-After", "30")
		jsonErr(w, 429, "too many segments; wait a bit")
		return
	}
	select {
	case s.subSem <- struct{}{}:
		defer func() { <-s.subSem }()
	default:
		w.Header().Set("Retry-After", "10")
		jsonErr(w, 503, "busy, retry shortly")
		return
	}
	var b api.Bundle
	if err := decodeBody(w, r, &b, maxSegmentBody, maxSegmentInflated); err != nil {
		jsonErr(w, 400, "bad segment: "+err.Error())
		return
	}
	if b.DeviceID != dev.ID {
		jsonErr(w, 403, "segment device_id does not match token")
		return
	}
	if s.clientTooOld(dev.ClientVersion) {
		jsonErr(w, http.StatusUpgradeRequired, s.updateClientMsg())
		return
	}
	if b.Project == "" || len(b.Project) > 300 {
		jsonErr(w, 400, "bad project")
		return
	}
	if err := audit.CheckSignature(dev.PublicKey, b); err != nil {
		jsonErr(w, 400, "bad segment signature: "+err.Error())
		return
	}
	ctx := r.Context()
	proj, err := s.store.DeviceProject(ctx, user.ID, dev.ID, api.ProjectMeta{Key: b.Project})
	if err != nil {
		jsonErr(w, 500, "internal error")
		return
	}
	now := s.now()
	sg := &store.Segment{ID: randHex(8), UserID: user.ID, ProjectID: proj.ID, ProjectKey: b.Project, DeviceID: dev.ID,
		Claimed: b.Claimed, ScoreVer: b.ScoreVersion, Created: now}
	a, minutes, err := s.checkSegment(ctx, dev, &b, sg)
	if err != nil {
		s.log.Error("check segment", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	json.NewEncoder(zw).Encode(b)
	zw.Close()
	if err := s.store.PutSegment(ctx, sg, gz.Bytes(), minutes); err != nil {
		s.log.Error("store segment", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	if err := s.showChecked(ctx, sg, proj.Key, minutes); err != nil {
		s.log.Error("show checked minutes", "err", err)
	}
	// the segment's commits go on the project page
	var cs []store.Commit
	for _, e := range a.Events {
		if e.Kind == "commit" && e.Project == b.Project && shaRe.MatchString(strings.ToLower(e.SHA)) {
			cs = append(cs, store.Commit{ProjectID: proj.ID, SHA: strings.ToLower(e.SHA), TS: e.TS, Ins: e.Insertions, Del: e.Deletions})
		}
	}
	if err := s.store.UpsertCommits(ctx, cs); err != nil {
		s.log.Error("upsert commits", "err", err)
	}
	s.log.Info("segment", "id", sg.ID, "user", user.Username, "status", sg.Status, "problems", len(sg.Problems))
	jsonOut(w, 200, api.SegmentResp{ID: sg.ID, Status: sg.Status, Problems: sg.Problems, Scored: sg.Scored,
		Hours: audit.Hours(sg.Scored.Seconds(), sg.Status), Pending: sg.Pending})
}

// checkSegment analyses a segment against the device's checkpoints and
// sets its status, problems, numbers and times; minutes are what it
// credits. sg.Created orders it among the account's segments: a minute an
// earlier segment credits is not credited again.
func (s *Server) checkSegment(ctx context.Context, dev *store.Device, b *api.Bundle, sg *store.Segment) (*Analysis, []store.SegmentMinute, error) {
	a, err := s.analyze(ctx, dev, b, sg)
	if err != nil {
		return nil, nil, err
	}
	sg.SeqLo, sg.SeqHi, sg.Scored, sg.Pending, sg.Checked = a.SeqLo, a.SeqHi, a.Scored, a.Pending, s.now()
	sg.FirstTS, sg.LastTS = time.Time{}, time.Time{}
	for _, e := range a.Events {
		if e.Project != b.Project {
			continue
		}
		if sg.FirstTS.IsZero() || e.TS.Before(sg.FirstTS) {
			sg.FirstTS = e.TS
		}
		if e.TS.After(sg.LastTS) {
			sg.LastTS = e.TS
		}
	}
	sg.Problems = chainProblems(a.Problems)
	switch {
	case len(sg.Problems) > 0:
		sg.Status = store.SegFailed
	case a.Pending > 0 && !sg.Open || s.pullsOpen(ctx, sg.UserID, b.Project):
		// a stretch that is not over is counted again at every cycle
		// anyway: a prompt of it still being checked adds its time then
		sg.Status = store.SegPending
	default:
		sg.Status = store.SegVerified
	}
	var minutes []store.SegmentMinute
	for _, bk := range a.Buckets {
		minutes = append(minutes, store.SegmentMinute{StartMin: bk.Start.Unix() / 60, Human: bk.Seconds(), Dark: bk.AINotCountedSeconds,
			Hand: bk.CodingSeconds, Prompts: bk.Prompts, Models: bk.Models})
	}
	return a, minutes, nil
}

// chainProblems are the analysis's problems that fail a segment. The
// server counts the records itself, so a device that counted differently
// (an older rule, or prompt checks that came back after it counted) is not
// a problem; records that do not match the chain, or that the chain's
// checkpoints do not cover, are.
func chainProblems(ps []string) []string {
	var out []string
	for _, p := range ps {
		if strings.HasPrefix(p, "claimed ") || strings.HasPrefix(p, "rule version mismatch") || strings.HasPrefix(p, "this server checks prompts") {
			continue
		}
		out = append(out, p)
	}
	return out
}

// pullsOpen reports whether the account's prompts for a project key were
// asked for and have not all come back (an open pull for one of the keys,
// or for every project).
func (s *Server) pullsOpen(ctx context.Context, userID int64, keys ...string) bool {
	ps, err := s.store.PromptPulls(ctx, userID, true)
	if err != nil {
		return false
	}
	for _, p := range ps {
		if p.Project == "" || slices.Contains(keys, p.Project) {
			return true
		}
	}
	return false
}

// recordKeys are the keys a project's records carry: every key a device
// has sent for it, and the key it goes by.
func (s *Server) recordKeys(ctx context.Context, p *store.Project) []string {
	keys := []string{p.Key}
	ks, err := s.store.ProjectKeys(ctx, p.ID)
	if err != nil {
		s.log.Error("project keys", "err", err)
	}
	for _, k := range ks {
		if !slices.Contains(keys, k.Key) {
			keys = append(keys, k.Key)
		}
	}
	return keys
}

// recheckSegment counts a stored segment again (its prompts' checks came
// back, or its prompts were asked for).
func (s *Server) recheckSegment(ctx context.Context, sg *store.Segment) error {
	raw, err := s.store.SegmentBundle(ctx, sg.ID)
	if err != nil {
		return err
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	var b api.Bundle
	if err := json.NewDecoder(io.LimitReader(zr, maxSegmentInflated)).Decode(&b); err != nil {
		return err
	}
	dev, err := s.store.DeviceByID(ctx, sg.DeviceID)
	if err != nil {
		return err
	}
	_, minutes, err := s.checkSegment(ctx, dev, &b, sg)
	if err != nil {
		return err
	}
	if err := s.store.PutSegment(ctx, sg, nil, minutes); err != nil {
		return err
	}
	proj, err := s.store.ProjectByID(ctx, sg.ProjectID)
	if err != nil {
		return err
	}
	return s.showChecked(ctx, sg, proj.Key, minutes)
}

// segDevice is the device id under which a segment's minutes, as the server
// counted them, are kept among the synced minutes (showChecked).
func segDevice(segmentID string) string { return store.SegDevicePrefix + segmentID }

// showChecked puts a segment's minutes, as the server counted them, where
// the site reads counted time: among the minutes that devices sync, under a
// device id of the segment's own, so that no device's sync changes them.
// With the prompt check on, a device does not know which prompts count and
// syncs hand time only; the AI time of a stretch of work appears here, once
// its records have been checked. Readers take the largest duration of a
// minute's rows, and the counts (prompts, edits, commits, runs) stay the
// device's. A segment that fails shows none.
func (s *Server) showChecked(ctx context.Context, sg *store.Segment, key string, minutes []store.SegmentMinute) error {
	var out []ledger.Bucket
	if sg.Status != store.SegFailed {
		for _, m := range minutes {
			if m.Human <= 0 && m.Dark <= 0 {
				continue
			}
			hand := min(m.Hand, m.Human)
			nb := ledger.Bucket{Project: key, Start: time.Unix(m.StartMin*60, 0).UTC(), CodingSeconds: hand, AISeconds: m.Human - hand, AINotCountedSeconds: m.Dark}
			for _, u := range m.Models {
				if u.Seconds > 0 {
					nb.Models = append(nb.Models, ledger.ModelUse{Model: u.Model, Seconds: u.Seconds})
				}
			}
			out = append(out, nb)
		}
	}
	dev := segDevice(sg.ID)
	if !sg.FirstTS.IsZero() {
		if err := s.store.DeleteBuckets(ctx, sg.UserID, dev, sg.FirstTS.Add(-time.Hour).Truncate(time.Minute), sg.LastTS.Add(time.Hour), out); err != nil {
			return err
		}
	}
	return s.store.UpsertBuckets(ctx, sg.UserID, dev, out)
}

// showAllChecked runs showChecked over every stored segment: segments
// checked before the server kept their minutes among the synced ones. It
// writes nothing for a segment whose minutes are there already.
func (s *Server) showAllChecked(ctx context.Context) {
	sgs, err := s.store.AllSegments(ctx)
	if err != nil {
		s.log.Error("show checked minutes", "err", err)
		return
	}
	keys := map[int64]string{}
	n := 0
	for i := range sgs {
		sg := &sgs[i]
		if ctx.Err() != nil {
			return
		}
		key, ok := keys[sg.ProjectID]
		if !ok {
			p, err := s.store.ProjectByID(ctx, sg.ProjectID)
			if err != nil {
				continue
			}
			key, keys[sg.ProjectID] = p.Key, p.Key
		}
		minutes, err := s.store.SegmentMinutesOf(ctx, sg.ID)
		if err == nil {
			err = s.showChecked(ctx, sg, key, minutes)
		}
		if err != nil {
			s.log.Error("show checked minutes", "segment", sg.ID, "err", err)
			continue
		}
		n++
	}
	s.log.Info("checked minutes shown", "segments", n)
}

// recheckUser counts the user's pending segments again, after settling the
// account's prompt pulls (a pull past its deadline marks what never came
// as missing, which fails the project).
func (s *Server) recheckUser(ctx context.Context, userID int64) {
	if err := s.settlePulls(ctx, userID); err != nil {
		s.log.Error("settle pulls", "err", err)
	}
	segs, err := s.store.ListSegments(ctx, userID, 0)
	if err != nil {
		return
	}
	for i := range segs {
		if segs[i].Status != store.SegPending || segs[i].Stream {
			continue // one the server made is counted again at its cycle (records.go)
		}
		if err := s.recheckSegment(ctx, &segs[i]); err != nil {
			s.log.Error("recheck segment", "id", segs[i].ID, "err", err)
		}
	}
}

// recheckLoop counts pending segments again every few minutes, so a
// project turns verified (or failed) without anyone reading it.
func (s *Server) recheckLoop(ctx context.Context) {
	t := time.NewTicker(3 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		segs, err := s.store.PendingSegments(ctx, 200)
		if err != nil {
			continue
		}
		users := map[int64]bool{}
		for _, sg := range segs {
			users[sg.UserID] = true
		}
		for uid := range users {
			s.recheckUser(ctx, uid)
		}
	}
}
