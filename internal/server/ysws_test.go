package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

// compat fixture: user "carol" with a Slack ID and a Hack Club Auth id, two visible projects and one hidden one.
type compatFix struct {
	e   *tenv
	d   *device
	u   *store.User
	day time.Time // UTC midnight of today
}

const (
	testSlack = "U0TESTAAA"
	testHCA   = "ident!test-carol"
)

func newCompat(t *testing.T) *compatFix {
	t.Helper()
	e := newEnv(t)
	d, _ := e.enroll("carol")
	ctx := context.Background()
	u, err := e.st.UserByName(ctx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.LinkIdentity(ctx, u.ID, store.Identity{Provider: "hackclub", Subject: testHCA}, ""); err != nil {
		t.Fatal(err)
	}
	e.st.SetSlackID(ctx, u.ID, testSlack)
	m := minute(30)
	bs := []ledger.Bucket{
		// app: 3 minutes, 100s struggle of 150s presence
		{Project: "github.com/carol/app", Start: m, CodingSeconds: 40, AINotCountedSeconds: 20},
		{Project: "github.com/carol/app", Start: m.Add(time.Minute), CodingSeconds: 30, AINotCountedSeconds: 30},
		{Project: "github.com/carol/app", Start: m.Add(2 * time.Minute), CodingSeconds: 30},
		// tool: 1 minute
		{Project: "github.com/carol/tool", Start: m.Add(3 * time.Minute), CodingSeconds: 50, AINotCountedSeconds: 10},
		// hidden project
		{Project: "local:secret", Start: m.Add(4 * time.Minute), CodingSeconds: 60},
	}
	code, out := d.sync(chain.Checkpoint{Seq: 3, Hash: h(3)}, bs, func(r *api.SyncReq) {
		r.Projects = []api.ProjectMeta{
			{Key: "github.com/carol/app", Name: "app", Remote: "https://github.com/carol/app"},
			{Key: "github.com/carol/tool", Name: "tool", Remote: "https://github.com/carol/tool"},
			{Key: "local:secret", Name: "secret"},
		}
	})
	if code != 200 {
		t.Fatal(code, out)
	}
	// the device sent each project's records and the server checked them
	checkedSegments(t, e, u, d.id, bs)
	p, err := e.st.ProjectByKey(ctx, u.ID, "local:secret")
	if err != nil {
		t.Fatal(err)
	}
	e.st.SetProjectHidden(ctx, u.ID, p.ID, true)
	now := time.Now().UTC()
	return &compatFix{e, d, u, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)}
}

// checkedSegments stores one verified segment per project crediting
// exactly bs: what the server keeps once a device's records check out.
func checkedSegments(t *testing.T, e *tenv, u *store.User, deviceID string, bs []ledger.Bucket) {
	t.Helper()
	ctx := context.Background()
	by := map[string][]ledger.Bucket{}
	var order []string
	for _, b := range bs {
		if by[b.Project] == nil {
			order = append(order, b.Project)
		}
		by[b.Project] = append(by[b.Project], b)
	}
	for _, key := range order {
		p, err := e.st.UpsertProject(ctx, u.ID, api.ProjectMeta{Key: key})
		if err != nil {
			t.Fatal(err)
		}
		var mins []store.SegmentMinute
		sg := &store.Segment{ID: "seg-" + key, UserID: u.ID, ProjectID: p.ID, DeviceID: deviceID, Status: store.SegVerified,
			Created: time.Now(), Checked: time.Now()}
		for _, b := range by[key] {
			mins = append(mins, store.SegmentMinute{StartMin: b.Start.Unix() / 60, Human: b.Seconds(), Dark: b.AINotCountedSeconds})
			if sg.FirstTS.IsZero() {
				sg.FirstTS = b.Start
			}
			sg.LastTS = b.Start.Add(time.Minute)
			sg.Scored.CodingSeconds += b.CodingSeconds
		}
		if err := e.st.PutSegment(ctx, sg, []byte{0}, mins); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *compatFix) get(path string) (int, map[string]any) {
	f.e.t.Helper()
	return f.getAs(path, "")
}

func (f *compatFix) getAs(path, token string) (int, map[string]any) {
	f.e.t.Helper()
	code, out := f.e.apiDo("GET", path, token, nil)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		f.e.t.Fatalf("%s: not JSON: %s", path, out)
	}
	return code, m
}

