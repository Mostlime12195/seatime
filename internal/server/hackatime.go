package server

// Hackatime-compatible read API. A YSWS program that already calls
// hackatime.hackclub.com can point its base URL at seatimed. total_seconds (and
// every "hours" derived from it) is seatime's STRUGGLE seconds; presence seconds
// (comparable to Hackatime's number) ride along as presence_seconds. Pass
// ?metric=presence to make total_seconds the presence number instead.
// See YSWS.md for the exact compatibility table.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // IANA zones for ?timezone= even on minimal hosts

	"seatime/internal/server/store"
	"seatime/pkg/api"
)

var slackIDRe = regexp.MustCompile(`^[UW][A-Z0-9]{6,}$`)

// lookupUser resolves a Hackatime-style identifier: Slack member ID, Hack Club
// Auth id ("ident!..."), or SeaTime username (optionally "@name").
func (s *Server) lookupUser(ctx context.Context, ident string) (*store.User, error) {
	ident = strings.TrimPrefix(strings.TrimSpace(ident), "@")
	if ident == "" || len(ident) > 100 {
		return nil, store.ErrNotFound
	}
	if slackIDRe.MatchString(ident) {
		if u, err := s.store.UserBySlackID(ctx, ident); err == nil {
			return u, nil
		}
	}
	if strings.HasPrefix(ident, "ident!") {
		if u, err := s.store.UserByIdentity(ctx, "hackclub", ident); err == nil {
			return u, nil
		}
	}
	if u, err := s.store.UserByName(ctx, ident); err == nil {
		return u, nil
	}
	if u, err := s.store.UserByIdentity(ctx, "hackclub", ident); err == nil {
		return u, nil
	}
	return nil, store.ErrNotFound
}

func compatErr(w http.ResponseWriter, code int, msg string) { jsonErr(w, code, msg) }

// bearer returns the user behind the request's bearer token, or nil: a
// SeaTime device token (app false) or an application's OAuth access token
// (app true, oauth.go). It never writes a response.
func (s *Server) bearer(r *http.Request) (u *store.User, app bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return nil, false
	}
	if strings.HasPrefix(tok, accessPrefix) {
		return s.oauthUser(r.Context(), tok), true
	}
	dev, err := s.store.DeviceByTokenHash(r.Context(), hashHex(tok))
	if err != nil || dev.Revoked {
		return nil, false
	}
	u, err = s.store.UserByID(r.Context(), dev.UserID)
	if err != nil {
		return nil, false
	}
	return u, false
}

// appReader reports whether the request is an application's token for u.
func (s *Server) appReader(r *http.Request, u *store.User) bool {
	b, app := s.bearer(r)
	return app && b != nil && b.ID == u.ID
}

// compatUser resolves {user}/{id}; "current" and "my" mean the caller's
// token. A private profile is readable only with its owner's token: a
// device's, or an application's the owner approved (403 otherwise, as Hackatime
// does). self is the owner's own device token: only that sees hidden
// projects.
func (s *Server) compatUser(w http.ResponseWriter, r *http.Request, ident string) (u *store.User, self bool, ok bool) {
	b, app := s.bearer(r)
	if ident == "current" || ident == "my" {
		if b == nil {
			compatErr(w, 401, "current user needs 'Authorization: Bearer <token>' (an application's OAuth token or a SeaTime device token)")
			return nil, false, false
		}
		return b, !app, true
	}
	u, err := s.lookupUser(r.Context(), ident)
	if err != nil {
		compatErr(w, 404, "User not found")
		return nil, false, false
	}
	mine := b != nil && b.ID == u.ID
	if !u.Public && !mine {
		compatErr(w, 403, "user has disabled public stats")
		return nil, false, false
	}
	return u, mine && !app, true
}

// ---------------- time parsing & formatting ----------------

