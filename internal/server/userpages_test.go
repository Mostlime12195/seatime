package server

import (
	"strings"
	"testing"
	"time"

	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/ledger"
)

// User pages show counted time only, with a range picker, a project list
// that opens each project, and a small overview on the home page.
func TestUserPagesCountedOnlyWithRanges(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("mo")
	if _, home := br.get("/"); !strings.Contains(home, "Your week") || !strings.Contains(home, "No time yet") {
		t.Fatal("home lacks the empty overview")
	}
	m := minute(30)
	var bs []ledger.Bucket
	for i := 0; i < 20; i++ {
		h := []string{"editor:vscode"}
		if i%2 == 1 {
			h = []string{"claude-code"}
		}
		bs = append(bs, ledger.Bucket{Project: "github.com/mo/app", Start: m.Add(time.Duration(i) * time.Minute), CodingSeconds: 50, AINotCountedSeconds: 10, Harnesses: h})
	}
	meta := func(r *api.SyncReq) {
		r.Projects = []api.ProjectMeta{{Key: "github.com/mo/app", Name: "app", Remote: "https://github.com/mo/app"}}
	}
	if code, out := d.sync(chain.Checkpoint{Seq: 5, Hash: h(5)}, bs, meta); code != 200 {
		t.Fatal(code, out)
	}
	for _, path := range []string{"/", "/@mo", "/@mo?range=30d", "/@mo?range=all", "/@mo/app", "/@mo/app?range=today", "/@mo/app?range=year"} {
		code, page := br.get(path)
		if code != 200 {
			t.Fatalf("%s: %d", path, code)
		}
		for _, bad := range []string{"present", "Present"} {
			if strings.Contains(page, bad) {
				t.Errorf("%s shows %q", path, bad)
			}
		}
	}
	_, prof := br.get("/@mo?range=30d")
	if !strings.Contains(prof, `href="/@mo/app?range=30d"`) || !strings.Contains(prof, `aria-current="true">Last 30 days`) {
		t.Error("profile: project link or range picker missing")
	}
	_, proj := br.get("/@mo/app")
	if !strings.Contains(proj, "By hand and with AI") || !strings.Contains(proj, "vscode") || !strings.Contains(proj, "claude-code") {
		t.Error("project page lacks the tool split")
	}
	if _, home := br.get("/"); !strings.Contains(home, `href="/@mo/app"`) {
		t.Error("home overview lacks the top project")
	}
}