func num(t *testing.T, m map[string]any, keys ...string) float64 {
	t.Helper()
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %v is not an object", keys, cur)
		}
		cur = mm[k]
	}
	f, ok := cur.(float64)
	if !ok {
		t.Fatalf("path %v: %v is not a number (%v)", keys, cur, m)
	}
	return f
}

func TestCompatStatsShapeAndMetric(t *testing.T) {
	f := newCompat(t)
	// the same call works with username, @username, Slack ID and Hack Club Auth id
	for _, id := range []string{"carol", "@carol", testSlack, testHCA} {
		code, m := f.get("/api/v1/users/" + url.PathEscape(id) + "/stats?features=projects&start_date=2020-01-01")
		if code != 200 {
			t.Fatalf("%s: %d %v", id, code, m)
		}
		// hidden project excluded: counted 40+30+30+50 = 150; under heartbeat/1
		// presence is the same counted number
		if got := num(t, m, "data", "total_seconds"); got != 150 {
			t.Fatalf("%s: total_seconds %v, want 150", id, got)
		}
		if num(t, m, "data", "presence_seconds") != 150 || num(t, m, "data", "dark_flow_seconds") != 60 {
			t.Fatalf("%s: extras %v", id, m)
		}
		projs := m["data"].(map[string]any)["projects"].([]any)
		if len(projs) != 2 {
			t.Fatalf("hidden project leaked or missing: %v", projs)
		}
		first := projs[0].(map[string]any)
		if first["name"] != "app" || first["total_seconds"].(float64) != 100 || first["text"] != "1m 40s" || first["digital"] != "00:01:40" {
			t.Fatalf("project shape: %v", first)
		}
		if first["percent"].(float64) != 66.67 {
			t.Fatalf("percent %v", first["percent"])
		}
		if m["trust_factor"].(map[string]any)["trust_level"] != "blue" {
			t.Fatalf("trust_factor %v", m["trust_factor"])
		}
		d := m["data"].(map[string]any)
		for _, k := range []string{"username", "user_id", "status", "start", "end", "range", "human_readable_total", "daily_average", "is_coding_activity_visible"} {
			if _, ok := d[k]; !ok {
				t.Fatalf("missing Hackatime field %q in %v", k, d)
			}
		}
	}
	// presence metric swaps the headline number; under heartbeat/1 it is the
	// same counted number, 150
	if _, m := f.get("/api/v1/users/carol/stats?metric=presence&features=projects"); num(t, m, "data", "total_seconds") != 150 {
		t.Fatalf("metric=presence: %v", m)
	}
	// total_seconds=true short form (summer-of-making style) + filter_by_project
	_, m := f.get("/api/v1/users/" + testSlack + "/stats?filter_by_project=app,secret&features=projects&total_seconds=true&test_param=true&start_date=" + url.QueryEscape(time.Now().UTC().Add(-48*time.Hour).Format(time.RFC3339)))
	if num(t, m, "total_seconds") != 100 {
		t.Fatalf("filtered total (hidden project must not be reachable by name): %v", m)
	}
	// start_date after the activity -> zero
	if _, m := f.get("/api/v1/users/carol/stats?total_seconds=true&start_date=" + url.QueryEscape(time.Now().UTC().Add(time.Hour).Format(time.RFC3339))); num(t, m, "total_seconds") != 0 {
		t.Fatalf("future start: %v", m)
	}
	// end_date before it -> zero; date-only values parse
	if _, m := f.get("/api/v1/users/carol/stats?total_seconds=true&start_date=2020-01-01&end_date=2020-01-02"); num(t, m, "total_seconds") != 0 {
		t.Fatalf("old range: %v", m)
	}
	// errors
	if code, m := f.get("/api/v1/users/nobody/stats"); code != 404 || m["error"] == nil {
		t.Fatalf("unknown user: %d %v", code, m)
	}
	if code, _ := f.get("/api/v1/users/carol/stats?start_date=banana"); code != 400 {
		t.Fatalf("bad date: %d", code)
	}
	// the metric header tells callers what total_seconds means
	req, _ := newReq(f.e, "/api/v1/users/carol/stats")
	if got := req.Header.Get("X-Seatime-Metric"); got != "struggle_seconds" {
		t.Fatalf("X-Seatime-Metric %q", got)
	}
}

