package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/ledger"
)

// What applications read. Every number of hours is time the server counted
// from records that matched the device's hash chain (segments.go). Each
// project carries a status: verified, pending (some time is not checked
// yet) or failed (some records did not match; they do not count), and
// as_of, the time up to which its hours are checked.

// projStatus is one project's checked hours in a range and its status.
type projStatus struct {
	Status, Reason string
	// AsOf is the end of the last segment the server checked.
	AsOf time.Time
	// Verified and Dark are checked seconds in the range; Pending is synced
	// time in the range that no checked segment covers yet.
	Verified, Dark, Pending float64
	Minutes                 []store.SegmentMinute
}

// projectStatus works out a project's checked hours in [from, to) and its status.
func (s *Server) projectStatus(ctx context.Context, u *store.User, p *store.Project, from, to time.Time) (projStatus, error) {
	var ps projStatus
	segs, err := s.store.ListSegments(ctx, u.ID, p.ID)
	if err != nil {
		return ps, err
	}
	failed, pendingPrompts, pendingSegs := 0, 0, 0
	var firstProblem string
	for _, sg := range segs {
		switch sg.Status {
		case store.SegFailed:
			failed++
			if firstProblem == "" && len(sg.Problems) > 0 {
				firstProblem = sg.Problems[0]
			}
			continue
		case store.SegPending:
			pendingSegs++
			pendingPrompts += sg.Pending
		}
		if sg.LastTS.After(ps.AsOf) {
			ps.AsOf = sg.LastTS
		}
	}
	if ps.Minutes, err = s.store.VerifiedMinutes(ctx, p.ID, from, to); err != nil {
		return ps, err
	}
	for _, m := range ps.Minutes {
		ps.Verified += m.Human
		ps.Dark += m.Dark
	}
	// synced time after the last checked segment: not checked yet
	pf := from
	if ps.AsOf.After(pf) {
		pf = ps.AsOf.Add(time.Minute).Truncate(time.Minute)
	}
	if pf.Before(to) {
		bs, err := s.store.Buckets(ctx, u.ID, p.ID, pf, to)
		if err != nil {
			return ps, err
		}
		for _, b := range bs {
			ps.Pending += b.Seconds()
		}
	}
	closed, err := s.store.ClosedPromptCount(ctx, p.ID)
	if err != nil {
		return ps, err
	}
	switch {
	case failed > 0:
		ps.Status = api.StatusFailed
		ps.Reason = fmt.Sprintf("%d stretch%s of records did not match the device's hash chain, so %s not count", failed, map[bool]string{true: "", false: "es"}[failed == 1], map[bool]string{true: "it does", false: "they do"}[failed == 1])
		if firstProblem != "" {
			ps.Reason += ": " + firstProblem
		}
	case closed[store.ChainMissing] > 0:
		ps.Status = api.StatusFailed
		ps.Reason = fmt.Sprintf("%d prompt%s asked for the prompt check never came from the computer within %d days; they count as not passing",
			closed[store.ChainMissing], pluralS(closed[store.ChainMissing]), int(PullWait.Hours()/24))
	case closed[store.ChainMismatch] > 0:
		ps.Status = api.StatusFailed
		ps.Reason = fmt.Sprintf("%d prompt%s sent for the prompt check did not match the hash the device recorded; they count as not passing",
			closed[store.ChainMismatch], pluralS(closed[store.ChainMismatch]))
	case pendingSegs > 0 && s.pullsOpen(ctx, u.ID, s.recordKeys(ctx, p)...):
		ps.Status, ps.Reason = api.StatusPending, "the server asked the computer for prompts from stretches when the prompt check was off, and is checking them"
	case pendingSegs > 0:
		ps.Status, ps.Reason = api.StatusPending, fmt.Sprintf("%d prompt%s wait for the prompt check", pendingPrompts, pluralS(pendingPrompts))
	case ps.Pending >= 60:
		ps.Status = api.StatusPending
		ps.Reason = fmt.Sprintf("%s of recent work is not checked yet; the computer sends it once that stretch of work is over", fmtDur(ps.Pending))
	default:
		ps.Status = api.StatusVerified
	}
	return ps, nil
}

