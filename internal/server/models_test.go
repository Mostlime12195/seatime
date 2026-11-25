package server

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

var modelLi = regexp.MustCompile(`<li><span[^>]*>([^<]+)</span><span class="share"><i class="(\w)"[^>]*></i></span><b>([^<]+)</b><span class="np">([^<]*)</span></li>`)

// modelSection is the rows of a page's Models section: name -> "time|prompts".
func modelSection(page string) map[string]string {
	i := strings.Index(page, "<h2>Models</h2>")
	if i < 0 {
		return nil
	}
	sec := page[i:]
	if j := strings.Index(sec, "</section>"); j >= 0 {
		sec = sec[:j]
	}
	out := map[string]string{}
	for _, m := range modelLi.FindAllStringSubmatch(sec, -1) {
		out[m[1]] = m[3] + "|" + m[4]
	}
	return out
}

// The owner sees counted time and prompts by model on the profile and on
// each project's page, with hand time and time without a model as their
// own rows; the rows add up to the total. Nobody else sees the section.
func TestModelsOnOwnPages(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("mo")
	m := minute(40)
	var bs []ledger.Bucket
	add := func(project string, n int, b ledger.Bucket) {
		for i := 0; i < n; i++ {
			b.Project, b.Start = project, m.Add(time.Duration(len(bs))*time.Minute)
			bs = append(bs, b)
		}
	}
	add("github.com/mo/app", 10, ledger.Bucket{CodingSeconds: 60, Harnesses: []string{"editor:vscode"}})
	add("github.com/mo/app", 12, ledger.Bucket{AISeconds: 60, Prompts: 1, Harnesses: []string{"claude-code"},
		Models: []ledger.ModelUse{{Model: "claude-opus-5-20260115", Seconds: 60, Prompts: 1}}})
	add("github.com/mo/app", 3, ledger.Bucket{AISeconds: 60, Harnesses: []string{"claude-code"},
		Models: []ledger.ModelUse{{Model: " claude-opus-5\n", Seconds: 50}, {Model: "gpt-5.2-codex", Seconds: 500}}}) // more than the minute holds: cut to fit
	add("github.com/mo/app", 5, ledger.Bucket{AISeconds: 40, Prompts: 2, Harnesses: []string{"gemini-cli"}})
	add("github.com/mo/other", 4, ledger.Bucket{AISeconds: 60, Prompts: 1, Harnesses: []string{"cursor"},
		Models: []ledger.ModelUse{{Model: "gpt-5.2-codex", Seconds: 60, Prompts: 1}}})
	meta := func(r *api.SyncReq) {
		r.Projects = []api.ProjectMeta{{Key: "github.com/mo/app", Name: "app"}, {Key: "github.com/mo/other", Name: "other"}}
	}
	if code, out := d.sync(chain.Checkpoint{Seq: 5, Hash: h(5)}, bs, meta); code != 200 {
		t.Fatal(code, out)
	}
	// app: 10 min by hand; opus 12 + 3*(50/550) min; codex 3*(500/550) min; 5*40 s with no model
	_, proj := br.get("/@mo/app?range=30d")
	rows := modelSection(proj)
	want := map[string]string{"claude-opus-5": "12m|12 prompts", "gpt-5.2-codex": "3m|", "By hand": "10m|", "Model not recorded": "3m|10 prompts"}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Errorf("project page rows:\n got %v\nwant %v", rows, want)
	}
	if !strings.Contains(proj, `title="claude-opus-5, claude-opus-5-20260115"`) {
		t.Error("the names as reported are not on the row")
	}
	// the rows' minutes add up to the total the page shows (28m 20s -> 28m)
	if !strings.Contains(proj, `<b>28<i class="u">m</i></b><span>Last 30 days</span>`) {
		t.Error("project total is not 28m")
	}
	_, prof := br.get("/@mo?range=30d")
	rows = modelSection(prof)
	want = map[string]string{"claude-opus-5": "12m|12 prompts", "gpt-5.2-codex": "7m|4 prompts", "By hand": "10m|", "Model not recorded": "3m|10 prompts"}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Errorf("profile rows:\n got %v\nwant %v", rows, want)
	}
	if !strings.Contains(prof, `<b>32<i class="u">m</i></b><span>Last 30 days</span>`) {
		t.Error("profile total is not 32m")
	}
	// the store agrees to the second
	u, _ := e.st.UserByName(t.Context(), "mo")
	ms, _ := e.st.ModelSums(t.Context(), u.ID, 0, time.Unix(0, 0), time.Now())
	sum := 0.0
	for _, x := range ms {
		sum += x.Seconds
	}
	if math.Abs(sum-(10*60+15*60+5*40+4*60)) > 1e-6 {
		t.Errorf("rows hold %.1f s", sum)
	}
	// anyone else: the pages, without the section
	for _, path := range []string{"/@mo", "/@mo/app"} {
		code, page := e.browser().get(path)
		if code != 200 || strings.Contains(page, "<h2>Models</h2>") || strings.Contains(page, "claude-opus") {
			t.Errorf("%s shows models to a visitor (%d)", path, code)
		}
	}
	_, other := e.enroll("zed")
	if _, page := other.get("/@mo/app"); strings.Contains(page, "<h2>Models</h2>") {
		t.Error("another user sees the models")
	}
}

