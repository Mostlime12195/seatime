package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/ledger"
)

// ---------------- home / leaderboard ----------------

func (s *Server) globalStats(ctx context.Context) store.GlobalStats {
	v, err := s.cached("global", 3*time.Second, func(ctx context.Context) (any, error) { return s.store.Global(ctx, s.now()) })
	if err != nil {
		return store.GlobalStats{}
	}
	return v.(store.GlobalStats)
}

func periodStart(p string, now time.Time) (string, time.Time) {
	switch p {
	case "today":
		return "today", now.UTC().Truncate(24 * time.Hour)
	case "month":
		return "month", now.Add(-30 * 24 * time.Hour)
	}
	return "week", now.Add(-7 * 24 * time.Hour)
}

func (s *Server) leaders(ctx context.Context, period string, n int) []store.LeaderRow {
	p, from := periodStart(period, s.now())
	v, err := s.cached(fmt.Sprintf("lb:%s:%d", p, n), 20*time.Second, func(ctx context.Context) (any, error) {
		return s.store.Leaderboard(ctx, from.Truncate(time.Hour), n)
	})
	if err != nil {
		return nil
	}
	return v.([]store.LeaderRow)
}

// countTerms are the numbers the landing page states, from the rule in
// force: the agent's window after a heartbeat of yours (prompt check off),
// or, with the check on, the most minutes a passing prompt can get.
type countTerms struct {
	Gap     string // Hackatime's gap between two heartbeats
	Window  string
	Words   int
	CheckOn bool
	Most    string // the most minutes any prompt can get
	Idle    string // idle time that stops a prompt's clock
}

func (s *Server) countTerms() countTerms {
	c := countTerms{Gap: minNum(ledger.Timeout.Minutes()), Window: minNum(ledger.Window.Minutes()), Words: ledger.MinWords, Idle: minNum(ledger.IdleStop.Minutes())}
	p := s.pol()
	if !p.PromptCheck || p.Rules == nil {
		return c
	}
	c.CheckOn, c.Most = true, minNum(p.Rules.Longest().Minutes())
	return c
}

func minNum(m float64) string { return strconv.FormatFloat(m, 'f', -1, 64) }

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	ss, u := s.session(w, r)
	if u == nil {
		// signed out: what SeaTime is and how to start
		signIn := "/login"
		if s.hcaEnabled() {
			signIn = "/auth/hackclub"
		}
		s.pages.render(w, r, "landing", s.base(ss, nil, "SeaTime"), map[string]any{"SignIn": signIn, "Repo": sourceURL, "Count": s.countTerms(), "Deleted": r.URL.Query().Get("deleted") == "1"})
		return
	}
	data := map[string]any{"Global": s.globalStats(r.Context()), "Leaders": s.leaders(r.Context(), "week", 5)}
	if pv, err := s.profileView(r.Context(), u, u, pickRange("7d")); err == nil {
		if len(pv.Cards) > 3 {
			pv.Cards = pv.Cards[:3]
		}
		data["Me"] = pv
	}
	s.pages.render(w, r, "home", s.base(ss, u, "SeaTime"), data)
}

// rangeOpt is a span of days a stats page shows, like Hackatime's range
// picker. Days 0 is all time.
type rangeOpt struct {
	Key, Label string
	Days       int
}

var ranges = []rangeOpt{{"today", "Today", 1}, {"7d", "Last 7 days", 7}, {"30d", "Last 30 days", 30}, {"90d", "Last 90 days", 90}, {"year", "Last year", 365}, {"all", "All time", 0}}

func pickRange(key string) rangeOpt {
	for _, o := range ranges {
		if o.Key == key {
			return o
		}
	}
	return ranges[1]
}

// span is the range's first local day (days since epoch) and how many
// daily bars to draw for it.
func (o rangeOpt) span(today int64) (from int64, bars int) {
	n := o.Days
	if n == 0 {
		n = 365 * 50
	}
	return today - int64(n) + 1, min(max(o.Days, 7), 90)
}

