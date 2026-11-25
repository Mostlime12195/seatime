package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"seatime/pkg/chain"
	"seatime/pkg/ledger"
)

// The hide switch: a project that arrives with a sync is
// shown to its owner once, on the profile and in settings, with "Hide it"
// and "Keep it public". The page's script posts the choice for JSON and
// updates in place, so no reload; without the script the form posts and
// comes back. Only the owner can flip it, and a hidden project is gone from
// the public pages at once.
func TestHideSwitchAtFirstSync(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("alice")
	bs := []ledger.Bucket{
		{Project: "github.com/alice/diary", Start: minute(3), CodingSeconds: 45},
		{Project: "github.com/alice/game", Start: minute(2), CodingSeconds: 45},
	}
	if code, out := d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, bs, nil); code != 200 {
		t.Fatalf("sync: %d %s", code, out)
	}
	_, page := br.get("/@alice")
	if !strings.Contains(page, "New on your profile") || !strings.Contains(page, "data-visibility") || !strings.Contains(page, "/static/hide.js") {
		t.Fatalf("no hide switch on the owner's profile after the first sync:\n%s", page)
	}
	if _, s := br.get("/settings"); !strings.Contains(s, "New on your profile") {
		t.Fatal("no hide switch in settings")
	}
	anon := e.browser()
	if c, _ := anon.get("/@alice/diary"); c != 200 {
		t.Fatalf("public project page: %d", c)
	}
	if _, p := anon.get("/@alice"); strings.Contains(p, "New on your profile") {
		t.Fatal("a visitor sees the owner's hide switch")
	}
	ps, _ := e.st.ListProjects(t.Context(), mustUser(t, e, "alice"))
	ids := map[string]string{}
	for _, p := range ps {
		ids[p.Name] = itoa(p.ID)
	}
	tok := br.csrf("/@alice")

	// the script's request: JSON back, no redirect
	form := url.Values{"csrf": {tok}, "id": {ids["diary"]}, "hidden": {"1"}, "next": {"/@alice"}}
	req, _ := http.NewRequest("POST", e.ts.URL+"/settings/project/visibility", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	br.c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := br.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Hidden bool   `json:"hidden"`
		Name   string `json:"name"`
	}
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if resp.StatusCode != 200 || !got.Hidden || got.Name != "diary" {
		t.Fatalf("visibility for JSON: %d %+v", resp.StatusCode, got)
	}
	if c, _ := anon.get("/@alice/diary"); c != 404 {
		t.Fatalf("hidden project still public: %d", c)
	}

	// another account cannot flip it
	_, bob := e.enroll("bob")
	if c, _ := bob.post("/settings/project/visibility", url.Values{"csrf": {bob.csrf("/settings")}, "id": {ids["game"]}, "hidden": {"1"}}); c != 404 {
		t.Fatalf("bob hid alice's project: %d", c)
	}

	// the plain form: back to the page it came from, never off-site
	for next, want := range map[string]string{"/@alice": "/@alice", "//evil.example/x": "/settings"} {
		c, _ := br.post("/settings/project/visibility", url.Values{"csrf": {tok}, "id": {ids["game"]}, "hidden": {"0"}, "next": {next}})
		if c != http.StatusSeeOther {
			t.Fatalf("form post: %d", c)
		}
		resp, err := br.c.PostForm(e.ts.URL+"/settings/project/visibility", url.Values{"csrf": {tok}, "id": {ids["game"]}, "hidden": {"0"}, "next": {next}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); loc != want {
			t.Fatalf("next %q: redirected to %q", next, loc)
		}
	}
	if c, _ := anon.get("/@alice/game"); c != 200 {
		t.Fatalf("kept-public project: %d", c)
	}
	// both decided: the switch is gone, and a new project brings it back
	br.c.CheckRedirect = nil
	if _, p := br.get("/@alice"); strings.Contains(p, "New on your profile") {
		t.Fatal("hide switch still shown after both projects were decided")
	}
	if code, out := d.sync(chain.Checkpoint{Seq: 2, Hash: h(2)}, []ledger.Bucket{{Project: "github.com/alice/third", Start: minute(1), CodingSeconds: 30}}, nil); code != 200 {
		t.Fatalf("sync: %d %s", code, out)
	}
	if _, p := br.get("/@alice"); !strings.Contains(p, "github.com/alice/third") || strings.Count(p, "data-visibility") != 1 {
		t.Fatal("a new project did not bring the switch back, or a decided one is still in it")
	}
}

func mustUser(t *testing.T, e *tenv, name string) int64 {
	t.Helper()
	u, err := e.st.UserByName(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