func TestModelRows(t *testing.T) {
	// nothing but hand time: no section
	if rows := modelRows([]store.ModelSum{{Hand: true, Seconds: 600}}, 600); rows != nil {
		t.Fatalf("%+v", rows)
	}
	// three rows of 20.4 min: the minutes shown add up to the 61 the total shows
	rows := modelRows([]store.ModelSum{{Model: "b", Seconds: 1224, Prompts: 2}, {Model: "a", Seconds: 1224}, {Hand: true, Seconds: 1224}}, 3672)
	if len(rows) != 3 || rows[0].Name != "a" || rows[2].Name != "By hand" {
		t.Fatalf("%+v", rows)
	}
	shown := map[string]int{}
	for _, r := range rows {
		shown[r.Time]++
	}
	if shown["21m"] != 1 || shown["20m"] != 2 || fmtDur(3672) != "1h 01m" {
		t.Fatalf("minutes do not add up to the total: %+v", rows)
	}
	// time the rollup does not hold is not dropped, and not given to a model
	rows = modelRows([]store.ModelSum{{Model: "a", Seconds: 60}}, 300)
	if len(rows) != 2 || rows[1].Name != "Model not recorded" || rows[1].Time != "4m" {
		t.Fatalf("%+v", rows)
	}
	for raw, want := range map[string]string{"claude-opus-5-20260115": "claude-opus-5", "gpt-5.2@2026-01-15": "gpt-5.2", " o3 ": "o3",
		"gemini-3-pro-latest": "gemini-3-pro", "claude-opus-5": "claude-opus-5", "anthropic/claude-sonnet-5-5": "anthropic/claude-sonnet-5-5", "20260115": "20260115"} {
		if got := modelName(raw); got != want {
			t.Errorf("modelName(%q) = %q, want %q", raw, got, want)
		}
	}
	// what an application reads: time checked before models were kept is "not_recorded"
	js := modelsJSON([]store.ModelSum{{Model: "b", Seconds: 30, Prompts: 1}, {Model: "a", Seconds: 10}, {Hand: true, Seconds: 20}, {Prompts: 2}}, 100)
	b, _ := json.Marshal(js)
	if string(b) != `[{"kind":"model","model":"a","seconds":10,"prompts":0},{"kind":"model","model":"b","seconds":30,"prompts":1},{"kind":"hand","seconds":20,"prompts":0},{"kind":"not_recorded","seconds":40,"prompts":2}]` {
		t.Fatalf("%s", b)
	}
}