func TestCompatProjectsAndLookups(t *testing.T) {
	f := newCompat(t)
	_, m := f.get("/api/v1/users/carol/projects")
	if ps := m["projects"].([]any); len(ps) != 2 || ps[0] != "app" || ps[1] != "tool" {
		t.Fatalf("projects: %v", m)
	}
	code, m := f.get("/api/v1/users/carol/project/app")
	if code != 200 || m["name"] != "app" || m["total_seconds"].(float64) != 100 || m["repo_url"] != "https://github.com/carol/app" || m["total_heartbeats"].(float64) != 3 {
		t.Fatalf("project detail: %d %v", code, m)
	}
	if m["first_heartbeat"] == nil || m["presence_seconds"].(float64) != 100 {
		t.Fatalf("project detail extras: %v", m)
	}
	if code, _ := f.get("/api/v1/users/carol/project/secret"); code != 404 {
		t.Fatalf("hidden project by name: %d", code)
	}
	if code, _ := f.get("/api/v1/users/carol/project/nope"); code != 404 {
		t.Fatalf("unknown project: %d", code)
	}
	_, m = f.get("/api/v1/users/carol/projects/details?projects=tool,app")
	ps := m["projects"].([]any)
	if len(ps) != 2 || ps[0].(map[string]any)["name"] != "app" { // sorted by hours, not request order
		t.Fatalf("details: %v", m)
	}
	_, m = f.get("/api/v1/users/carol/projects/details?projects=tool")
	if len(m["projects"].([]any)) != 1 {
		t.Fatalf("details filter: %v", m)
	}
	// lookups
	if code, m := f.get("/api/v1/users/lookup_slack_uid/" + testSlack); code != 200 || m["user_id"] != "carol" || m["slack_uid"] != testSlack {
		t.Fatalf("lookup_slack_uid: %d %v", code, m)
	}
	if code, _ := f.get("/api/v1/users/lookup_slack_uid/U0NOBODY1"); code != 404 {
		t.Fatalf("unknown slack id: %d", code)
	}
	if code, m := f.get("/api/v1/users/lookup_email/someone@example.test"); code != 404 || m["error"] == nil {
		t.Fatalf("lookup_email: %d %v", code, m)
	}
	if code, m := f.get("/api/v1/users/" + testHCA + "/trust_factor"); code != 200 || m["trust_level"] != "blue" {
		t.Fatalf("trust_factor: %d %v", code, m)
	}
	// the route that existed before the compat layer still works
	if code, m := f.get("/api/v1/users/carol/projects/app/stats"); code != 200 || m["struggle_seconds"].(float64) != 100 {
		t.Fatalf("native stats: %d %v", code, m)
	}
}

func TestCompatPrivateProfile(t *testing.T) {
	f := newCompat(t)
	u := *f.u
	u.Public = false
	if err := f.e.st.UpdateUser(context.Background(), &u); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/api/v1/users/carol/stats", "/api/v1/users/" + testSlack + "/projects", "/api/v1/users/carol/project/app",
		"/api/hackatime/v1/users/carol/summaries?start=2020-01-01&end=2020-01-02", "/api/summary?user=carol"} {
		if code, m := f.get(p); code != 403 || m["error"] == nil {
			t.Fatalf("%s: %d %v", p, code, m)
		}
	}
	if code, _ := f.get("/api/v1/users/lookup_slack_uid/" + testSlack); code != 404 {
		t.Fatalf("private user must not be resolvable by Slack id: %d", code)
	}
	// the owner can read it with their device token, hidden projects included
	_, m := f.getAs("/api/v1/users/carol/stats?features=projects&total_seconds=true", f.d.token)
	if num(t, m, "total_seconds") != 210 {
		t.Fatalf("owner total (incl. hidden project): %v", m)
	}
}

