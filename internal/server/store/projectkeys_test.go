package store

import (
	"context"
	"math"
	"testing"
	"time"

	"seatime/internal/server/store/storetest"
	"seatime/pkg/api"
	"seatime/pkg/ledger"
)

// The one rule for which project a device's key belongs to.
func TestDeviceProject(t *testing.T) {
	s, ctx := open(t), context.Background()
	u, _ := s.LoginIdentity(ctx, Identity{"dev", "k1"}, "kim", "", "", "", "")
	other, _ := s.LoginIdentity(ctx, Identity{"dev", "k2"}, "lee", "", "", "", "")
	dp := func(uid int64, dev string, m api.ProjectMeta) *Project {
		t.Helper()
		p, err := s.DeviceProject(ctx, uid, dev, m)
		if err != nil {
			t.Fatalf("%+v: %v", m, err)
		}
		return p
	}
	local := api.HashedProjectKey("local:ysws-cupid")
	const first, second = "github.com/kim/cupid", "github.com/hackclub/cupid"

	// a folder with no remote
	p := dp(u.ID, "laptop", api.ProjectMeta{Key: local, Name: "ysws-cupid", Folder: true})
	if p.Key != local || p.Slug != "ysws-cupid" || p.Name != "ysws-cupid" || p.Repo != "" {
		t.Fatalf("new project: %+v", p)
	}
	// it gets a remote: the same project, which now goes by the remote
	q := dp(u.ID, "laptop", api.ProjectMeta{Key: local, Label: first, Name: "cupid", Remote: "https://" + first})
	if q.ID != p.ID || q.Key != first || q.Repo != "kim/cupid" || q.Name != "cupid" || q.Slug != "ysws-cupid" {
		t.Fatalf("after a remote was added: %+v", q)
	}
	// the repo moves to an org: still the same project
	q = dp(u.ID, "laptop", api.ProjectMeta{Key: local, Label: second, Name: "cupid", Remote: "https://" + second})
	if q.ID != p.ID || q.Key != second || q.Repo != "hackclub/cupid" || q.Slug != "ysws-cupid" {
		t.Fatalf("after the remote changed: %+v", q)
	}
	// a repeat of the same meta changes nothing
	if q = dp(u.ID, "laptop", api.ProjectMeta{Key: local, Label: second, Name: "cupid", Remote: "https://" + second}); q.ID != p.ID || q.Key != second {
		t.Fatalf("repeat: %+v", q)
	}
	// records under the device's key, with no label (a segment): the same project
	if q = dp(u.ID, "laptop", api.ProjectMeta{Key: local, Label: second}); q.ID != p.ID {
		t.Fatalf("the key alone: %+v", q)
	}
	// the same repo cloned on a second computer joins it
	if q = dp(u.ID, "desktop", api.ProjectMeta{Key: second, Name: "cupid", Remote: "https://" + second}); q.ID != p.ID {
		t.Fatalf("second computer: %+v", q)
	}
	// a key the project went by before is not the project's any more: a
	// folder that shows up with it (on a third computer) is its own project
	stale := dp(u.ID, "old-pc", api.ProjectMeta{Key: first, Name: "cupid", Remote: "https://" + first})
	if stale.ID == p.ID || stale.Key != first {
		t.Fatalf("an earlier key took a new folder into the project: %+v", stale)
	}
	// and so is a folder with the first folder's old name
	if q = dp(u.ID, "old-pc", api.ProjectMeta{Key: local, Name: "ysws-cupid", Folder: true}); q.ID == p.ID || q.Key != local {
		t.Fatalf("an earlier local key took a new folder into the project: %+v", q)
	}
	// the laptop's own key stays the project's for good
	if q = dp(u.ID, "laptop", api.ProjectMeta{Key: local, Label: second}); q.ID != p.ID {
		t.Fatalf("the device's key moved: %+v", q)
	}

	// another folder, with its own time, is pointed at the project's
	// remote: both stay as they are, and nothing of the project changes
	hw := dp(u.ID, "laptop", api.ProjectMeta{Key: api.HashedProjectKey("local:homework"), Name: "homework", Folder: true})
	hw2 := dp(u.ID, "laptop", api.ProjectMeta{Key: hw.Key, Label: second, Name: "cupid", Remote: "https://" + second})
	if hw2.ID != hw.ID || hw2.ID == p.ID || hw2.Key != hw.Key || hw2.Name != "homework" || hw2.Repo != "" {
		t.Fatalf("a folder pointed at another project's remote: %+v", hw2)
	}
	if q, _ = s.ProjectByID(ctx, p.ID); q.Key != second || q.Name != "cupid" {
		t.Fatalf("the project changed: %+v", q)
	}

	// two computers with different remotes for one project: the one that
	// did not change does not pull the project back
	const third = "github.com/hackclub/cupid-2"
	dp(u.ID, "laptop", api.ProjectMeta{Key: local, Label: third, Name: "cupid-2", Remote: "https://" + third})
	q = dp(u.ID, "desktop", api.ProjectMeta{Key: second, Name: "cupid", Remote: "https://" + second})
	if q.ID != p.ID || q.Key != third || q.Name != "cupid-2" || q.Repo != "hackclub/cupid-2" {
		t.Fatalf("the computer whose remote did not change renamed the project: %+v", q)
	}

	// an older device sends no label and changes its key with the remote:
	// two projects, as before
	a := dp(u.ID, "old", api.ProjectMeta{Key: api.HashedProjectKey("local:game")})
	b := dp(u.ID, "old", api.ProjectMeta{Key: "github.com/kim/game", Remote: "https://github.com/kim/game"})
	if a.ID == b.ID {
		t.Fatal("an older device's two keys were one project")
	}

	// another account's keys are its own
	if q = dp(other.ID, "x", api.ProjectMeta{Key: local, Label: third}); q.UserID != other.ID || q.ID == p.ID {
		t.Fatalf("another account: %+v", q)
	}
	if q = dp(other.ID, "x", api.ProjectMeta{Key: third}); q.UserID != other.ID {
		t.Fatalf("another account: %+v", q)
	}
	// the laptop's local database is lost: its folder starts again under
	// what it goes by now, which is the project
	if q = dp(u.ID, "laptop", api.ProjectMeta{Key: third, Label: third, Name: "cupid-2", Remote: "https://" + third}); q.ID != p.ID {
		t.Fatalf("after the device's database was lost: %+v", q)
	}
	ks, err := s.ProjectKeys(ctx, p.ID)
	if err != nil || len(ks) != 3 {
		t.Fatalf("keys of the project: %+v %v", ks, err)
	}
}