// modelChain is a live-looking session like localChain, with models as
// the tools report them: turns 0-3 answered by one model (named on the stop
// only, as Claude Code does), turns 4-5 by another, turns 6-7 by none, and a
// hand edit at the end.
func modelChain(t *testing.T, project string) ([]event.Event, []string) {
	t.Helper()
	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Millisecond)
	prev := chain.Genesis
	var evs []event.Event
	var heads []string
	add := func(e event.Event) {
		e.Seq, e.Project = int64(len(evs)+1), project
		var err error
		if prev, err = chain.Next(prev, e); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
		heads = append(heads, prev)
	}
	for i := 0; i < 8; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		model := []string{"claude-opus-5-20260115", "claude-opus-5-20260115", "claude-opus-5-20260115", "claude-opus-5-20260115", "gpt-5.2-codex", "gpt-5.2-codex", "", ""}[i]
		add(event.Event{TS: at, Kind: event.Prompt, Harness: "claude-code", SessionID: "s1", Text: chain.Commit(promptText(i)), Words: 15, TextHash: fmt.Sprintf("%064x", 2000+i)})
		add(event.Event{TS: at.Add(40 * time.Second), Kind: event.AgentStop, Harness: "claude-code", SessionID: "s1", Model: model})
	}
	add(event.Event{TS: base.Add(8*time.Minute + 30*time.Second), Kind: event.Edit, Harness: "editor:vscode", Category: ledger.Coding})
	add(event.Event{TS: base.Add(9 * time.Minute), Kind: event.Edit, Harness: "editor:vscode", Category: ledger.Coding})
	return evs, heads
}

// An application the user approved reads the same breakdown for a project
// and a time range, from checked time; the rows add up to the checked
// seconds. A read without the user's token gets the hours and no models.
func TestApplicationReadsModels(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("lena")
	evs, heads := modelChain(t, "local:myapp")
	if code, out := d.sync(chain.Checkpoint{Seq: int64(len(evs)), Hash: heads[len(heads)-1]}, nil, nil); code != 200 {
		t.Fatal(code, out)
	}
	code, out := e.apiDo("POST", "/api/v1/segments", d.token, d.localBundle(evs, "local:myapp"))
	var sr api.SegmentResp
	json.Unmarshal(out, &sr)
	if code != 200 || sr.Status != api.StatusVerified {
		t.Fatalf("segment: %d %s", code, out)
	}
	_, dev := e.enroll("appdev")
	id, secret := registerApp(t, e, dev)
	tok, _ := connect(t, e, br, id, secret)
	path := "/api/v1/users/lena/projects/" + api.HashedProjectKey("local:myapp") + "/stats"
	code, st := apiJSON[api.ProjectStats](t, e, path, tok)
	if code != 200 || st.Status != api.StatusVerified {
		t.Fatalf("%d %+v", code, st)
	}
	got := map[string]api.ModelTime{}
	sum := 0.0
	for _, m := range st.Models {
		got[m.Kind+":"+m.Model] = m
		sum += m.Seconds
	}
	// each gap goes to the heartbeat that ends it: 40 s to each stop, 20 s
	// to the prompt after it (that prompt's turn); then the two hand edits
	want := map[string]api.ModelTime{
		"model:claude-opus-5-20260115": {Seconds: 4*40 + 3*20, Prompts: 4},
		"model:gpt-5.2-codex":          {Seconds: 2*40 + 2*20, Prompts: 2},
		"not_recorded:":                {Seconds: 2*40 + 2*20, Prompts: 2},
		"hand:":                        {Seconds: 50 + 30, Prompts: 0},
	}
	for k, w := range want {
		if g := got[k]; math.Abs(g.Seconds-w.Seconds) > 0.01 || g.Prompts != w.Prompts {
			t.Errorf("%s: %.1f s, %d prompts; want %.1f s, %d", k, g.Seconds, g.Prompts, w.Seconds, w.Prompts)
		}
	}
	if len(st.Models) != len(want) || math.Abs(sum-st.HumanSeconds) > 0.01 || st.HumanSeconds < 500 {
		t.Errorf("rows %+v hold %.1f s, human_seconds %.1f", st.Models, sum, st.HumanSeconds)
	}
	// a time range: the first three minutes hold opus only
	from := evs[0].TS.Truncate(time.Minute)
	code, part := apiJSON[api.ProjectStats](t, e, path+"?since="+from.Format(time.RFC3339)+"&until="+from.Add(3*time.Minute).Format(time.RFC3339), tok)
	psum := 0.0
	for _, m := range part.Models {
		psum += m.Seconds
		if m.Kind != api.ModelNamed || m.Model != "claude-opus-5-20260115" {
			t.Errorf("in the first minutes: %+v", m)
		}
	}
	if code != 200 || part.HumanSeconds <= 0 || part.HumanSeconds >= st.HumanSeconds || math.Abs(psum-part.HumanSeconds) > 0.01 {
		t.Errorf("range: %d, rows %.1f s, human_seconds %.1f", code, psum, part.HumanSeconds)
	}
	// the owner's own device reads them too; nobody else does
	if _, own := apiJSON[api.ProjectStats](t, e, path, d.token); len(own.Models) != len(want) {
		t.Errorf("the owner's device: %+v", own.Models)
	}
	other, _ := e.enroll("zed")
	for who, token := range map[string]string{"no token": "", "another user's device": other.token} {
		code, raw := e.apiDo("GET", path, token, nil)
		if code != 200 || strings.Contains(string(raw), `"models"`) || strings.Contains(string(raw), "opus") || !strings.Contains(string(raw), `"human_seconds"`) {
			t.Errorf("%s: %d %s", who, code, raw)
		}
	}
	// a hidden project is the owner's alone: not the application's
	u, _ := e.st.UserByName(t.Context(), "lena")
	p, _ := e.st.ProjectByKey(t.Context(), u.ID, api.HashedProjectKey("local:myapp"))
	e.st.SetProjectHidden(t.Context(), u.ID, p.ID, true)
	if code, _ := e.apiDo("GET", path, tok, nil); code != 404 {
		t.Errorf("an application read a hidden project: %d", code)
	}
	if code, own := apiJSON[api.ProjectStats](t, e, path, d.token); code != 200 || len(own.Models) != len(want) {
		t.Errorf("the owner's device on a hidden project: %d", code)
	}
}