// appRead runs when an application (an OAuth token) reads a project. The
// first time, if the project has prompts that were never checked (sent
// while the account had the check off), the server asks the account's
// devices for them (ShipPull): the project is pending until they are
// checked, and failed if they never come.
func (s *Server) appRead(ctx context.Context, u *store.User, p *store.Project) {
	if !s.promptCheckOn() {
		return
	}
	pulled, err := s.store.ProjectPulled(ctx, p.ID)
	if err != nil || pulled {
		return
	}
	now := s.now()
	asked := 0
	for _, key := range s.recordKeys(ctx, p) { // a device's prompts carry the key it sent them under
		n, settled, err := s.store.PullResolve(ctx, store.PromptPull{UserID: u.ID, Project: key, From: time.UnixMilli(0).UTC(), To: now.Add(time.Minute)})
		if err != nil || n <= settled {
			continue
		}
		if _, err := ShipPull(ctx, s.store, u.ID, key, "application", now, 0); err != nil {
			s.log.Error("application read: pull", "err", err)
			return
		}
		asked += n - settled
	}
	if asked > 0 {
		s.log.Info("application read: asked for unchecked prompts", "user", u.Username, "project", p.Key, "prompts", asked)
		_ = s.store.MarkSegmentsPending(ctx, u.ID, p.ID)
	}
	_ = s.store.SetProjectPulled(ctx, p.ID)
}

// recheckSoon counts a user's pending segments again before their hours
// are read, at most every 30 seconds per user (the background loop does
// the rest).
func (s *Server) recheckSoon(ctx context.Context, userID int64) {
	now := s.now()
	if v, ok := s.rechecked.Load(userID); ok {
		if last := v.(time.Time); now.Sub(last) < 30*time.Second && !now.Before(last) {
			return
		}
	}
	s.rechecked.Store(userID, now)
	s.recheckUser(ctx, userID)
}

// statusJSON is the status fields every project entry carries.
func statusJSON(m map[string]any, st projStatus) map[string]any {
	m["status"] = st.Status
	if st.Reason != "" {
		m["status_reason"] = st.Reason
	}
	if st.AsOf.IsZero() {
		m["as_of"] = nil
	} else {
		m["as_of"] = isoZ(st.AsOf)
	}
	m["pending_seconds"] = int64(st.Pending)
	return m
}

// ---------------- /api/v1/users/{user}/projects/{project}/... ----------------

// readProject resolves the user and project of an application's request: a
// public profile, or the owner's own token (a device or an application they
// approved). Hidden projects are the owner's alone.
func (s *Server) readProject(w http.ResponseWriter, r *http.Request) (*store.User, *store.Project, bool) {
	u, self, ok := s.compatUser(w, r, strings.TrimPrefix(r.PathValue("user"), "@"))
	if !ok {
		return nil, nil, false
	}
	ctx := r.Context()
	p, err := s.store.ProjectBySlug(ctx, u.ID, r.PathValue("project"))
	if err != nil {
		if ps, err2 := s.store.ListProjects(ctx, u.ID); err2 == nil {
			for i := range ps {
				if ps[i].Name == r.PathValue("project") {
					p, err = &ps[i], nil
					break
				}
			}
		}
	}
	if err != nil || p.Hidden && !self {
		jsonErr(w, 404, "not found")
		return nil, nil, false
	}
	if s.appReader(r, u) {
		s.appRead(ctx, u, p)
	}
	s.recheckSoon(ctx, u.ID)
	return u, p, true
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	u, p, ok := s.readProject(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
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
	ps, err := s.projectStatus(ctx, u, p, since, until)
	if err != nil {
		jsonErr(w, 500, "internal error")
		return
	}
	st := api.ProjectStats{User: u.Username, Project: p.Key, Status: ps.Status, StatusReason: ps.Reason, AsOf: ps.AsOf,
		HumanSeconds: ps.Verified, StruggleSeconds: ps.Verified, DarkFlowSeconds: ps.Dark, PendingSeconds: ps.Pending}
	if b, _ := s.bearer(r); b != nil && b.ID == u.ID {
		ms, err := s.store.VerifiedModels(ctx, p.ID, since, until)
		if err != nil {
			jsonErr(w, 500, "internal error")
			return
		}
		st.Models = modelsJSON(ms, ps.Verified)
	}
	cs, _ := s.store.Commits(ctx, p.ID, since, until)
	st.Commits = len(cs)
	for _, c := range cs {
		st.CommitList = append(st.CommitList, api.Commit{SHA: c.SHA, TS: c.TS, Ins: c.Ins, Del: c.Del})
	}
	setReadHeaders(w, r)
	jsonOut(w, 200, st)
}

// GET /api/v1/users/{user}/projects/{project}/ysws and /ysws.txt
func (s *Server) handleYSWSExport(text bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := s.readProject(w, r)
		if !ok {
			return
		}
		x, err := s.projectExport(r.Context(), u, p)
		if err != nil {
			s.log.Error("ysws export", "project", p.ID, "err", err)
			jsonErr(w, 500, "internal error")
			return
		}
		setReadHeaders(w, r)
		if text {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte(x.Text))
			return
		}
		jsonOut(w, 200, x)
	}
}