func (s *Server) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	ss, u := s.session(w, r)
	period, _ := periodStart(r.URL.Query().Get("p"), s.now())
	rows := s.leaders(r.Context(), period, 50)
	max := 1.0
	if len(rows) > 0 {
		max = rows[0].Struggle
	}
	s.pages.render(w, r, "leaderboard", s.base(ss, u, "Leaderboard"), map[string]any{
		"Rows": rows, "Max": max, "Period": period})
}

func (s *Server) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if strings.HasPrefix(p, "/@") {
		parts := strings.Split(strings.Trim(p[2:], "/"), "/")
		switch len(parts) {
		case 1:
			s.handleProfile(w, r, parts[0])
			return
		case 2:
			s.handleProject(w, r, parts[0], parts[1])
			return
		}
	}
	s.notFound(w, r)
}

// ---------------- profile ----------------

type projCard struct {
	P            *store.Project
	Total, Range float64
	Pct          int // share of the range's counted time
	LastActive   time.Time
	Commits      int
	Active       bool
}

type profileView struct {
	Owner                 *store.User
	IsOwner               bool
	Range                 rangeOpt
	Ranges                []rangeOpt
	RangeTotal, DailyAvg  float64
	Today, Week, LastWeek store.DaySum
	AllStruggle           float64
	Streak, BestStreak    int
	BestDay               float64
	GoalPct               int
	WeekDelta             float64
	Heatmap, Bars         string
	Cards                 []projCard
	LiveURL               string
	Models                []modelRow       // owner only: the range's counted time and prompts by model
	NewProjects           *newProjectsView // owner only: projects still to be shown public or hidden
	// PendingMin is time waiting for the prompt check (whole minutes; 0
	// while the check is off or nothing waits).
	PendingMin int
	// Unchecked is counted time that the server has not checked yet, in
	// seconds. It is part of the totals.
	Unchecked float64
	// UncheckedPrompts are the prompts sent in that time. With the prompt
	// check on, their time is counted when the server checks them.
	UncheckedPrompts int
}

// visible: public profiles and your own.
func (s *Server) visible(u *store.User, viewer *store.User) bool {
	return u.Public || (viewer != nil && viewer.ID == u.ID)
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request, name string) {
	ss, viewer := s.session(w, r)
	ctx := r.Context()
	owner, err := s.store.UserByName(ctx, name)
	if err != nil || !s.visible(owner, viewer) {
		s.notFound(w, r)
		return
	}
	pv, err := s.profileView(ctx, owner, viewer, pickRange(r.URL.Query().Get("range")))
	if err != nil {
		s.log.Error("profile", "err", err)
		http.Error(w, "internal error", 500)
		return
	}
	b := s.base(ss, viewer, "@"+owner.Username)
	if r.URL.Query().Get("frag") == "1" {
		s.pages.fragment(w, "profile", "live", b, pv)
		return
	}
	if pv.IsOwner {
		pv.NewProjects = s.newProjects(ctx, owner, b.CSRF, "/@"+owner.Username)
	}
	s.pages.render(w, r, "profile", b, pv)
}

func sumDays(days map[int64]store.DaySum, from, to int64) store.DaySum {
	var t store.DaySum
	for d := from; d <= to; d++ {
		x := days[d]
		t.Struggle += x.Struggle
		t.DarkFlow += x.DarkFlow
		t.Human += x.Human
		t.Prompts += x.Prompts
		t.Commits += x.Commits
	}
	return t
}

func (s *Server) dayMap(ctx context.Context, uid, pid int64, from, to int64, off int) (map[int64]store.DaySum, error) {
	ds, err := s.store.DaySums(ctx, uid, pid, from, to, off)
	m := map[int64]store.DaySum{}
	for _, d := range ds {
		m[d.Day] = d
	}
	return m, err
}

const streakMin = 600.0 // 10 min of struggle keeps a streak