func parseFlexTime(v string, loc *time.Location) (time.Time, bool) {
	for _, l := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(l, v, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func endOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1).Add(-time.Second)
}
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func isoZ(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func detailedText(sec float64) string {
	n := int(sec)
	h, m, s := n/3600, n%3600/60, n%60
	var p []string
	if h > 0 {
		p = append(p, fmt.Sprintf("%dh", h))
	}
	if m > 0 {
		p = append(p, fmt.Sprintf("%dm", m))
	}
	if s > 0 {
		p = append(p, fmt.Sprintf("%ds", s))
	}
	if len(p) == 0 {
		return "0s"
	}
	return strings.Join(p, " ")
}

func digital(sec float64) string {
	n := int(sec)
	return fmt.Sprintf("%02d:%02d:%02d", n/3600, n%3600/60, n%60)
}

func duration(sec float64) map[string]any {
	n := int64(sec)
	return map[string]any{"total_seconds": n, "hours": n / 3600, "minutes": n % 3600 / 60, "seconds": n % 60,
		"digital": digital(sec), "decimal": fmt.Sprintf("%.2f", float64(n)/3600), "text": detailedText(sec)}
}

func csvSet(v string) map[string]bool {
	var m map[string]bool
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			if m == nil {
				m = map[string]bool{}
			}
			m[p] = true
		}
	}
	return m
}

func userLoc(u *store.User) *time.Location {
	return time.FixedZone(tzName(u.TZOffsetMin), u.TZOffsetMin*60)
}

func tzName(offMin int) string {
	if offMin == 0 || offMin%60 != 0 {
		return "UTC"
	}
	if offMin > 0 {
		return fmt.Sprintf("Etc/GMT-%d", offMin/60)
	}
	return fmt.Sprintf("Etc/GMT+%d", -offMin/60)
}

// ---------------- aggregation ----------------

type projAgg struct {
	Name                     string
	Struggle, Presence, Dark float64
	Minutes                  int
	First, Last              time.Time
	Repo, Remote             string
	St                       projStatus
	days                     map[string]*dayAgg // filled when a location is given
}

type dayAgg struct{ Struggle, Presence, Dark float64 }

func (p *projAgg) metric(presence bool) float64 {
	if presence {
		return p.Presence
	}
	return p.Struggle
}