func TestCompatSummaryAndWakaEndpoints(t *testing.T) {
	f := newCompat(t)
	from := f.day.AddDate(0, 0, -1).Format("2006-01-02")
	to := f.day.AddDate(0, 0, 1).Format("2006-01-02")

	_, m := f.get("/api/summary?user=" + testSlack + "&from=" + from + "&to=" + to)
	pj := m["projects"].([]any)
	if len(pj) != 2 || pj[0].(map[string]any)["key"] != "app" || pj[0].(map[string]any)["total"].(float64) != 100 {
		t.Fatalf("/api/summary: %v", m)
	}
	if _, ok := m["languages"]; !ok {
		t.Fatalf("/api/summary shape: %v", m)
	}
	if code, _ := f.get("/api/summary"); code != 400 {
		t.Fatalf("missing user: %d", code)
	}
	if _, m = f.get("/api/summary?user=carol&interval=last_7_days"); len(m["projects"].([]any)) != 2 {
		t.Fatalf("interval: %v", m)
	}

	// wakatime-shaped daily summaries
	code, m := f.get("/api/hackatime/v1/users/" + testHCA + "/summaries?start=" + from + "&end=" + to)
	if code != 200 {
		t.Fatalf("summaries: %d %v", code, m)
	}
	days := m["data"].([]any)
	if len(days) != 3 {
		t.Fatalf("want 3 days, got %d", len(days))
	}
	if num(t, m, "cumulative_total", "seconds") != 150 || num(t, m, "daily_average", "days_including_holidays") != 3 {
		t.Fatalf("summaries totals: %v", m)
	}
	var active int
	for _, d := range days {
		dm := d.(map[string]any)
		gt := dm["grand_total"].(map[string]any)
		if gt["total_seconds"].(float64) > 0 {
			active++
			if len(dm["projects"].([]any)) == 0 || gt["presence_seconds"].(float64) == 0 {
				t.Fatalf("active day without projects/presence: %v", dm)
			}
		}
		if dm["range"].(map[string]any)["timezone"] == nil || gt["decimal"] == nil || gt["digital"] == nil {
			t.Fatalf("day shape: %v", dm)
		}
	}
	if active < 1 || active > 2 { // 30 minutes ago can straddle UTC midnight
		t.Fatalf("active days: %d", active)
	}
	// project filter and timezone
	_, m = f.get("/api/hackatime/v1/users/carol/summaries?project=tool&timezone=America/New_York&start=" + from + "&end=" + to)
	if num(t, m, "cumulative_total", "seconds") != 50 {
		t.Fatalf("project filter: %v", m)
	}
	for q, want := range map[string]int{
		"start=2025-01-02&end=2025-01-01":                            400,
		"start=bad&end=2025-01-01":                                   400,
		"start=2024-01-01&end=2025-12-31":                            400, // > 366 days
		"start=2025-01-01&end=2025-01-02&timezone=Mars/Olympus_Mons": 400,
	} {
		if code, _ := f.get("/api/hackatime/v1/users/carol/summaries?" + q); code != want {
			t.Fatalf("%s: %d", q, code)
		}
	}

	// "current" needs the device token
	if code, _ := f.get("/api/hackatime/v1/users/current/stats/last_7_days"); code != 401 {
		t.Fatalf("current without token: %d", code)
	}
	code, m = f.getAs("/api/hackatime/v1/users/current/stats/last_7_days", f.d.token)
	if code != 200 || num(t, m, "data", "total_seconds") != 210 || m["data"].(map[string]any)["status"] != "ok" || m["data"].(map[string]any)["range"] != "last_7_days" {
		t.Fatalf("last_7_days (owner sees hidden projects): %d %v", code, m)
	}
	if _, m = f.get("/api/hackatime/v1/users/carol/stats/last_7_days"); num(t, m, "data", "total_seconds") != 150 || num(t, m, "data", "presence_seconds") != 150 {
		t.Fatalf("last_7_days public: %v", m)
	}
	if code, m = f.get("/api/hackatime/v1/users/carol/statusbar/today"); code != 200 || m["data"].(map[string]any)["grand_total"] == nil {
		t.Fatalf("statusbar: %d %v", code, m)
	}
	code, m = f.get("/api/hackatime/v1/users/carol")
	if d := m["data"].(map[string]any); code != 200 || d["username"] != "carol" || d["plan"] != "free" || d["timezone"] != "UTC" {
		t.Fatalf("show_user: %d %v", code, m)
	}
}

