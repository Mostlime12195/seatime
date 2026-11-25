package server

import (
	"strings"
	"testing"

	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/ledger"
)

// The server does not ask GitHub about commits. The project page gives the
// viewer's browser what it needs to ask (the repo, each SHA, the script and
// leave to reach api.github.com), and the API says nothing about GitHub.
func TestCommitsAreCheckedByTheBrowser(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("bob")
	sha := strings.Repeat("a", 40)
	bs := []ledger.Bucket{
		{Project: "github.com/bob/app", Start: minute(3), CodingSeconds: 45},
		{Project: "notes", Start: minute(2), CodingSeconds: 45},
	}
	if code, out := d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, bs, func(r *api.SyncReq) {
		r.Projects = []api.ProjectMeta{{Key: "github.com/bob/app", Name: "app", Remote: "https://github.com/bob/app"}}
		r.Commits = []api.CommitRef{
			{Project: "github.com/bob/app", SHA: sha, TS: minute(3), Ins: 3, Del: 1},
			{Project: "notes", SHA: strings.Repeat("b", 40), TS: minute(2)},
		}
	}); code != 200 {
		t.Fatalf("sync: %d %s", code, out)
	}

	resp, err := br.c.Get(e.ts.URL + "/@bob/app")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self' https://api.github.com;") {
		t.Fatalf("the page may not reach api.github.com: %s", csp)
	}
	_, page := br.get("/@bob/app")
	for _, want := range []string{`data-repo="bob/app"`, `data-sha="` + sha + `"`, `<script src="/static/commits.js" defer></script>`} {
		if !strings.Contains(page, want) {
			t.Fatalf("project page lacks %s:\n%s", want, page)
		}
	}
	if strings.Contains(page, `class="seal ok"`) || strings.Contains(page, "<script>") {
		t.Fatalf("the server wrote a badge or an inline script:\n%s", page)
	}
	if _, js := br.get("/static/commits.js"); !strings.Contains(js, "https://api.github.com/repos/") {
		t.Fatal("commits.js is not served")
	}

	// no linked repo: the server says so, and the browser has nothing to ask
	_, page = br.get("/@bob/notes")
	if !strings.Contains(page, "no repo linked") || strings.Contains(page, "data-repo") || strings.Contains(page, "commits.js") {
		t.Fatalf("project without a repo:\n%s", page)
	}

	code, out := e.apiDo("GET", "/api/v1/users/bob/projects/app/stats", "", nil)
	if code != 200 || !strings.Contains(string(out), sha) || strings.Contains(string(out), "on_github") {
		t.Fatalf("stats: %d %s", code, out)
	}
}