// aggregate sums a user's checked minutes in [from,to) per project name
// (projects sharing a name merge, as they do on Hackatime), with each
// project's status (apps.go). A project with synced time that is not
// checked yet shows with its pending time. Hidden projects are skipped
// unless the caller is the owner. filter matches name, key or slug. An
// application's token for the user also asks for unchecked prompts
// (appRead).
func (s *Server) aggregate(ctx context.Context, u *store.User, from, to time.Time, filter map[string]bool, self bool, loc *time.Location, app bool) ([]*projAgg, error) {
	ps, err := s.store.ListProjects(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	s.recheckSoon(ctx, u.ID)
	byName := map[string]*projAgg{}
	var order []*projAgg
	for i := range ps {
		p := &ps[i]
		if p.Hidden && !self {
			continue
		}
		name := firstNonEmpty(p.Name, p.Key)
		if filter != nil && !filter[name] && !filter[p.Key] && !filter[p.Slug] {
			continue
		}
		if app {
			s.appRead(ctx, u, p)
		}
		st, err := s.projectStatus(ctx, u, p, from, to)
		if err != nil {
			return nil, err
		}
		if len(st.Minutes) == 0 && st.Pending == 0 {
			continue
		}
		a := byName[name]
		if a == nil {
			a = &projAgg{Name: name, Repo: p.Repo, Remote: p.Remote, St: st}
			if loc != nil {
				a.days = map[string]*dayAgg{}
			}
			byName[name] = a
			order = append(order, a)
		} else {
			a.St = mergeStatus(a.St, st)
		}
		for _, m := range st.Minutes {
			start := time.Unix(m.StartMin*60, 0).UTC()
			a.Struggle += m.Human
			a.Presence += m.Human
			a.Dark += m.Dark
			a.Minutes++
			if a.First.IsZero() || start.Before(a.First) {
				a.First = start
			}
			if e := start.Add(time.Minute); e.After(a.Last) {
				a.Last = e
			}
			if loc != nil {
				k := start.In(loc).Format("2006-01-02")
				d := a.days[k]
				if d == nil {
					d = &dayAgg{}
					a.days[k] = d
				}
				d.Struggle += m.Human
				d.Presence += m.Human
				d.Dark += m.Dark
			}
		}
	}
	return order, nil
}

// mergeStatus is the status of two projects shown under one name: the
// worse of the two, the earlier as_of, the pending time added.
func mergeStatus(a, b projStatus) projStatus {
	rank := map[string]int{api.StatusVerified: 0, api.StatusPending: 1, api.StatusFailed: 2}
	out := a
	if rank[b.Status] > rank[a.Status] {
		out.Status, out.Reason = b.Status, b.Reason
	}
	if !b.AsOf.IsZero() && (out.AsOf.IsZero() || b.AsOf.Before(out.AsOf)) {
		out.AsOf = b.AsOf
	}
	out.Pending += b.Pending
	return out
}

func sortAggs(as []*projAgg, presence bool) {
	sort.SliceStable(as, func(i, j int) bool {
		if a, b := as[i].metric(presence), as[j].metric(presence); a != b {
			return a > b
		}
		return as[i].Name < as[j].Name
	})
}

func sumAggs(as []*projAgg) (struggle, presence, dark float64) {
	for _, a := range as {
		struggle += a.Struggle
		presence += a.Presence
		dark += a.Dark
	}
	return
}

func wantPresence(r *http.Request) bool { return r.URL.Query().Get("metric") == "presence" }

func setCompatHeaders(w http.ResponseWriter, r *http.Request, presence bool) {
	m := "struggle_seconds"
	if presence {
		m = "presence_seconds"
	}
	w.Header().Set("X-Seatime-Metric", m)
	setReadHeaders(w, r)
}

// ---------------- /api/v1/users/{a}/{b} ----------------

func (s *Server) handleCompatUser2(w http.ResponseWriter, r *http.Request) {
	a, b := r.PathValue("a"), r.PathValue("b")
	switch a {
	case "lookup_slack_uid":
		u, err := s.store.UserBySlackID(r.Context(), b)
		if err != nil || !u.Public {
			jsonOut(w, 404, map[string]any{"error": "User not found", "slack_uid": b})
			return
		}
		jsonOut(w, 200, map[string]any{"user_id": u.Username, "slack_uid": b})
		return
	case "lookup_email":
		jsonOut(w, 404, map[string]any{"error": "seatime does not store email addresses; look users up by Slack ID, Hack Club Auth id or username", "email": b})
		return
	}
	u, self, ok := s.compatUser(w, r, a)
	if !ok {
		return
	}
	switch b {
	case "stats":
		s.compatStats(w, r, u, self)
	case "projects":
		s.compatProjectNames(w, r, u, self)
	case "trust_factor":
		// Hackatime's colour for this account when SEATIME_HACKATIME_TRUST=1, else "blue" (trust.go)
		jsonOut(w, 200, trustJSON(s.trustFor(r.Context(), u)))
	default:
		compatErr(w, 404, "not found")
	}
}

// statsRange parses start_date/end_date (default: all time .. end of today UTC).
func statsRange(w http.ResponseWriter, r *http.Request, now time.Time, defStart time.Time) (from, to time.Time, ok bool) {
	q := r.URL.Query()
	from, to = defStart, endOfDay(now.UTC())
	for k, dst := range map[string]*time.Time{"start_date": &from, "end_date": &to} {
		if v := q.Get(k); v != "" {
			t, good := parseFlexTime(v, time.UTC)
			if !good {
				compatErr(w, 400, "Invalid "+k)
				return from, to, false
			}
			*dst = t
		}
	}
	return from, to, true
}

func (s *Server) compatStats(w http.ResponseWriter, r *http.Request, u *store.User, self bool) {
	q := r.URL.Query()
	from, to, ok := statsRange(w, r, s.now(), time.Unix(0, 0))
	if !ok {
		return
	}
	presence := wantPresence(r)
	filter := csvSet(q.Get("filter_by_project"))
	aggs, err := s.aggregate(r.Context(), u, from, to, filter, self, nil, s.appReader(r, u))
	if err != nil {
		compatErr(w, 500, "internal error")
		return
	}
	sortAggs(aggs, presence)
	st, pr, dk := sumAggs(aggs)
	total := st
	if presence {
		total = pr
	}
	setCompatHeaders(w, r, presence)
	if q.Get("total_seconds") == "true" {
		jsonOut(w, 200, map[string]any{"total_seconds": int64(total), "presence_seconds": int64(pr), "struggle_seconds": int64(st), "as_of": isoZ(s.now())})
		return
	}
	features := csvSet(q.Get("features"))
	if features == nil {
		features = map[string]bool{"languages": true}
	}
	days := math.Max((to.Sub(from)).Hours()/24, 0)
	avg := 0.0
	if days > 0 {
		avg = total / days
	}
	data := map[string]any{
		"username": u.Username, "user_id": u.Username, "is_coding_activity_visible": true, "is_other_usage_visible": true,
		"status": "ok", "start": isoZ(from), "end": isoZ(to), "range": "all_time", "human_readable_range": "All Time",
		"total_seconds": int64(total), "daily_average": avg,
		"human_readable_total": detailedText(total), "human_readable_daily_average": detailedText(avg),
		// SeaTime extras
		"presence_seconds": int64(pr), "struggle_seconds": int64(st), "dark_flow_seconds": int64(dk),
		"metric": map[bool]string{false: "struggle", true: "presence"}[presence],
	}
	if features["projects"] {
		limit, _ := strconv.Atoi(q.Get("limit"))
		var list []map[string]any
		for _, a := range aggs {
			if limit > 0 && len(list) >= limit {
				break
			}
			list = append(list, projectChunk(a, presence, total))
		}
		if list == nil {
			list = []map[string]any{}
		}
		data["projects"] = list
		if filter != nil {
			data["unique_total_seconds"] = int64(total) // Hackatime: gap-capped heartbeat seconds; seatime: same headline metric
		}
	}
	if features["languages"] {
		data["languages"] = []any{} // SeaTime does not track languages
	}
	jsonOut(w, 200, map[string]any{"data": data, "trust_factor": trustJSON(s.trustFor(r.Context(), u)), "as_of": isoZ(s.now())})
}

func projectChunk(a *projAgg, presence bool, total float64) map[string]any {
	v := a.metric(presence)
	pct := 0.0
	if total > 0 {
		pct = math.Round(10000*v/total) / 100
	}
	n := int64(v)
	return statusJSON(map[string]any{"name": a.Name, "total_seconds": n, "text": detailedText(v), "hours": n / 3600, "minutes": n % 3600 / 60,
		"percent": pct, "digital": digital(v),
		"presence_seconds": int64(a.Presence), "struggle_seconds": int64(a.Struggle), "dark_flow_seconds": int64(a.Dark)}, a.St)
}

func (s *Server) compatProjectNames(w http.ResponseWriter, r *http.Request, u *store.User, self bool) {
	from, to, ok := statsRange(w, r, s.now(), startOfDay(s.now().UTC().AddDate(0, 0, -30)))
	if !ok {
		return
	}
	aggs, err := s.aggregate(r.Context(), u, from, to, nil, self, nil, s.appReader(r, u))
	if err != nil {
		compatErr(w, 500, "internal error")
		return
	}
	names := []string{}
	for _, a := range aggs {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	setCompatHeaders(w, r, false)
	jsonOut(w, 200, map[string]any{"projects": names})
}

func projectDetail(a *projAgg, presence bool) map[string]any {
	var repo any
	switch {
	case a.Repo != "":
		repo = "https://github.com/" + a.Repo
	case a.Remote != "":
		repo = a.Remote
	}
	first, last := isoZ(a.First), isoZ(a.Last)
	return statusJSON(map[string]any{"name": a.Name, "total_seconds": int64(a.metric(presence)), "languages": []any{}, "repo_url": repo,
		"total_heartbeats": a.Minutes, "first_heartbeat": first, "last_heartbeat": last, "most_recent_heartbeat": last, "archived": false,
		"presence_seconds": int64(a.Presence), "struggle_seconds": int64(a.Struggle), "dark_flow_seconds": int64(a.Dark)}, a.St)
}

func (s *Server) handleCompatProject(w http.ResponseWriter, r *http.Request) {
	u, self, ok := s.compatUser(w, r, r.PathValue("user"))
	if !ok {
		return
	}
	from, to, ok := statsRange(w, r, s.now(), time.Unix(0, 0))
	if !ok {
		return
	}
	name := r.PathValue("project")
	aggs, err := s.aggregate(r.Context(), u, from, to, map[string]bool{name: true}, self, nil, s.appReader(r, u))
	if err != nil {
		compatErr(w, 500, "internal error")
		return
	}
	if len(aggs) == 0 {
		compatErr(w, 404, "project not found")
		return
	}
	setCompatHeaders(w, r, wantPresence(r))
	d := projectDetail(aggs[0], wantPresence(r))
	d["response_as_of"] = isoZ(s.now())
	jsonOut(w, 200, d)
}

func (s *Server) handleCompatDetails(w http.ResponseWriter, r *http.Request) {
	u, self, ok := s.compatUser(w, r, r.PathValue("user"))
	if !ok {
		return
	}
	from, to, ok := statsRange(w, r, s.now(), time.Unix(0, 0))
	if !ok {
		return
	}
	aggs, err := s.aggregate(r.Context(), u, from, to, csvSet(r.URL.Query().Get("projects")), self, nil, s.appReader(r, u))
	if err != nil {
		compatErr(w, 500, "internal error")
		return
	}
	p := wantPresence(r)
	sortAggs(aggs, p)
	out := []map[string]any{}
	for _, a := range aggs {
		out = append(out, projectDetail(a, p))
	}
	setCompatHeaders(w, r, p)
	jsonOut(w, 200, map[string]any{"projects": out, "as_of": isoZ(s.now())})
}

// ---------------- /api/summary ----------------

func summaryInterval(now time.Time, interval string) (time.Time, time.Time) {
	switch interval {
	case "today":
		return startOfDay(now), endOfDay(now)
	case "yesterday":
		y := now.AddDate(0, 0, -1)
		return startOfDay(y), endOfDay(y)
	case "week", "7_days":
		off := (int(now.Weekday()) + 6) % 7 // Monday start
		m := startOfDay(now.AddDate(0, 0, -off))
		return m, endOfDay(m.AddDate(0, 0, 6))
	case "last_7_days":
		return startOfDay(now.AddDate(0, 0, -7)), endOfDay(now)
	case "month", "30_days":
		m := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		return m, endOfDay(m.AddDate(0, 1, -1))
	case "last_30_days":
		return startOfDay(now.AddDate(0, 0, -30)), endOfDay(now)
	case "6_months":
		m := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, -5, 0)
		return m, endOfDay(time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, 1, -1))
	case "last_6_months":
		return startOfDay(now.AddDate(0, -6, 0)), endOfDay(now)
	case "year", "12_months":
		y := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location())
		return y, endOfDay(y.AddDate(1, 0, -1))
	case "last_12_months", "last_year":
		return startOfDay(now.AddDate(-1, 0, 0)), endOfDay(now)
	case "any", "all_time", "":
		return time.Unix(0, 0), endOfDay(now)
	}
	return startOfDay(now), endOfDay(now)
}