func newReq(e *tenv, path string) (*http.Response, error) {
	resp, err := http.Get(e.ts.URL + path)
	if err == nil {
		resp.Body.Close()
	}
	return resp, err
}

func commitEv(i int) event.Event {
	return event.Event{Seq: int64(i + 1), Kind: event.Commit, SHA: strings.Repeat(string(rune('a'+i%6)), 40),
		TS: time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC)}
}

// The per-project export: the checked hours, the status, the commit
// range, and the block to paste, as JSON and as text.
func TestYSWSExportEndToEnd(t *testing.T) {
	e := newEnv(t)
	d, _ := e.enroll("dan")
	evs, heads := buildChain(t, 12)
	for _, s := range []int{6, 12} {
		if code, out := d.sync(chain.Checkpoint{Seq: int64(s), Hash: heads[s-1]}, nil, nil); code != 200 {
			t.Fatal(code, out)
		}
	}
	code, out := e.apiDo("POST", "/api/v1/segments", d.token, d.bundle(evs, chain.Genesis, nil))
	var r api.SegmentResp
	json.Unmarshal(out, &r)
	if code != 200 || r.Status != api.StatusVerified {
		t.Fatalf("segment: %d %s", code, out)
	}
	code, out = e.apiDo("GET", "/api/v1/users/dan/projects/proj/ysws", "", nil)
	var x api.YSWSExport
	json.Unmarshal(out, &x)
	if code != 200 || x.Status != api.StatusVerified || x.AsOf.IsZero() || x.Hours != round1(r.Scored.Seconds()/3600) ||
		x.Commits != 1 || x.CommitRange == nil || x.CommitRange.Count != 1 || x.CodeURL != "https://github.com/dan/proj" ||
		x.ShareURL != "http://example.test/@dan/proj" {
		t.Fatalf("export: %d %s", code, out)
	}
	for _, line := range []string{"Override Hours Spent: ", "(SeaTime hours, verified)", "Override Hours Spent Justification: ",
		"every record matched the device's hash chain", "Code URL: https://github.com/dan/proj", "Commits: ", "SeaTime project: http://example.test/@dan/proj"} {
		if !strings.Contains(x.Text, line) {
			t.Fatalf("text block missing %q:\n%s", line, x.Text)
		}
	}
	// plain text variant is exactly the copy-paste block
	code, out = e.apiDo("GET", "/api/v1/users/dan/projects/proj/ysws.txt", "", nil)
	if code != 200 || string(out) != x.Text {
		t.Fatalf("txt: %d %q", code, out)
	}
	if code, _ := e.apiDo("GET", "/api/v1/users/dan/projects/nope/ysws", "", nil); code != 404 {
		t.Fatalf("unknown project: %d", code)
	}
	// found by Slack ID too
	u, _ := e.st.UserByName(context.Background(), "dan")
	e.st.SetSlackID(context.Background(), u.ID, "U0DANDAN1")
	if code, _ := e.apiDo("GET", "/api/v1/users/U0DANDAN1/projects/proj/ysws", "", nil); code != 200 {
		t.Fatalf("by Slack ID: %d", code)
	}
	// a tampered segment fails the project, says why, and credits nothing more
	bad := d.bundle(evs, chain.Genesis, nil)
	bad.Entries[4].Event.Text = "totally different prompt text"
	d.resign(&bad)
	e.apiDo("POST", "/api/v1/segments", d.token, bad)
	_, out = e.apiDo("GET", "/api/v1/users/dan/projects/proj/ysws", "", nil)
	var fx api.YSWSExport
	json.Unmarshal(out, &fx)
	if fx.Status != api.StatusFailed || !strings.Contains(fx.StatusReason, "did not match") || fx.Hours != x.Hours ||
		!strings.Contains(fx.Text, "FAILED, do not use") {
		t.Fatalf("after a tampered segment: %s", out)
	}
}