func streaks(days map[int64]store.DaySum, today int64, window int) (cur, best int) {
	d := today
	if days[d].Struggle < streakMin {
		d--
	}
	for days[d].Struggle >= streakMin {
		cur++
		d--
	}
	run := 0
	for d := today - int64(window) + 1; d <= today; d++ {
		if days[d].Struggle >= streakMin {
			run++
			if run > best {
				best = run
			}
		} else {
			run = 0
		}
	}
	return
}

func (s *Server) profileView(ctx context.Context, owner, viewer *store.User, rg rangeOpt) (*profileView, error) {
	now := s.now()
	off := owner.TZOffsetMin
	today := now.Add(time.Duration(off)*time.Minute).Unix() / 86400
	const window = 182
	from, nbars := rg.span(today)
	days, err := s.dayMap(ctx, owner.ID, 0, min(today-window+1, max(from, today-400)), today, off)
	if err != nil {
		return nil, err
	}
	pv := &profileView{Owner: owner, IsOwner: viewer != nil && viewer.ID == owner.ID, LiveURL: "/live/@" + owner.Username, Range: rg, Ranges: ranges}
	pv.Today = days[today]
	pv.Week = sumDays(days, today-6, today)
	pv.PendingMin = s.pendingMin(ctx, owner.ID, 0)
	pv.LastWeek = sumDays(days, today-13, today-7)
	pv.WeekDelta = pv.Week.Struggle - pv.LastWeek.Struggle
	pv.Streak, pv.BestStreak = streaks(days, today, window)
	for _, d := range days {
		if d.Struggle > pv.BestDay {
			pv.BestDay = d.Struggle
		}
	}
	pv.GoalPct = int(min(100, pv.Today.Struggle/3600*100))
	pv.Heatmap = heatmap(days, today, 26)
	pv.Bars = dailyBars(days, today, nbars)

	rangeFrom := time.Unix(from*86400-int64(off)*60, 0)
	if rg.Days == 0 {
		rangeFrom = time.Unix(0, 0)
	}
	all, err := s.store.ProjectSums(ctx, owner.ID, time.Unix(0, 0), now)
	if err != nil {
		return nil, err
	}
	inRange, _ := s.store.ProjectSums(ctx, owner.ID, rangeFrom, now)
	rangeBy := map[int64]float64{}
	active := 0
	for _, x := range inRange {
		rangeBy[x.Project.ID] = x.Struggle
		pv.RangeTotal += x.Struggle
	}
	for d := max(from, today-int64(len(days))*2); d <= today; d++ {
		if days[d].Struggle > 0 {
			active++
		}
	}
	if active > 0 {
		pv.DailyAvg = pv.RangeTotal / float64(active)
	}
	if pv.IsOwner {
		if ms, err := s.store.ModelSums(ctx, owner.ID, 0, rangeFrom, now); err == nil {
			pv.Models = modelRows(ms, pv.RangeTotal)
		}
	}
	pv.Unchecked, pv.UncheckedPrompts = s.unchecked(ctx, owner.ID, all, now)
	for _, x := range all {
		pv.AllStruggle += x.Struggle
		if x.Project.Hidden && !pv.IsOwner {
			continue
		}
		nc, _ := s.store.CommitCount(ctx, x.Project.ID, time.Unix(0, 0), now.Add(time.Hour))
		if x.Struggle <= 0 && nc == 0 {
			continue // no counted time and no commits: nothing to list
		}
		c := projCard{P: x.Project, Total: x.Struggle, Range: rangeBy[x.Project.ID],
			LastActive: x.LastStart, Commits: nc, Active: now.Sub(x.LastStart) < 10*time.Minute}
		if pv.RangeTotal > 0 {
			c.Pct = int(100*c.Range/pv.RangeTotal + 0.5)
		}
		pv.Cards = append(pv.Cards, c)
	}
	// most time in the range first, then the most recently active
	sort.SliceStable(pv.Cards, func(i, j int) bool {
		if pv.Cards[i].Range != pv.Cards[j].Range {
			return pv.Cards[i].Range > pv.Cards[j].Range
		}
		return pv.Cards[i].LastActive.After(pv.Cards[j].LastActive)
	})
	return pv, nil
}

// ---------------- project ----------------