func (s *Server) handleCompatSummary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ident := firstNonEmpty(q.Get("user_id"), q.Get("user"))
	if ident == "" {
		compatErr(w, 400, "Missing required parameter: user_id")
		return
	}
	u, self, ok := s.compatUser(w, r, ident)
	if !ok {
		return
	}
	now := s.now().UTC()
	var from, to time.Time
	if f, t := firstNonEmpty(q.Get("from"), q.Get("start")), firstNonEmpty(q.Get("to"), q.Get("end")); f != "" && t != "" {
		a, ok1 := parseFlexTime(f, time.UTC)
		b, ok2 := parseFlexTime(t, time.UTC)
		if !ok1 || !ok2 {
			compatErr(w, 400, "Invalid date range")
			return
		}
		from, to = startOfDay(a), endOfDay(b)
	} else {
		from, to = summaryInterval(now, firstNonEmpty(q.Get("interval"), q.Get("range")))
	}
	presence := wantPresence(r)
	aggs, err := s.aggregate(r.Context(), u, from, to.Add(time.Second), nil, self, nil, s.appReader(r, u))
	if err != nil {
		compatErr(w, 500, "internal error")
		return
	}
	sortAggs(aggs, presence)
	st, pr, _ := sumAggs(aggs)
	projects := []map[string]any{}
	for _, a := range aggs {
		projects = append(projects, statusJSON(map[string]any{"key": a.Name, "total": int64(a.metric(presence)),
			"presence_seconds": int64(a.Presence), "struggle_seconds": int64(a.Struggle)}, a.St))
	}
	setCompatHeaders(w, r, presence)
	jsonOut(w, 200, map[string]any{
		"user_id": ident, "from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339),
		"projects": projects, "languages": []any{}, "editors": map[string]any{}, "operating_systems": map[string]any{},
		"machines": map[string]any{}, "categories": map[string]any{}, "branches": map[string]any{},
		"entities": map[string]any{}, "labels": map[string]any{},
		"presence_seconds": int64(pr), "struggle_seconds": int64(st), "as_of": isoZ(s.now()),
	})
}

