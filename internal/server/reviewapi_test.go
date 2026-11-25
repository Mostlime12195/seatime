package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/event"
)

// The review API hands a review tool the checked records of a project,
// and nothing that says what was typed.
func TestReviewEvidence(t *testing.T) {
	e := newEnv(t)
	d, _ := e.enroll("lena")
	evs, heads := localChain(t, "local:myapp")
	if code, out := d.sync(chain.Checkpoint{Seq: int64(len(evs)), Hash: heads[len(heads)-1]}, nil, nil); code != 200 {
		t.Fatal(code, out)
	}
	if code, out := e.apiDo("POST", "/api/v1/segments", d.token, d.localBundle(evs, "local:myapp")); code != 200 {
		t.Fatal(code, string(out))
	}
	key := api.HashedProjectKey("local:myapp")
	path := "/api/v1/review/users/lena/projects/" + key

	// With no key configured the API does not exist; with one, only that key opens it.
	if code, _ := e.apiDo("GET", path, strings.Repeat("k", 40), nil); code != 404 {
		t.Fatalf("no keys configured: %d", code)
	}
	good := strings.Repeat("k", 40)
	e.srv.cfg.ReviewKeys = []string{strings.Repeat("j", 40), good}
	for name, token := range map[string]string{"no key": "", "wrong key": strings.Repeat("x", 40), "a device's token": d.token} {
		if code, out := e.apiDo("GET", path, token, nil); code != 401 || strings.Contains(string(out), "records") {
			t.Errorf("%s: %d", name, code)
		}
	}
	code, raw := e.apiDo("GET", path, good, nil)
	if code != 200 {
		t.Fatalf("with the key: %d %s", code, raw)
	}
	var ev api.ReviewEvidence
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.User != "lena" || ev.Project != key || ev.Status != api.StatusVerified || ev.HumanSeconds < 60 {
		t.Errorf("summary: %+v", ev)
	}
	if len(ev.Segments) != 1 || len(ev.Devices) != 1 || ev.Devices[0].OS != "linux" {
		t.Fatalf("%d segments, devices %+v", len(ev.Segments), ev.Devices)
	}
	sg := ev.Segments[0]
	if sg.Status != api.StatusVerified || sg.Device != 1 || len(sg.Records) != len(evs) || len(sg.Minutes) == 0 {
		t.Fatalf("segment: status %s, %d records of %d, %d minutes", sg.Status, len(sg.Records), len(evs), len(sg.Minutes))
	}
	// The minutes add up to the hours applications read.
	sum := 0.0
	for _, m := range sg.Minutes {
		sum += m[1]
	}
	if int(sum+0.5) != int(ev.HumanSeconds+0.5) {
		t.Errorf("minutes add up to %.0f s, the summary says %.0f s", sum, ev.HumanSeconds)
	}
	kinds := map[event.Kind]int{}
	for i, r := range sg.Records {
		kinds[r.Kind]++
		if !r.TS.Equal(evs[i].TS) || r.Session != 1 || r.Harness != "claude-code" {
			t.Fatalf("record %d: %+v", i, r)
		}
	}
	if kinds[event.Prompt] != 8 || kinds[event.AgentStop] != 8 || kinds[event.Run] != 8 || kinds[event.Commit] != 1 {
		t.Errorf("kinds: %v", kinds)
	}
	if p := sg.Records[0]; p.Words != 15 || p.Check != nil {
		t.Errorf("first prompt: %+v", p)
	}
	if r := sg.Records[14]; r.Kind != event.Run || r.ExitCode == nil || *r.ExitCode != 1 || r.RunKind != event.RunTest {
		t.Errorf("the failed test run: %+v", r)
	}
	if c := sg.Records[len(sg.Records)-1]; c.SHA != strings.Repeat("c", 40) || c.Insertions != 40 {
		t.Errorf("commit: %+v", c)
	}

	// Nothing that could say what was typed, or where: no commitment to a
	// prompt's text, no text hash, no raw session id, none of the private fields.
	body := string(raw)
	for i := 0; i < 8; i++ {
		if strings.Contains(body, chain.Commit(promptText(i))) || strings.Contains(body, fmt.Sprintf("%064x", 1000+i)) {
			t.Fatalf("prompt %d's hash is in the answer", i)
		}
	}
	for _, field := range []string{`"text"`, `"text_hash"`, `"check_hash"`, `"command"`, `"cwd"`, `"files"`, `"extra"`, `"session_id"`, `"s1"`, d.id} {
		if strings.Contains(body, field) {
			t.Errorf("the answer contains %s", field)
		}
	}

	// The account's own page lists the project with the same hours.
	if code, _ := e.apiDo("GET", "/api/v1/review/users/lena", d.token, nil); code != 401 {
		t.Errorf("the project list with a device's token: %d", code)
	}
	code, rawUser := e.apiDo("GET", "/api/v1/review/users/lena", good, nil)
	var ru api.ReviewUser
	if err := json.Unmarshal(rawUser, &ru); err != nil || code != 200 {
		t.Fatal(code, err, string(rawUser))
	}
	if ru.User != "lena" || len(ru.Projects) != 1 || ru.Projects[0].Key != key || ru.Projects[0].Status != api.StatusVerified ||
		ru.Projects[0].HumanSeconds != ev.HumanSeconds || !ru.Projects[0].FirstTS.Equal(sg.FirstTS) || !ru.Projects[0].LastTS.Equal(sg.LastTS) {
		t.Errorf("project list: %+v", ru)
	}
	if code, _ := e.apiDo("GET", "/api/v1/review/users/nobody", good, nil); code != 404 {
		t.Errorf("unknown user's project list: %d", code)
	}

	// Search finds the account by a part of its name, whatever the case, and takes % and _ as plain characters.
	e.enroll("helena_2")
	for q, want := range map[string]int{"LEN": 2, "lena": 2, "na_2": 1, "%": 0, "l": 0, "zzz": 0} {
		code, raw := e.apiDo("GET", "/api/v1/review/users?q="+url.QueryEscape(q), good, nil)
		var got api.ReviewUsers
		if err := json.Unmarshal(raw, &got); err != nil || code != 200 || len(got.Users) != want {
			t.Errorf("search %q: %d %s", q, code, raw)
		}
	}
	if code, _ := e.apiDo("GET", "/api/v1/review/users?q=lena", d.token, nil); code != 401 {
		t.Errorf("search with a device's token: %d", code)
	}

	// A range with no work in it has no segments.
	if _, raw := e.apiDo("GET", path+"?until=2020-01-01T00:00:00Z", good, nil); !strings.Contains(string(raw), `"segments":[]`) {
		t.Errorf("an old range: %s", raw)
	}
	if code, _ := e.apiDo("GET", path+"?since=yesterday", good, nil); code != 400 {
		t.Errorf("bad since: %d", code)
	}
	// Unknown people and projects, and a project its owner hid.
	if code, _ := e.apiDo("GET", "/api/v1/review/users/nobody/projects/"+key, good, nil); code != 404 {
		t.Errorf("unknown user: %d", code)
	}
	if code, _ := e.apiDo("GET", "/api/v1/review/users/lena/projects/nope", good, nil); code != 404 {
		t.Errorf("unknown project: %d", code)
	}
	ctx := context.Background()
	u, _ := e.st.UserByName(ctx, "lena")
	ps, _ := e.st.ListProjects(ctx, u.ID)
	if err := e.st.SetProjectHidden(ctx, u.ID, ps[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.apiDo("GET", path, good, nil); code != 404 {
		t.Errorf("a hidden project: %d", code)
	}
	if _, raw := e.apiDo("GET", "/api/v1/review/users/lena", good, nil); !strings.Contains(string(raw), `"projects":[]`) {
		t.Errorf("a hidden project is in the list: %s", raw)
	}
}
