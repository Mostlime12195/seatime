package sim

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	clientstore "seatime/internal/client/store"
	srvstore "seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/event"
)

// gitFolder makes root a git checkout whose origin is remote (none for "").
func gitFolder(d *Device, root, remote string) {
	_ = os.MkdirAll(filepath.Join(root, ".git"), 0o700)
	cfg := "[core]\n\tbare = false\n"
	if remote != "" {
		cfg += "[remote \"origin\"]\n\turl = " + remote + "\n"
	}
	_ = os.WriteFile(filepath.Join(root, ".git", "config"), []byte(cfg), 0o600)
	_ = os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600)
	d.Pipe.Res.Forget() // the next hook is a new process and reads the folder again
}

// handWork is an editor heartbeat a minute in root, for n minutes.
func handWork(d *Device, root string, n int) {
	for i := 0; i < n; i++ {
		d.W.Advance(time.Minute)
		d.Heartbeat(map[string]any{"entity": filepath.Join(root, "main.go"), "type": "file", "category": "coding",
			"user_agent": "wakatime/v1.90 vscode/1.85 vscode-wakatime/24.0"})
	}
}

type serverSide struct {
	t *testing.T
	w *World
	u *srvstore.User
}

func (s serverSide) projects() []srvstore.Project {
	s.t.Helper()
	ps, err := s.w.Store.ListProjects(context.Background(), s.u.ID)
	if err != nil {
		s.t.Fatal(err)
	}
	return ps
}

// counted is a project's synced seconds over all time.
func (s serverSide) counted(pid int64) float64 {
	s.t.Helper()
	sums, err := s.w.Store.ProjectSums(context.Background(), s.u.ID, time.Unix(0, 0), s.w.Now().Add(time.Hour))
	if err != nil {
		s.t.Fatal(err)
	}
	for _, x := range sums {
		if x.Project.ID == pid {
			return x.Struggle
		}
	}
	return 0
}

func (s serverSide) stats(project string) api.ProjectStats {
	s.t.Helper()
	var st api.ProjectStats
	if _, err := s.w.getJSON("/api/v1/users/"+s.u.Username+"/projects/"+project+"/stats", &st); err != nil {
		s.t.Fatal(err)
	}
	return st
}