// ---------------- /api/hackatime/v1 (WakaTime-shaped) ----------------

func (s *Server) handleWakaUser(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.compatUser(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	jsonOut(w, 200, map[string]any{"data": map[string]any{
		"id": u.Username, "username": u.Username, "display_name": firstNonEmpty(u.DisplayName, u.Username),
		"full_name": firstNonEmpty(u.DisplayName, u.Username), "photo": u.Avatar, "timezone": tzName(u.TZOffsetMin),
		"created_at": u.Created.Format(time.RFC3339), "modified_at": u.Created.Format(time.RFC3339), "plan": "free"}})
}

func (s *Server) handleWakaSummaries(w http.ResponseWriter, r *http.Request) {
	u, self, ok := s.compatUser(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	q := r.URL.Query()
	dateRe := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	if !dateRe.MatchString(q.Get("start")) || !dateRe.MatchString(q.Get("end")) {
		compatErr(w, 400, "start and end must be valid dates in YYYY-MM-DD format")
		return
	}
	loc := userLoc(u)
	tzs := q.Get("timezone")
	if tzs != "" {
		l, err := time.LoadLocation(tzs)
		if err != nil {
			compatErr(w, 400, "Invalid timezone")
			return
		}
		loc = l
	} else {
		tzs = tzName(u.TZOffsetMin)
	}
	start, _ := time.ParseInLocation("2006-01-02", q.Get("start"), loc)
	end, _ := time.ParseInLocation("2006-01-02", q.Get("end"), loc)
	if end.Before(start) {
		compatErr(w, 400, "end must not be before start")
		return
	}
	if int(end.Sub(start).Hours()/24) >= 366 {
		compatErr(w, 400, "date range cannot exceed 366 days")
		return
	}
	var filter map[string]bool
	if p := q.Get("project"); p != "" {
		filter = map[string]bool{p: true}
	}
	presence := wantPresence(r)
	aggs, err := s.aggregate(r.Context(), u, start, end.AddDate(0, 0, 1), filter, self, loc, s.appReader(r, u))
	if err != nil {
		compatErr(w, 500, "internal error")
		return
	}
	var data []map[string]any
	var totalAll float64
	activeDays := 0
	nowLoc := s.now().In(loc)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		var dayTotal, dayPresence float64
		var rows []*projAgg
		for _, a := range aggs {
			if x := a.days[key]; x != nil {
				c := &projAgg{Name: a.Name, Struggle: x.Struggle, Presence: x.Presence, Dark: x.Dark, St: a.St}
				rows = append(rows, c)
				dayTotal += c.metric(presence)
				dayPresence += x.Presence
			}
		}
		sortAggs(rows, presence)
		projects := []map[string]any{}
		for _, c := range rows {
			v := c.metric(presence)
			if v < 1 {
				continue
			}
			e := duration(v)
			e["name"] = c.Name
			e["percent"] = math.Round(10000*v/dayTotal) / 100
			e["presence_seconds"], e["struggle_seconds"] = int64(c.Presence), int64(c.Struggle)
			e["status"] = c.St.Status
			projects = append(projects, e)
		}
		gt := duration(dayTotal)
		gt["presence_seconds"] = int64(dayPresence)
		text := d.Format("January 2, 2006")
		if key == nowLoc.Format("2006-01-02") {
			text = "Today"
		} else if key == nowLoc.AddDate(0, 0, -1).Format("2006-01-02") {
			text = "Yesterday"
		}
		data = append(data, map[string]any{
			"grand_total": gt, "branches": []any{}, "categories": []any{}, "dependencies": []any{}, "editors": []any{},
			"entities": []any{}, "languages": []any{}, "machines": []any{}, "operating_systems": []any{}, "projects": projects,
			"range": map[string]any{"date": key, "start": isoZ(d), "end": isoZ(endOfDay(d)), "text": text, "timezone": tzs}})
		totalAll += dayTotal
		if int64(dayTotal) > 0 {
			activeDays++
		}
	}
	avg := 0.0
	if len(data) > 0 {
		avg = totalAll / float64(len(data))
	}
	cum := duration(totalAll)
	cum["seconds"] = cum["total_seconds"]
	delete(cum, "total_seconds")
	setCompatHeaders(w, r, presence)
	jsonOut(w, 200, map[string]any{"data": data, "as_of": isoZ(s.now()), "cumulative_total": cum,
		"daily_average": map[string]any{"holidays": len(data) - activeDays, "days_including_holidays": len(data), "days_minus_holidays": activeDays,
			"seconds": int64(avg), "seconds_including_other_language": int64(avg), "text": detailedText(avg), "text_including_other_language": detailedText(avg)},
		"start": start.Format("2006-01-02"), "end": end.Format("2006-01-02")})
}

func (s *Server) handleWakaStatusbar(w http.ResponseWriter, r *http.Request) {
	u, self, ok := s.compatUser(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	loc := userLoc(u)
	now := s.now().In(loc)
	presence := wantPresence(r)
	aggs, err := s.aggregate(r.Context(), u, startOfDay(now), now.Add(time.Minute), nil, self, nil, s.appReader(r, u))
	if err != nil {
		compatErr(w, 500, "internal error")
		return
	}
	st, pr, _ := sumAggs(aggs)
	total := st
	if presence {
		total = pr
	}
	setCompatHeaders(w, r, presence)
	jsonOut(w, 200, map[string]any{"data": map[string]any{"grand_total": map[string]any{
		"text": detailedText(total), "total_seconds": int64(total), "presence_seconds": int64(pr), "struggle_seconds": int64(st)}}, "as_of": isoZ(s.now())})
}

func (s *Server) handleWakaLast7(w http.ResponseWriter, r *http.Request) {
	u, self, ok := s.compatUser(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	loc := userLoc(u)
	now := s.now().In(loc)
	from, to := startOfDay(now.AddDate(0, 0, -7)), endOfDay(now)
	presence := wantPresence(r)
	aggs, err := s.aggregate(r.Context(), u, from, to.Add(time.Second), nil, self, loc, s.appReader(r, u))
	if err != nil {
		compatErr(w, 500, "internal error")
		return
	}
	sortAggs(aggs, presence)
	st, pr, dk := sumAggs(aggs)
	total := st
	if presence {
		total = pr
	}
	days := map[string]bool{}
	for _, a := range aggs {
		for k := range a.days {
			days[k] = true
		}
	}
	avg := 0.0
	if len(days) > 0 {
		avg = math.Round(10*total/float64(len(days))) / 10
	}
	var projects []map[string]any
	for _, a := range aggs {
		projects = append(projects, projectChunk(a, presence, total))
	}
	if projects == nil {
		projects = []map[string]any{}
	}
	coding := duration(total)
	coding["name"], coding["percent"] = "coding", 100.0
	setCompatHeaders(w, r, presence)
	jsonOut(w, 200, map[string]any{"data": map[string]any{
		"username": u.Username, "user_id": u.Username, "start": from.Format(time.RFC3339), "end": to.Format(time.RFC3339), "status": "ok",
		"total_seconds": int64(total), "daily_average": avg, "days_including_holidays": len(days),
		"range": "last_7_days", "human_readable_range": "Last 7 Days",
		"human_readable_total": detailedText(total), "human_readable_daily_average": detailedText(avg),
		"is_coding_activity_visible": true, "is_other_usage_visible": true,
		"editors": []any{}, "languages": []any{}, "categories": []any{coding}, "projects": projects,
		"presence_seconds": int64(pr), "struggle_seconds": int64(st), "dark_flow_seconds": int64(dk)}, "as_of": isoZ(s.now())})
}