type projectView struct {
	Owner                *store.User
	P                    *store.Project
	IsOwner              bool
	Range                rangeOpt
	Ranges               []rangeOpt
	RangeTotal, DailyAvg float64
	RangeCommits         int
	Split                timeSplit // the range's counted time, by hand and with AI tools
	Tools                []toolRow
	Models               []modelRow // owner only: the range's counted time and prompts by model
	Day                  string
	PrevDay              string
	NextDay              string
	IsToday              bool
	DayTotals            store.DaySum
	AllStruggle          float64
	Timeline             string
	Bars                 string
	Commits              []commitRow
	DayCommits           int
	// Check is what applications read: the hours checked against the hash
	// chain, and the project's status (apps.go).
	Check      projStatus
	RepoURL    string
	LiveURL    string
	PendingMin int // time waiting for the prompt check, whole minutes
}

type commitRow struct {
	store.Commit
	URL string
}

func commitURL(repo, sha string) string {
	if repo == "" {
		return ""
	}
	return "https://github.com/" + repo + "/commit/" + sha
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request, name, slug string) {
	ss, viewer := s.session(w, r)
	ctx := r.Context()
	owner, err := s.store.UserByName(ctx, name)
	if err != nil || !s.visible(owner, viewer) {
		s.notFound(w, r)
		return
	}
	p, err := s.store.ProjectBySlug(ctx, owner.ID, slug)
	isOwner := viewer != nil && viewer.ID == owner.ID
	if err != nil || (p.Hidden && !isOwner) {
		s.notFound(w, r)
		return
	}
	now := s.now()
	off := owner.TZOffsetMin
	today := now.Add(time.Duration(off)*time.Minute).Unix() / 86400
	day := today
	if q := r.URL.Query().Get("day"); q != "" {
		if t, err := time.Parse("2006-01-02", q); err == nil {
			day = t.Unix() / 86400
		}
	} else if sums, _ := s.store.ProjectSums(ctx, owner.ID, time.Unix(0, 0), now); len(sums) > 0 {
		for _, x := range sums { // default to this project's most recent active day
			if x.Project.ID == p.ID {
				day = x.LastStart.Add(time.Duration(off)*time.Minute).Unix() / 86400
				break
			}
		}
	}
	dayStart := time.Unix(day*86400-int64(off)*60, 0).UTC()
	dayEnd := dayStart.Add(24 * time.Hour)
	fmtDay := func(d int64) string { return time.Unix(d*86400, 0).UTC().Format("2006-01-02") }

	rg := pickRange(r.URL.Query().Get("range"))
	from, nbars := rg.span(today)
	days, err := s.dayMap(ctx, owner.ID, p.ID, max(from, today-400), today, off)
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	bs, _ := s.store.Buckets(ctx, owner.ID, p.ID, dayStart, dayEnd)
	cs, _ := s.store.Commits(ctx, p.ID, dayStart, dayEnd)
	var marks []commitMark
	for _, c := range cs {
		marks = append(marks, commitMark{At: c.TS, SHA: c.SHA, URL: commitURL(p.Repo, c.SHA)})
	}
	pv := &projectView{Owner: owner, P: p, IsOwner: isOwner, Day: fmtDay(day), PrevDay: fmtDay(day - 1), IsToday: day == today,
		Timeline: dayTimeline(bs, marks, dayStart), Bars: dailyBars(days, today, nbars), DayCommits: len(cs),
		LiveURL: "/live/@" + owner.Username, Range: rg, Ranges: ranges}
	pv.PendingMin = s.pendingMin(ctx, owner.ID, p.ID)
	if day < today {
		pv.NextDay = fmtDay(day + 1)
	}
	for _, b := range bs {
		pv.DayTotals.Struggle += b.Seconds()
	}
	rangeFrom := time.Unix(from*86400-int64(off)*60, 0).UTC()
	if all, err := s.store.ProjectSums(ctx, owner.ID, time.Unix(0, 0), now); err == nil {
		for _, x := range all {
			if x.Project.ID == p.ID {
				pv.AllStruggle = x.Struggle
				if rg.Days == 0 && !x.LastStart.IsZero() {
					rangeFrom = time.Unix(0, 0).UTC()
				}
			}
		}
	}
	// counted time in the range, by tool (at most a year of minutes)
	toolFrom := rangeFrom
	if lim := now.Add(-366 * 24 * time.Hour); toolFrom.Before(lim) {
		toolFrom = lim
	}
	rbs, _ := s.store.Buckets(ctx, owner.ID, p.ID, toolFrom, now.Add(time.Minute))
	pv.Split, pv.Tools = byTool(rbs)
	if sums, err := s.store.ProjectSums(ctx, owner.ID, rangeFrom, now); err == nil {
		for _, x := range sums {
			if x.Project.ID == p.ID {
				pv.RangeTotal = x.Struggle
			}
		}
	}
	if isOwner {
		if ms, err := s.store.ModelSums(ctx, owner.ID, p.ID, rangeFrom, now); err == nil {
			pv.Models = modelRows(ms, pv.RangeTotal)
		}
	}
	active := 0
	for d, x := range days {
		if d >= from && x.Struggle > 0 {
			active++
		}
	}
	if active > 0 {
		pv.DailyAvg = pv.RangeTotal / float64(active)
	}
	pv.RangeCommits, _ = s.store.CommitCount(ctx, p.ID, rangeFrom, now.Add(time.Hour))
	recent, _ := s.store.Commits(ctx, p.ID, now.Add(-30*24*time.Hour), now.Add(time.Hour))
	for i := len(recent) - 1; i >= 0 && len(pv.Commits) < 15; i-- {
		pv.Commits = append(pv.Commits, commitRow{recent[i], commitURL(p.Repo, recent[i].SHA)})
	}
	s.recheckSoon(ctx, owner.ID)
	pv.Check, _ = s.projectStatus(ctx, owner, p, time.Unix(0, 0), now.Add(time.Minute))
	if p.Repo != "" {
		pv.RepoURL = "https://github.com/" + p.Repo
	}
	b := s.base(ss, viewer, p.Name+" by @"+owner.Username)
	if r.URL.Query().Get("frag") == "1" {
		s.pages.fragment(w, "project", "live", b, pv)
		return
	}
	s.pages.render(w, r, "project", b, pv)
}