// Migration 8 on a database an older device split: one folder that went by
// three keys is three projects holding the same commits. They fold into
// the newest, with their time, checked segments and commits.
func TestFoldSplitProjects(t *testing.T) {
	dsn := storetest.DSN(t)
	all := migrations
	migrations = all[:7]
	s, err := Open(dsn)
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u, _ := s.LoginIdentity(ctx, Identity{"dev", "f1"}, "lia", "", "", "", "")
	v, _ := s.LoginIdentity(ctx, Identity{"dev", "f2"}, "max", "", "", "", "")
	for _, d := range []Device{{ID: "mac", UserID: u.ID, Name: "mac", PublicKey: "k"}, {ID: "pc", UserID: v.ID, Name: "pc", PublicKey: "k"}} {
		if err := s.CreateDevice(ctx, d, "tok-"+d.ID); err != nil {
			t.Fatal(err)
		}
	}
	local := api.HashedProjectKey("local:ysws-cupid")
	keys := []string{local, "github.com/lia/cupid", "github.com/hackclub/cupid"}
	t0 := time.Now().UTC().Truncate(time.Hour).Add(-6 * time.Hour)
	shas := []string{"aaaaaaa1", "aaaaaaa2", "aaaaaaa3", "aaaaaaa4", "aaaaaaa5"}
	var ids []int64
	var wantSeconds, wantChecked float64
	for i, k := range keys {
		m := api.ProjectMeta{Key: k}
		if i == 0 {
			m.Name, m.Folder = "ysws-cupid", true
		} else {
			m.Name, m.Remote = "cupid", "https://"+k
		}
		p, err := s.UpsertProject(ctx, u.ID, m)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
		// ten minutes each, an hour apart, and one minute all three share
		// (20 s each: the folder's key changed inside it)
		var bs []ledger.Bucket
		for j := 0; j < 10; j++ {
			bs = append(bs, ledger.Bucket{Project: k, Start: t0.Add(time.Duration(i)*time.Hour + time.Duration(j)*time.Minute),
				CodingSeconds: 20, AISeconds: 30, Prompts: 1, Models: []ledger.ModelUse{{Model: "m-1", Seconds: 30, Prompts: 1}}})
			wantSeconds += 50
		}
		bs = append(bs, ledger.Bucket{Project: k, Start: t0.Add(4 * time.Hour), CodingSeconds: 20, Commits: 1, Harnesses: []string{"editor:vscode"}})
		if err := s.UpsertBuckets(ctx, u.ID, "mac", bs); err != nil {
			t.Fatal(err)
		}
		// every commit under every key, as the device's scan recorded them
		var cs []Commit
		for _, sha := range shas {
			cs = append(cs, Commit{ProjectID: p.ID, SHA: sha, TS: t0.Add(time.Duration(i) * time.Hour), Ins: 3})
		}
		if err := s.UpsertCommits(ctx, cs); err != nil {
			t.Fatal(err)
		}
		// a checked segment of its ten minutes
		sg := &Segment{ID: "seg" + string(rune('a'+i)), UserID: u.ID, ProjectID: p.ID, DeviceID: "mac", Status: SegVerified,
			SeqLo: int64(i*100 + 1), SeqHi: int64(i*100 + 50), FirstTS: t0.Add(time.Duration(i) * time.Hour), LastTS: t0.Add(time.Duration(i)*time.Hour + 9*time.Minute),
			Created: t0.Add(time.Duration(i+1) * time.Hour), Checked: t0.Add(time.Duration(i+1) * time.Hour)}
		var mins []SegmentMinute
		for j := 0; j < 10; j++ {
			mins = append(mins, SegmentMinute{StartMin: sg.FirstTS.Unix()/60 + int64(j), Human: 50, Hand: 20, Prompts: 1, Models: []ledger.ModelUse{{Model: "m-1", Seconds: 30, Prompts: 1}}})
			wantChecked += 50
		}
		if err := s.PutSegment(ctx, sg, []byte("gz"), mins); err != nil {
			t.Fatal(err)
		}
	}
	wantSeconds += 60 // the shared minute: 3 x 20 s on one device
	// a prompt of the first key that was closed, and time waiting under it
	if _, err := s.db.ExecContext(ctx, `INSERT INTO prompt_chain(device_id,seq,ts,project_key,commit_hash,rep,link,received,status) VALUES ('mac',1,?,?,'h',0,'l',?,'mismatch')`,
		t0.UnixMilli(), local, t0.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPending(ctx, u.ID, "mac", map[string]float64{local: 120}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// a project of their own with its own commit, and another account's
	// project with one of the same commits: neither folds
	own, _ := s.UpsertProject(ctx, u.ID, api.ProjectMeta{Key: "github.com/lia/other"})
	s.UpsertCommits(ctx, []Commit{{ProjectID: own.ID, SHA: "bbbbbbb1", TS: t0}})
	s.UpsertBuckets(ctx, u.ID, "mac", []ledger.Bucket{{Project: own.Key, Start: t0.Add(5 * time.Hour), CodingSeconds: 40}})
	theirs, _ := s.UpsertProject(ctx, v.ID, api.ProjectMeta{Key: "github.com/hackclub/cupid"})
	s.UpsertCommits(ctx, []Commit{{ProjectID: theirs.ID, SHA: shas[0], TS: t0}})
	s.UpsertBuckets(ctx, v.ID, "pc", []ledger.Bucket{{Project: theirs.Key, Start: t0, CodingSeconds: 30}})
	s.Close()

	if s, err = Open(dsn); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	ps, err := s.ListProjects(ctx, u.ID)
	if err != nil || len(ps) != 2 {
		t.Fatalf("projects after the upgrade: %+v %v", ps, err)
	}
	p, err := s.ProjectByKey(ctx, u.ID, "github.com/hackclub/cupid")
	if err != nil || p.ID != ids[2] || p.Slug != "cupid" || p.Name != "cupid" || p.Repo != "hackclub/cupid" {
		t.Fatalf("the folded project: %+v %v", p, err)
	}
	// every link that led to one of the three leads to it
	for _, slug := range []string{"cupid", "cupid-2", "ysws-cupid"} {
		if q, err := s.ProjectBySlug(ctx, u.ID, slug); err != nil || q.ID != p.ID {
			t.Errorf("slug %s: %+v %v", slug, q, err)
		}
	}
	// each commit once
	if cs, _ := s.Commits(ctx, p.ID, t0.Add(-time.Hour), t0.Add(24*time.Hour)); len(cs) != len(shas) {
		t.Errorf("commits: %d, want %d", len(cs), len(shas))
	}
	// all the time, the shared minute as one minute, commits counted once
	sums, _ := s.ProjectSums(ctx, u.ID, t0.Add(-time.Hour), t0.Add(24*time.Hour))
	var got float64
	for _, x := range sums {
		if x.Project.ID == p.ID {
			got = x.Struggle
		}
	}
	if math.Abs(got-wantSeconds) > 1e-6 {
		t.Errorf("counted time: %.0f s, want %.0f", got, wantSeconds)
	}
	bs, _ := s.Buckets(ctx, u.ID, p.ID, t0.Add(4*time.Hour), t0.Add(4*time.Hour+time.Minute))
	if len(bs) != 1 || bs[0].Seconds() != 60 || bs[0].Commits != 1 {
		t.Errorf("the minute the three shared: %+v", bs)
	}
	if total := rollupAgrees(t, s, u.ID); math.Abs(total-wantSeconds-40) > 1e-6 {
		t.Errorf("hourly in all: %.0f s, want %.0f", total, wantSeconds+40)
	}
	g, _ := s.Global(ctx, time.Now())
	if math.Abs(g.AllStruggle-wantSeconds-40-30) > 1e-6 {
		t.Errorf("all counted time: %.0f s, want %.0f", g.AllStruggle, wantSeconds+70)
	}
	// checked time stays checked
	segs, _ := s.ListSegments(ctx, u.ID, p.ID)
	for _, sg := range segs {
		if sg.Status != SegVerified {
			t.Errorf("segment %s: %s", sg.ID, sg.Status)
		}
	}
	mins, _ := s.VerifiedMinutes(ctx, p.ID, t0.Add(-time.Hour), t0.Add(24*time.Hour))
	var checked float64
	for _, m := range mins {
		checked += m.Human
	}
	if len(segs) != 3 || math.Abs(checked-wantChecked) > 1e-6 {
		t.Errorf("checked: %d segments, %.0f s; want 3 and %.0f", len(segs), checked, wantChecked)
	}
	ms, _ := s.VerifiedModels(ctx, p.ID, t0.Add(-time.Hour), t0.Add(24*time.Hour))
	var bymodel float64
	for _, m := range ms {
		bymodel += m.Seconds
	}
	if math.Abs(bymodel-wantChecked) > 1e-6 {
		t.Errorf("checked time by model: %.0f s, want %.0f", bymodel, wantChecked)
	}
	// records under any of the three keys are the project's
	for _, k := range keys {
		if q, err := s.DeviceProject(ctx, u.ID, "mac", api.ProjectMeta{Key: k}); err != nil || q.ID != p.ID || q.Key != "github.com/hackclub/cupid" || q.Name != "cupid" {
			t.Errorf("key %s: %+v %v", k, q, err)
		}
	}
	if n, _ := s.ClosedPromptCount(ctx, p.ID); n[ChainMismatch] != 1 {
		t.Errorf("closed prompts: %+v", n)
	}
	if sec, _ := s.Pending(ctx, u.ID, p.ID, time.Now().Add(-time.Hour)); sec != 120 {
		t.Errorf("waiting for the check: %.0f s, want 120", sec)
	}
	// the others are as they were
	if q, err := s.ProjectByID(ctx, own.ID); err != nil || q.Key != own.Key {
		t.Errorf("their other project: %+v %v", q, err)
	}
	if q, err := s.ProjectByID(ctx, theirs.ID); err != nil || q.UserID != v.ID {
		t.Errorf("another account's project: %+v %v", q, err)
	}
	if cs, _ := s.Commits(ctx, theirs.ID, t0.Add(-time.Hour), t0.Add(time.Hour)); len(cs) != 1 {
		t.Errorf("another account's commits: %+v", cs)
	}
}
