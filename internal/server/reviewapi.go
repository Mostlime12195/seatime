package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/event"
)

// The review API: what Hack Club's review tool (Spyglass) reads to show a
// reviewer how a project's hours happened. It is a server-to-server call
// with a key from SEATIME_REVIEW_KEYS. The tool's own sign-in decides who
// may look (Hack Club Auth, a signed NDA, the program's reviewer list).

// reviewSegmentsMax is how many segments one answer carries.
const reviewSegmentsMax = 200

// reviewKey reports whether the request carries one of the review keys.
func (s *Server) reviewKey(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || got == "" {
		return false
	}
	match := 0
	for _, k := range s.cfg.ReviewKeys { // every key is compared, so the time taken says nothing
		match |= subtle.ConstantTimeCompare([]byte(got), []byte(k))
	}
	return match == 1
}

// reviewCaller answers a request that has no review key, and reports
// whether the request may go on.
func (s *Server) reviewCaller(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if len(s.cfg.ReviewKeys) == 0 {
		jsonErr(w, 404, "not found")
		return false
	}
	if !s.reviewKey(r) {
		jsonErr(w, 401, "unauthorized")
		return false
	}
	return true
}

// handleReviewUsers finds accounts by a part of a name or an ID.
func (s *Server) handleReviewUsers(w http.ResponseWriter, r *http.Request) {
	if !s.reviewCaller(w, r) {
		return
	}
	out := api.ReviewUsers{Users: []api.ReviewAccount{}}
	q := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("q")), "@")
	if len(q) > 100 {
		jsonErr(w, 400, "q is at most 100 characters")
		return
	}
	if len(q) >= 2 {
		us, err := s.store.SearchUsers(r.Context(), q, 20)
		if err != nil {
			s.log.Error("review users", "err", err)
			jsonErr(w, 500, "internal error")
			return
		}
		for _, u := range us {
			out.Users = append(out.Users, api.ReviewAccount{User: u.Username, Name: u.DisplayName, Avatar: u.Avatar, SlackID: u.SlackID, GitHub: u.GitHub})
		}
	}
	jsonOut(w, 200, out)
}