// ---------------- settings ----------------

type settingsView struct {
	Projects    []store.Project
	Devices     []store.Device
	GH, HCA     bool
	Flash       string
	NewProjects *newProjectsView
	// the privacy lines for lookups that are on (serverfacts.go); each shows only when its switch is on
	TrustLookup bool
	// PromptCheck is the account's own prompt check setting; ServerCheck
	// the server's switch (promptchain.go)
	PromptCheck, ServerCheck bool
	// Computers are the account's devices with their health (devices.go);
	// AlsoHackatime and AutoUpdate the settings every computer applies.
	Computers                 []deviceRow
	AlsoHackatime, AutoUpdate bool
	// Apps are the applications the user allowed to read their hours (oauth.go).
	Apps []store.OAuthGrant
}

// newProjectsView is the hide switch shown once a project first syncs: each
// project the owner has not yet made public or hidden, with both choices.
type newProjectsView struct {
	CSRF, Next string
	Projects   []store.Project
}

func (s *Server) newProjects(ctx context.Context, u *store.User, csrf, next string) *newProjectsView {
	ps, err := s.listedProjects(ctx, u.ID)
	if err != nil {
		return nil
	}
	v := &newProjectsView{CSRF: csrf, Next: next}
	for _, p := range ps {
		if !p.Decided {
			v.Projects = append(v.Projects, p)
		}
	}
	if len(v.Projects) == 0 {
		return nil
	}
	return v
}