// setReadHeaders lets shared caches keep public answers only.
func setReadHeaders(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		w.Header().Set("Cache-Control", "private, no-store")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=10")
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// repoOf returns "owner/repo" for a project, from the linked repo or a
// github.com/... project key.
func repoOf(p *store.Project) string {
	if p.Repo != "" {
		return p.Repo
	}
	if r := strings.TrimPrefix(p.Key, "github.com/"); r != p.Key && strings.Count(r, "/") == 1 {
		return r
	}
	return ""
}

// projectExport is the Unified YSWS export of a whole project.
func (s *Server) projectExport(ctx context.Context, u *store.User, p *store.Project) (*api.YSWSExport, error) {
	now := s.now()
	ps, err := s.projectStatus(ctx, u, p, time.Unix(0, 0), now.Add(time.Minute))
	if err != nil {
		return nil, err
	}
	base := s.cfg.BaseURL
	x := &api.YSWSExport{User: u.Username, Project: firstNonEmpty(p.Name, p.Key), Status: ps.Status, StatusReason: ps.Reason, AsOf: ps.AsOf,
		Hours: round1(ps.Verified / 3600), DarkFlowHours: round1(ps.Dark / 3600), PendingHours: round1(ps.Pending / 3600),
		ShareURL: base + "/@" + u.Username + "/" + p.Slug, ExportURL: base + "/api/v1/users/" + u.Username + "/projects/" + p.Slug + "/ysws"}
	if n := len(ps.Minutes); n > 0 {
		x.From, x.To = time.Unix(ps.Minutes[0].StartMin*60, 0).UTC(), time.Unix(ps.Minutes[n-1].StartMin*60+60, 0).UTC()
	}
	repo := repoOf(p)
	cs, err := s.store.Commits(ctx, p.ID, time.Unix(0, 0), now.Add(time.Hour))
	if err != nil {
		return nil, err
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].TS.Before(cs[j].TS) })
	if n := len(cs); n > 0 {
		x.Commits = n
		cr := &api.YSWSCommitRange{First: cs[0].SHA, Last: cs[n-1].SHA, Count: n, FirstTime: cs[0].TS, LastTime: cs[n-1].TS}
		if repo != "" {
			cr.CompareURL = "https://github.com/" + repo + "/compare/" + cr.First + "..." + cr.Last
		}
		x.CommitRange = cr
	}
	if repo != "" {
		x.CodeURL = "https://github.com/" + repo
	}
	x.OverrideHoursJustification = s.justification(x, ps)
	x.Text = exportText(x)
	return x, nil
}

func (s *Server) justification(x *api.YSWSExport, ps projStatus) string {
	var p []string
	p = append(p, fmt.Sprintf("%.1fh counted by SeaTime as Hackatime counts heartbeats", x.Hours))
	if x.DarkFlowHours >= 0.1 {
		p = append(p, fmt.Sprintf("%.1fh of AI time not counted (%s)", x.DarkFlowHours, darkFlowWhy(ledger.Version, s.pol().Rules)))
	}
	p = append(p, fmt.Sprintf("%d %s", x.Commits, plural(x.Commits, "commit", "commits")))
	switch x.Status {
	case api.StatusVerified:
		p = append(p, "every record matched the device's hash chain")
	case api.StatusPending:
		p = append(p, "PENDING ("+ps.Reason+")")
	default:
		p = append(p, "FAILED ("+ps.Reason+")")
	}
	p = append(p, "project: "+x.ShareURL)
	return strings.Join(p, "; ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func exportText(x *api.YSWSExport) string {
	var b strings.Builder
	state := map[string]string{api.StatusVerified: "verified", api.StatusPending: "PENDING, not final", api.StatusFailed: "FAILED, do not use"}[x.Status]
	fmt.Fprintf(&b, "Override Hours Spent: %.1f (SeaTime hours, %s)\n", x.Hours, state)
	fmt.Fprintf(&b, "Override Hours Spent Justification: %s\n", x.OverrideHoursJustification)
	if x.CodeURL != "" {
		fmt.Fprintf(&b, "Code URL: %s\n", x.CodeURL)
	}
	if cr := x.CommitRange; cr != nil {
		fmt.Fprintf(&b, "Commits: %s..%s (%d)", shortSHA(cr.First), shortSHA(cr.Last), cr.Count)
		if cr.CompareURL != "" {
			fmt.Fprintf(&b, " %s", cr.CompareURL)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "SeaTime project: %s\n", x.ShareURL)
	fmt.Fprintf(&b, "SeaTime JSON: %s\n", x.ExportURL)
	return b.String()
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