// handleReviewUser lists the projects of one account that its owner has
// not hidden.
func (s *Server) handleReviewUser(w http.ResponseWriter, r *http.Request) {
	if !s.reviewCaller(w, r) {
		return
	}
	ctx := r.Context()
	u, err := s.lookupUser(ctx, r.PathValue("user"))
	if err != nil {
		jsonErr(w, 404, "User not found")
		return
	}
	ps, err := s.store.ListProjects(ctx, u.ID)
	if err != nil {
		s.log.Error("review user", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	out := api.ReviewUser{User: u.Username, Name: u.DisplayName, Avatar: u.Avatar, SlackID: u.SlackID, GitHub: u.GitHub, Projects: []api.ReviewProject{}}
	for i := range ps {
		p := &ps[i]
		if p.Hidden {
			continue
		}
		st, err := s.projectStatus(ctx, u, p, time.Unix(0, 0), s.now().Add(24*time.Hour))
		if err != nil {
			s.log.Error("review user", "err", err)
			jsonErr(w, 500, "internal error")
			return
		}
		rp := api.ReviewProject{Key: p.Key, Slug: p.Slug, Name: p.Name, Repo: p.Repo,
			Status: st.Status, HumanSeconds: st.Verified, PendingSeconds: st.Pending}
		segs, err := s.store.ListSegments(ctx, u.ID, p.ID)
		if err != nil {
			s.log.Error("review user", "err", err)
			jsonErr(w, 500, "internal error")
			return
		}
		for _, sg := range segs {
			if rp.FirstTS.IsZero() || sg.FirstTS.Before(rp.FirstTS) {
				rp.FirstTS = sg.FirstTS
			}
			if sg.LastTS.After(rp.LastTS) {
				rp.LastTS = sg.LastTS
			}
		}
		out.Projects = append(out.Projects, rp)
	}
	s.log.Info("review read", "user", u.Username, "projects", len(out.Projects))
	jsonOut(w, 200, out)
}

func (s *Server) handleReviewEvidence(w http.ResponseWriter, r *http.Request) {
	if !s.reviewCaller(w, r) {
		return
	}
	ctx := r.Context()
	u, err := s.lookupUser(ctx, r.PathValue("user"))
	if err != nil {
		jsonErr(w, 404, "User not found")
		return
	}
	p, err := s.reviewProject(ctx, u, r.PathValue("project"))
	if err != nil {
		jsonErr(w, 404, "project not found")
		return
	}
	since, until := time.Unix(0, 0), s.now().Add(24*time.Hour)
	for k, dst := range map[string]*time.Time{"since": &since, "until": &until} {
		if v := r.URL.Query().Get(k); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				jsonErr(w, 400, k+" must be RFC3339")
				return
			}
			*dst = t
		}
	}
	out, err := s.reviewEvidence(ctx, u, p, since, until)
	if err != nil {
		s.log.Error("review evidence", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	s.log.Info("review read", "user", u.Username, "project", p.Key, "segments", len(out.Segments))
	jsonOut(w, 200, out)
}

// reviewProject finds a user's project by slug, key or name. A project
// its owner hid stays hidden from reviewers too.
func (s *Server) reviewProject(ctx context.Context, u *store.User, name string) (*store.Project, error) {
	p, err := s.store.ProjectBySlug(ctx, u.ID, name)
	if err != nil {
		ps, err2 := s.store.ListProjects(ctx, u.ID)
		if err2 != nil {
			return nil, err2
		}
		for i := range ps {
			if ps[i].Name == name || ps[i].Key == name {
				p, err = &ps[i], nil
				break
			}
		}
	}
	if err != nil || p.Hidden {
		return nil, store.ErrNotFound
	}
	return p, nil
}

func (s *Server) reviewEvidence(ctx context.Context, u *store.User, p *store.Project, since, until time.Time) (*api.ReviewEvidence, error) {
	ps, err := s.projectStatus(ctx, u, p, since, until)
	if err != nil {
		return nil, err
	}
	out := &api.ReviewEvidence{User: u.Username, SlackID: u.SlackID, Project: p.Key, Name: p.Name, Repo: p.Repo,
		Status: ps.Status, StatusReason: ps.Reason, AsOf: ps.AsOf,
		HumanSeconds: ps.Verified, DarkFlowSeconds: ps.Dark, PendingSeconds: ps.Pending,
		Commits: []api.Commit{}, Devices: []api.ReviewDevice{}, Segments: []api.ReviewSegment{}}
	ms, err := s.store.VerifiedModels(ctx, p.ID, since, until)
	if err != nil {
		return nil, err
	}
	out.Models = modelsJSON(ms, ps.Verified)
	cs, err := s.store.Commits(ctx, p.ID, since, until)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		out.Commits = append(out.Commits, api.Commit{SHA: c.SHA, TS: c.TS, Ins: c.Ins, Del: c.Del})
	}
	segs, err := s.store.ListSegments(ctx, u.ID, p.ID)
	if err != nil {
		return nil, err
	}
	devices, sessions := map[string]int{}, map[string]int{}
	for i := range segs {
		sg := &segs[i]
		if sg.LastTS.Before(since) || !sg.FirstTS.Before(until) {
			continue
		}
		if len(out.Segments) == reviewSegmentsMax {
			out.More = true
			break
		}
		n, seen := devices[sg.DeviceID]
		if !seen {
			n = len(devices) + 1
			devices[sg.DeviceID] = n
			rd := api.ReviewDevice{N: n}
			if d, err := s.store.DeviceByID(ctx, sg.DeviceID); err == nil {
				rd.OS, rd.Version = d.OS, d.ClientVersion
			}
			out.Devices = append(out.Devices, rd)
		}
		rs := api.ReviewSegment{ID: sg.ID, Device: n, Status: sg.Status, Problems: sg.Problems,
			FirstTS: sg.FirstTS, LastTS: sg.LastTS, Checked: sg.Checked, Minutes: [][3]float64{}, Records: []api.ReviewRecord{}}
		mins, err := s.store.SegmentMinutesOf(ctx, sg.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range mins {
			rs.Minutes = append(rs.Minutes, [3]float64{float64(m.StartMin * 60), m.Human, m.Dark})
		}
		b, err := s.segmentBundle(ctx, sg.ID)
		if err != nil {
			return nil, err
		}
		for _, en := range b.Entries {
			if en.Event == nil || (en.Event.Project != b.Project && !api.ProjectKeyMatches(en.Event.Project, b.Project)) {
				continue // a bare leaf, or another project's record
			}
			rs.Records = append(rs.Records, s.reviewRecord(ctx, sg, en.Event, en.Path, sessions))
		}
		out.Segments = append(out.Segments, rs)
	}
	return out, nil
}

// segmentBundle reads a stored segment's bundle back.
func (s *Server) segmentBundle(ctx context.Context, id string) (*api.Bundle, error) {
	raw, err := s.store.SegmentBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	var b api.Bundle
	if err := json.NewDecoder(io.LimitReader(zr, maxSegmentInflated)).Decode(&b); err != nil {
		return nil, err
	}
	return &b, nil
}

// reviewRecord copies the fields of a record that a reviewer may see. The
// copy is field by field on purpose: a field added to the record later
// stays out until someone decides that it belongs here.
func (s *Server) reviewRecord(ctx context.Context, sg *store.Segment, e *event.Event, path string, sessions map[string]int) api.ReviewRecord {
	r := api.ReviewRecord{TS: e.TS, Kind: e.Kind, Harness: e.Harness, Model: e.Model,
		Words: e.Words, New: e.New, QueuedMS: e.QueuedMS, InputBeforeMS: e.InputBeforeMS, TerminalIdleS: e.TerminalIdleS,
		FrontWindow: e.FrontWindow, SessionPrompts: e.SessionPrompts, OtherStopHook: e.OtherStopHook,
		Tool: e.Tool, Write: e.Write, ExitCode: e.ExitCode, Lines: e.Lines, LineNo: e.LineNo, CursorPos: e.CursorPos,
		LineAdditions: e.LineAdditions, LineDeletions: e.LineDeletions, Category: e.Category, File: path,
		SHA: e.SHA, Insertions: e.Insertions, Deletions: e.Deletions, FilesChanged: e.FilesChanged, RunKind: e.RunKind,
		Run: e.Run, Git: e.Git, PromptInLog: e.PromptInLog,
		Presence: e.Presence, IdleSrc: e.IdleSrc, WindowMS: e.WindowMS, NoInput: e.NoInput,
		InputSlices: e.InputSlices, FrontSlices: e.FrontSlices, Backfilled: e.Backfilled}
	if e.SessionID != "" {
		n, seen := sessions[e.SessionID]
		if !seen {
			n = len(sessions) + 1
			sessions[e.SessionID] = n
		}
		r.Session = n
	}
	if e.Kind == event.Prompt && e.CheckHash != "" {
		if v, err := s.store.PromptVerdict(ctx, sg.DeviceID, e.CheckHash); err == nil && v != nil {
			r.Check = &api.ReviewCheck{Pass: v.Pass, Kind: v.Kind, Minutes: v.Minutes, Labels: v.Labels, Skipped: v.Skipped, Rules: v.Rules, Model: v.Model}
		} else if sg.Status == store.SegPending {
			r.Check = &api.ReviewCheck{Waiting: true}
		}
	}
	return r
}