// A project with no counted time and no commits is not listed: not on the
// profile, not in Settings, not as a new project to make public or hidden.
func TestEmptyProjectsAreNotListed(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("mo")
	bs := []ledger.Bucket{
		{Project: "github.com/mo/app", Start: minute(20), CodingSeconds: 60},
		{Project: "local:memory", Start: minute(19), AINotCountedSeconds: 30, Edits: 2},
		{Project: "local:onlycommits", Start: minute(18), Commits: 1},
	}
	meta := func(r *api.SyncReq) {
		r.Projects = []api.ProjectMeta{{Key: "github.com/mo/app", Name: "app"}, {Key: "local:memory", Name: "memory-folder", Folder: true},
			{Key: "local:onlycommits", Name: "docs-only", Folder: true}}
		r.Commits = []api.CommitRef{{Project: "local:onlycommits", SHA: strings.Repeat("a", 40), TS: minute(18)}}
	}
	if code, out := d.sync(chain.Checkpoint{Seq: 5, Hash: h(5)}, bs, meta); code != 200 {
		t.Fatal(code, out)
	}
	for _, path := range []string{"/@mo", "/@mo?range=all", "/settings", "/"} {
		code, page := br.get(path)
		if code != 200 || !strings.Contains(page, "app") {
			t.Fatalf("%s: %d", path, code)
		}
		if strings.Contains(page, "memory-folder") {
			t.Errorf("%s lists a project with no counted time and no commits", path)
		}
		if path != "/" && !strings.Contains(page, "docs-only") {
			t.Errorf("%s does not list a project that has a commit", path)
		}
	}
	if code, _ := br.get("/@mo/memory-folder"); code != 200 {
		t.Errorf("the project's own page: %d", code)
	}
}