// One folder is one project from its first minute: with no remote, with a
// remote added, with the repo moved to an org, and with the same repo on a
// second computer. Its time and commits are in one place, each commit
// once, and what the server checked stays checked.
func TestOneFolderOneProject(t *testing.T) {
	dir := sandbox(t)
	w, err := NewWorld(filepath.Join(dir, "w"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	d, err := w.NewDevice("lia", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	u, err := w.Store.UserByName(context.Background(), "lia")
	if err != nil {
		t.Fatal(err)
	}
	srv := serverSide{t, w, u}
	const local = "local:ysws-cupid"
	root := filepath.Join(d.Home, "code", "ysws-cupid")

	// no remote
	gitFolder(d, root, "")
	handWork(d, root, 12)
	d.Commit(root, time.Time{}, 5, 1)
	cut := w.Now().Add(time.Minute).Truncate(time.Minute)
	s1 := d.Submit(local, SubmitOpts{Until: cut})
	if s1.Err != "" || s1.Status != "verified" {
		t.Fatalf("first segment: %s %s %v", s1.Err, s1.Status, s1.Problems)
	}
	ps := srv.projects()
	if len(ps) != 1 || ps[0].Slug != "ysws-cupid" || ps[0].Name != "ysws-cupid" {
		t.Fatalf("with no remote: %+v", ps)
	}
	id := ps[0].ID
	checked1 := srv.stats("ysws-cupid").HumanSeconds
	if checked1 < 600 {
		t.Fatalf("checked after the first segment: %.0f s", checked1)
	}

	// a remote is added, then the repo moves to an org
	for _, step := range []struct{ remote, key, repo string }{
		{"https://github.com/lia/cupid.git", "github.com/lia/cupid", "lia/cupid"},
		{"git@github.com:hackclub/cupid.git", "github.com/hackclub/cupid", "hackclub/cupid"},
	} {
		w.Advance(3 * time.Minute)
		gitFolder(d, root, step.remote)
		handWork(d, root, 10)
		d.Commit(root, time.Time{}, 5, 1)
		if err := d.SyncNow(); err != nil {
			t.Fatal(err)
		}
		ps = srv.projects()
		if len(ps) != 1 || ps[0].ID != id || ps[0].Key != step.key || ps[0].Repo != step.repo || ps[0].Name != "cupid" || ps[0].Slug != "ysws-cupid" {
			t.Fatalf("with remote %s: %+v", step.remote, ps)
		}
	}
	// records from before the update, or from a process that had read the
	// folder before the remote changed, carry the remote as their key: the
	// same project, and the minute both keys share is one minute
	for i := 0; i < 4; i++ {
		w.Advance(20 * time.Second)
		d.Pipe.EmitEvent(event.Event{Kind: event.Edit, Harness: "editor:vscode", Project: "github.com/hackclub/cupid", Files: []string{"main.go"}})
		w.Advance(20 * time.Second)
		d.Heartbeat(map[string]any{"entity": filepath.Join(root, "main.go"), "type": "file", "category": "coding",
			"user_agent": "wakatime/v1.90 vscode/1.85 vscode-wakatime/24.0"})
	}
	if err := d.SyncNow(); err != nil {
		t.Fatal(err)
	}
	if ps = srv.projects(); len(ps) != 1 || ps[0].ID != id {
		t.Fatalf("after records under the remote's key: %+v", ps)
	}
	bs, _ := w.Store.Buckets(context.Background(), u.ID, id, time.Unix(0, 0), w.Now().Add(time.Hour))
	var perMinute float64
	for _, b := range bs {
		if b.Seconds() > 60.01 {
			t.Errorf("minute %s holds %.1f s", b.Start.Format("15:04"), b.Seconds())
		}
		perMinute += b.Seconds()
	}

	// the device's log holds one key for the folder's own records
	keys, _ := d.Store.ProjectKeys()
	if len(keys) != 2 || keys[local] == 0 {
		t.Errorf("keys in the device's log: %v", keys)
	}
	// the rest is checked: both keys' segments are the one project's
	s2 := d.Submit(local, SubmitOpts{Since: cut})
	s3 := d.Submit("github.com/hackclub/cupid", SubmitOpts{})
	for _, s := range []*SubResult{s2, s3} {
		if s.Err != "" || s.Status != "verified" {
			t.Fatalf("segment of %s: %s %s %v", s.Project, s.Err, s.Status, s.Problems)
		}
	}
	total := srv.counted(id)
	if math.Abs(total-perMinute) > 0.01 || total < 30*60 {
		t.Fatalf("counted time: %.0f s by the hour, %.0f s by the minute", total, perMinute)
	}
	// what applications read: one project, by its link, by the key it goes
	// by now and by the keys it went by before
	for _, name := range []string{"ysws-cupid", "github.com%2Fhackclub%2Fcupid", "github.com%2Flia%2Fcupid", "local:53f2021a88e1"} {
		st := srv.stats(name)
		if st.Project != "github.com/hackclub/cupid" || st.Status != api.StatusVerified || st.Commits != 3 || len(st.CommitList) != 3 ||
			math.Abs(st.HumanSeconds-total) > 1 || st.HumanSeconds < checked1 {
			t.Errorf("stats by %s: %+v (counted %.0f s)", name, st, total)
		}
	}
	x, err := w.Export("lia", "ysws-cupid")
	if err != nil || x.Commits != 3 || x.Status != api.StatusVerified || math.Abs(x.Hours-math.Round(total/360)/10) > 0.11 || x.CodeURL != "https://github.com/hackclub/cupid" {
		t.Errorf("export: %+v %v", x, err)
	}
	// each commit once on the device too
	cs, _ := d.Store.Events(clientstore.Query{Kinds: []event.Kind{event.Commit}})
	if len(cs) != 3 {
		t.Errorf("%d commit records on the device, want 3", len(cs))
	}

	// the same repo on a second computer is the same project
	d2, err := w.NewDevice("lia", "desktop")
	if err != nil {
		t.Fatal(err)
	}
	handWork(d2, d2.Project("hackclub", "cupid"), 6)
	if err := d2.SyncNow(); err != nil {
		t.Fatal(err)
	}
	if ps = srv.projects(); len(ps) != 1 || ps[0].ID != id {
		t.Fatalf("after the second computer: %+v", ps)
	}
	if more := srv.counted(id); more < total+240 {
		t.Errorf("the second computer's time: %.0f s after %.0f", more, total)
	}
}

// A folder with its own time that is pointed at another project's remote
// does not pour its time into that project, and another account's project
// with the same remote is never touched.
func TestRepointedFolderKeepsItsOwnTime(t *testing.T) {
	dir := sandbox(t)
	w, err := NewWorld(filepath.Join(dir, "w"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	d, err := w.NewDevice("lia", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	o, err := w.NewDevice("max", "pc")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u, _ := w.Store.UserByName(ctx, "lia")
	v, _ := w.Store.UserByName(ctx, "max")
	srv, theirs := serverSide{t, w, u}, serverSide{t, w, v}

	homework := filepath.Join(d.Home, "school", "homework")
	gitFolder(d, homework, "")
	handWork(d, homework, 25)
	tiny := d.Project("lia", "tiny")
	handWork(d, tiny, 5)
	handWork(o, o.Project("lia", "tiny"), 7)
	for _, x := range []*Device{d, o} {
		if err := x.SyncNow(); err != nil {
			t.Fatal(err)
		}
	}
	byKey := func(s serverSide) map[string]srvstore.Project {
		m := map[string]srvstore.Project{}
		for _, p := range s.projects() {
			m[p.Key] = p
		}
		return m
	}
	before := byKey(srv)
	hwKey := api.HashedProjectKey("local:homework")
	if len(before) != 2 || before[hwKey].ID == 0 || before["github.com/lia/tiny"].ID == 0 {
		t.Fatalf("before: %+v", before)
	}
	tinyBefore, hwBefore := srv.counted(before["github.com/lia/tiny"].ID), srv.counted(before[hwKey].ID)
	theirBefore := theirs.counted(byKey(theirs)["github.com/lia/tiny"].ID)
	if tinyBefore < 200 || hwBefore < 1200 || theirBefore < 300 {
		t.Fatalf("before: tiny %.0f s, homework %.0f s, the other account %.0f s", tinyBefore, hwBefore, theirBefore)
	}

	w.Advance(3 * time.Minute)
	gitFolder(d, homework, "https://github.com/lia/tiny.git")
	handWork(d, homework, 6)
	if err := d.SyncNow(); err != nil {
		t.Fatal(err)
	}
	after := byKey(srv)
	if len(after) != 2 || after[hwKey].ID != before[hwKey].ID || after[hwKey].Name != "homework" || after[hwKey].Repo != "" {
		t.Fatalf("after: %+v", after)
	}
	if got := srv.counted(after["github.com/lia/tiny"].ID); got != tinyBefore {
		t.Errorf("the other project's time changed: %.0f s, was %.0f", got, tinyBefore)
	}
	if got := srv.counted(after[hwKey].ID); got < hwBefore+240 {
		t.Errorf("the folder's own project: %.0f s, was %.0f", got, hwBefore)
	}
	if got := theirs.counted(byKey(theirs)["github.com/lia/tiny"].ID); got != theirBefore || len(theirs.projects()) != 1 {
		t.Errorf("another account's project changed: %.0f s, was %.0f", got, theirBefore)
	}
}

// The daemon uploads a project's records stretch by stretch and keeps its
// place per project key. A folder's key does not change with its remote,
// so the place is kept: nothing is sent twice and nothing starts again.
func TestSegmentsGoOnWhenTheRemoteChanges(t *testing.T) {
	dir := sandbox(t)
	w, err := NewWorld(filepath.Join(dir, "w"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	d, err := w.NewDevice("lia", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	d.Daemon.Auth = &d.Auth
	ctx := context.Background()
	u, _ := w.Store.UserByName(ctx, "lia")
	srv := serverSide{t, w, u}
	root := filepath.Join(d.Home, "code", "app")
	upload := func(want int) {
		t.Helper()
		w.Advance(3 * time.Hour) // the stretch is over and settled
		if err := d.SyncNow(); err != nil {
			t.Fatal(err)
		}
		for _, n := range []int{want, 0} { // and a second round sends nothing
			got, err := d.Daemon.SegmentsOnce(ctx, w.Now())
			if err != nil || got != n {
				t.Fatalf("segments sent: %d, want %d (%v)", got, n, err)
			}
		}
	}
	gitFolder(d, root, "")
	handWork(d, root, 10)
	upload(1)
	place, _ := d.Store.GetMeta("segment_until:local:app")
	gitFolder(d, root, "https://github.com/lia/app.git")
	handWork(d, root, 10)
	upload(1)
	gitFolder(d, root, "https://github.com/hackclub/app.git")
	handWork(d, root, 10)
	upload(1)

	ps := srv.projects()
	if len(ps) != 1 || ps[0].Key != "github.com/hackclub/app" {
		t.Fatalf("projects: %+v", ps)
	}
	segs, _ := w.Store.ListSegments(ctx, u.ID, ps[0].ID)
	if len(segs) != 3 {
		t.Fatalf("%d segments, want 3", len(segs))
	}
	for i, sg := range segs {
		if sg.Status != srvstore.SegVerified {
			t.Errorf("segment %d: %s %v", i, sg.Status, sg.Problems)
		}
		if i > 0 && !sg.FirstTS.After(segs[i-1].LastTS) {
			t.Errorf("segment %d starts at %s, inside the one before (to %s)", i, sg.FirstTS, segs[i-1].LastTS)
		}
	}
	st := srv.stats("app")
	if total := srv.counted(ps[0].ID); st.Status != api.StatusVerified || math.Abs(st.HumanSeconds-total) > 1 || total < 25*60 {
		t.Errorf("checked %.0f s of %.0f counted, %s", st.HumanSeconds, total, st.Status)
	}
	if now, _ := d.Store.GetMeta("segment_until:local:app"); place == "" || now <= place {
		t.Errorf("the upload's place: %q then %q", place, now)
	}
	for _, k := range []string{"github.com/lia/app", "github.com/hackclub/app"} {
		if v, _ := d.Store.GetMeta("segment_until:" + k); v != "" {
			t.Errorf("uploads started again under %s", k)
		}
	}
}

// `seatime send`: the records go up at once, without waiting for the
// stretch to be over, and work done afterwards goes in the next segment.
func TestSendNowUploadsTheStretchSoFar(t *testing.T) {
	dir := sandbox(t)
	w, err := NewWorld(filepath.Join(dir, "w"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	d, err := w.NewDevice("lia", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	d.Daemon.Auth = &d.Auth
	ctx := context.Background()
	u, _ := w.Store.UserByName(ctx, "lia")
	srv := serverSide{t, w, u}
	root := filepath.Join(d.Home, "code", "app")
	gitFolder(d, root, "")
	handWork(d, root, 10)
	w.Advance(2 * time.Minute) // nowhere near over
	if err := d.SyncNow(); err != nil {
		t.Fatal(err)
	}
	if n, err := d.Daemon.SegmentsOnce(ctx, w.Now()); err != nil || n != 0 {
		t.Fatalf("sent %d before the stretch was over (%v)", n, err)
	}
	if n, err := d.Daemon.SegmentsNow(ctx, w.Now()); err != nil || n != 1 {
		t.Fatalf("send now: %d (%v)", n, err)
	}
	if n, err := d.Daemon.SegmentsNow(ctx, w.Now()); err != nil || n != 0 {
		t.Fatalf("sent again: %d (%v)", n, err)
	}
	handWork(d, root, 10)
	w.Advance(3 * time.Hour)
	if err := d.SyncNow(); err != nil {
		t.Fatal(err)
	}
	if n, err := d.Daemon.SegmentsOnce(ctx, w.Now()); err != nil || n != 1 {
		t.Fatalf("the work after it: %d (%v)", n, err)
	}
	ps := srv.projects()
	segs, _ := w.Store.ListSegments(ctx, u.ID, ps[0].ID)
	if len(segs) != 2 {
		t.Fatalf("%d segments, want 2", len(segs))
	}
	for i, sg := range segs {
		if sg.Status != srvstore.SegVerified || sg.Scored.Seconds() < 5*60 {
			t.Errorf("segment %d: %s %v, %.0f s", i, sg.Status, sg.Problems, sg.Scored.Seconds())
		}
	}
}