// listedProjects are the user's projects that have something to show:
// counted time, checked time or a commit. A project with none of them (a
// folder an AI tool only passed through, before sessions kept to one
// project) is not listed on the profile, in Settings or to applications;
// its page still answers.
func (s *Server) listedProjects(ctx context.Context, userID int64) ([]store.Project, error) {
	ps, err := s.store.ListProjects(ctx, userID)
	if err != nil {
		return nil, err
	}
	empty, err := s.store.EmptyProjects(ctx, userID)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(ps, func(p store.Project) bool { return empty[p.ID] }), nil
}

// handleProjectVisibility is the hide switch: hidden=1 hides the project,
// hidden=0 keeps it public; either way the owner has decided. A script
// asking for JSON gets {"id","hidden","name"} and the page does not reload;
// a plain form post goes back to next.
func (s *Server) handleProjectVisibility(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.postGuard(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	p, err := s.store.ProjectByID(r.Context(), id)
	if err != nil || p.UserID != u.ID {
		http.NotFound(w, r)
		return
	}
	hidden := r.PostFormValue("hidden") == "1"
	if err := s.store.SetProjectHidden(r.Context(), u.ID, p.ID, hidden); err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": p.ID, "hidden": hidden, "name": p.Name})
		return
	}
	next := r.PostFormValue("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		next = "/settings"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) requireLogin(w http.ResponseWriter, r *http.Request) (*session, *store.User, bool) {
	ss, u := s.session(w, r)
	if u == nil {
		next := r.URL.RequestURI()
		http.Redirect(w, r, "/login?next="+urlEscape(next), http.StatusSeeOther)
		return ss, nil, false
	}
	return ss, u, true
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.~/", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	ss, u, ok := s.requireLogin(w, r)
	if !ok {
		return
	}
	ps, _ := s.listedProjects(r.Context(), u.ID)
	ds, _ := s.store.ListDevices(r.Context(), u.ID)
	b := s.base(ss, u, "Settings")
	acct := s.accountSettings(r.Context(), u.ID)
	grants, _ := s.store.UserOAuthGrants(r.Context(), u.ID)
	s.pages.render(w, r, "settings", b, settingsView{Projects: ps, Devices: ds,
		Computers:     s.deviceRows(r.Context(), u.ID, ds),
		Apps:          grants,
		AlsoHackatime: s.accountSettingOn(acct, api.SettingAlsoHackatime), AutoUpdate: s.accountSettingOn(acct, api.SettingAutoUpdate),
		GH: s.ghEnabled(), HCA: s.hcaEnabled(), Flash: r.URL.Query().Get("ok"),
		NewProjects: s.newProjects(r.Context(), u, b.CSRF, "/settings"),
		TrustLookup: s.cfg.HackatimeTrust,
		PromptCheck: s.userCheckOn(r.Context(), u.ID), ServerCheck: s.promptCheckOn()})
}

func (s *Server) postGuard(w http.ResponseWriter, r *http.Request) (*session, *store.User, bool) {
	ss, u, ok := s.requireLogin(w, r)
	if !ok {
		return nil, nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if !s.csrfOK(r, ss) {
		http.Error(w, "bad csrf token (reload the page and try again)", 403)
		return nil, nil, false
	}
	return ss, u, true
}

func (s *Server) handleSettingsProfile(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.postGuard(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("display_name"))
	if name == "" || len([]rune(name)) > 60 {
		name = u.Username
	}
	u.DisplayName = name
	u.Public = r.PostFormValue("public") == "1"
	if h, err := strconv.ParseFloat(r.PostFormValue("tz"), 64); err == nil && h >= -12 && h <= 14 {
		u.TZOffsetMin = int(h) * 60
	}
	if err := s.store.UpdateUser(r.Context(), u); err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	http.Redirect(w, r, "/settings?ok=Your+profile+is+saved.", http.StatusSeeOther)
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func normRepo(in string) (string, bool) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", true
	}
	if repoRe.MatchString(in) {
		in = "github.com/" + in
	}
	r := store.RepoFromRemote(in)
	return r, r != ""
}

func (s *Server) handleSettingsProject(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.postGuard(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	p, err := s.store.ProjectByID(r.Context(), id)
	if err != nil || p.UserID != u.ID {
		http.NotFound(w, r)
		return
	}
	repo, ok := normRepo(r.PostFormValue("repo"))
	if !ok {
		http.Error(w, "repo must look like owner/name or a github.com URL", 400)
		return
	}
	if name := strings.TrimSpace(r.PostFormValue("name")); name != "" && name != p.Name {
		if len([]rune(name)) > 80 {
			http.Error(w, "the name is longer than 80 characters", 400)
			return
		}
		if err := s.store.SetProjectName(r.Context(), u.ID, p.ID, name); err != nil {
			http.Error(w, "internal error", 500)
			return
		}
	}
	s.store.SetProjectHidden(r.Context(), u.ID, p.ID, r.PostFormValue("hidden") == "1")
	if repo != p.Repo {
		s.store.SetProjectRepo(r.Context(), u.ID, p.ID, repo)
	}
	http.Redirect(w, r, "/settings?ok=The+project+is+saved.", http.StatusSeeOther)
}

func (s *Server) handleSettingsDevice(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.postGuard(w, r)
	if !ok {
		return
	}
	if err := s.store.RevokeDevice(r.Context(), u.ID, r.PostFormValue("id")); err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/settings?ok=The+device+is+revoked.", http.StatusSeeOther)
}

// ---------------- device approval page ----------------

type deviceView struct {
	Code   string
	Device *store.DeviceCode
	Err    string
	Done   bool
}

func (s *Server) handleDevicePage(w http.ResponseWriter, r *http.Request) {
	ss, u, ok := s.requireLogin(w, r)
	if !ok {
		return
	}
	v := deviceView{Code: r.URL.Query().Get("code")}
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		if !s.csrfOK(r, ss) {
			http.Error(w, "bad csrf token", 403)
			return
		}
		v.Code = r.PostFormValue("code")
	}
	if v.Code != "" {
		if !s.deviceCodeAttempt(r, u) {
			v.Err = "Too many attempts. Wait a few seconds."
		} else if dc, err := s.store.DeviceCodeByUserCode(r.Context(), normUserCode(v.Code)); err != nil || dc.UserID != 0 {
			v.Err = "That code is not valid or has expired. Start the sign-in on your computer again to get a new one."
		} else {
			v.Device = dc
		}
	}
	s.pages.render(w, r, "device", s.base(ss, u, "Connect a device"), v)
}

func (s *Server) handleDeviceApprove(w http.ResponseWriter, r *http.Request) {
	ss, u, ok := s.requireLogin(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if !s.csrfOK(r, ss) {
		http.Error(w, "bad csrf token", 403)
		return
	}
	v := deviceView{}
	devs, _ := s.store.ListDevices(r.Context(), u.ID)
	live := 0
	for _, d := range devs {
		if !d.Revoked {
			live++
		}
	}
	if !s.deviceCodeAttempt(r, u) {
		v.Err = "Too many attempts. Wait a few seconds."
	} else if limit := s.pol().MaxDevices; live >= limit && !s.reconnectsLive(r.Context(), u, r.PostFormValue("code")) {
		v.Err = fmt.Sprintf("This account already has %d connected devices, the most it may have. Revoke one in Settings, then try again.", live)
	} else if err := s.store.ApproveDeviceCode(r.Context(), normUserCode(r.PostFormValue("code")), u.ID); err != nil {
		v.Err = "That code is not valid or has expired."
	} else {
		v.Done = true
	}
	s.pages.render(w, r, "device", s.base(ss, u, "Connect a device"), v)
}

// reconnectsLive reports whether the code reconnects one of u's devices
// that is not revoked: that adds no device, so the limit does not apply.
func (s *Server) reconnectsLive(ctx context.Context, u *store.User, code string) bool {
	dc, err := s.store.DeviceCodeByUserCode(ctx, normUserCode(code))
	if err != nil || dc.PrevDevice == "" {
		return false
	}
	d, ok := s.ownDevice(ctx, u, dc.PrevDevice)
	return ok && !d.Revoked
}

// deviceCodeAttempt rate-limits user-code guesses: confirming (POST /device)
// and approving (POST /device/approve) draw on the same per-user and per-IP
// buckets, so neither step is an unlimited oracle. A real login uses two.
func (s *Server) deviceCodeAttempt(r *http.Request, u *store.User) bool {
	return s.lim.Allow(fmt.Sprintf("devcode:%d", u.ID), 0.2, 8) && s.allowIP(r, "devcode-ip", 0.5, 30)
}

// toolRow is counted time with one tool: an editor or an AI tool.
type toolRow struct {
	Name    string
	Hand    bool
	Seconds float64
	Pct     int
}

// handTool reports whether a harness name is time by hand: an editor's
// heartbeats or a run in the person's own terminal.
func handTool(h string) bool {
	return strings.HasPrefix(h, "editor:") || h == "terminal" || h == "shell" || h == "other"
}

// byTool splits counted minutes among the tools seen in each minute, evenly
// when a minute had several. A minute names its tools, not how many seconds
// each earned, so the split is close, not exact.
func byTool(bs []ledger.Bucket) (timeSplit, []toolRow) {
	sec := map[string]float64{}
	var t timeSplit
	for _, b := range bs {
		if b.Seconds() <= 0 {
			continue
		}
		var hs []string
		for _, h := range b.Harnesses {
			if h != "" && h != "git" && !slices.Contains(hs, h) {
				hs = append(hs, h)
			}
		}
		if len(hs) == 0 {
			hs = []string{"other"}
		}
		share := b.Seconds() / float64(len(hs))
		for _, h := range hs {
			sec[h] += share
			if handTool(h) {
				t.Hand += share
			} else {
				t.AI += share
			}
		}
	}
	t.Counted = t.Hand + t.AI
	var out []toolRow
	for h, v := range sec {
		r := toolRow{Name: strings.TrimPrefix(h, "editor:"), Hand: handTool(h), Seconds: v}
		if t.Counted > 0 {
			r.Pct = int(100*v/t.Counted + 0.5)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seconds > out[j].Seconds })
	return t, out
}

// unchecked is a user's counted time, in seconds, after the last segment
// that the server checked in each project, and the prompts sent in it
// (projectStatus's Pending and PendingPrompts, over all of the projects in
// sums).
func (s *Server) unchecked(ctx context.Context, userID int64, sums []store.ProjectSum, now time.Time) (sec float64, prompts int) {
	upTo, err := s.store.CheckedUpTo(ctx, userID)
	if err != nil {
		return 0, 0
	}
	for _, x := range sums {
		from := time.Unix(0, 0)
		if t, ok := upTo[x.Project.ID]; ok {
			from = t.Add(time.Minute).Truncate(time.Minute)
		}
		if !x.LastStart.Add(time.Hour).After(from) { // LastStart is the hour of the last minute with anything in it
			continue
		}
		bs, err := s.store.SyncedBuckets(ctx, userID, x.Project.ID, from, now)
		if err != nil {
			continue
		}
		for _, b := range bs {
			sec += b.Seconds()
			prompts += b.Prompts
		}
	}
	return sec, prompts
}

// uncheckedLabel says what is not checked yet: "12m", "3 prompts" or
// "12m + 3 prompts"; "" when there is under a minute and no prompt.
func uncheckedLabel(sec float64, prompts int) string {
	var parts []string
	if sec >= 60 {
		parts = append(parts, fmtDur(sec))
	}
	if prompts == 1 {
		parts = append(parts, "1 prompt")
	} else if prompts > 1 {
		parts = append(parts, strconv.Itoa(prompts)+" prompts")
	}
	return strings.Join(parts, " + ")
}

// pendingMin is a user's time waiting for the prompt check, as their
// devices reported it in the last day, in whole minutes (0 while the check
// is off).
func (s *Server) pendingMin(ctx context.Context, userID, projectID int64) int {
	if !s.pol().PromptCheck {
		return 0
	}
	sec, err := s.store.Pending(ctx, userID, projectID, s.now().Add(-24*time.Hour))
	if err != nil {
		return 0
	}
	return int(sec/60 + 0.5)
}
