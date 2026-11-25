package store

import (
	"context"
	"errors"
	"seatime/internal/server/store/storetest"
	"strings"
	"testing"
	"time"

	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/ledger"
)

func open(t *testing.T) *DB {
	t.Helper()
	s, err := Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUsersUniqueNames(t *testing.T) {
	s, ctx := open(t), context.Background()
	a, err := s.LoginIdentity(ctx, Identity{"dev", "1"}, "Alice!", "", "", "", "")
	if err != nil || a.Username != "alice" || !a.Public {
		t.Fatalf("%+v %v", a, err)
	}
	b, _ := s.LoginIdentity(ctx, Identity{"dev", "2"}, "alice", "", "", "", "")
	if b.Username != "alice-2" {
		t.Fatal(b.Username)
	}
	again, _ := s.LoginIdentity(ctx, Identity{"dev", "1"}, "other", "", "", "", "")
	if again.ID != a.ID {
		t.Fatal("identity should resolve to same user")
	}
}

func TestBucketUpsertIdempotentAndRollup(t *testing.T) {
	s, ctx := open(t), context.Background()
	u, _ := s.LoginIdentity(ctx, Identity{"dev", "1"}, "bob", "", "", "", "")
	start := time.Now().UTC().Truncate(time.Minute)
	b := ledger.Bucket{Project: "github.com/bob/x", Start: start, CodingSeconds: 30, AINotCountedSeconds: 10, Prompts: 1}
	for i := 0; i < 3; i++ {
		if err := s.UpsertBuckets(ctx, u.ID, "d1", []ledger.Bucket{b}); err != nil {
			t.Fatal(err)
		}
	}
	b.CodingSeconds, b.AINotCountedSeconds = 40, 0 // recompute changes the minute
	s.UpsertBuckets(ctx, u.ID, "d1", []ledger.Bucket{b})
	p, err := s.ProjectByKey(ctx, u.ID, b.Project)
	if err != nil || p.Repo != "bob/x" {
		t.Fatalf("%+v %v", p, err)
	}
	got, _ := s.Buckets(ctx, u.ID, p.ID, start.Add(-time.Hour), start.Add(time.Hour))
	if len(got) != 1 || got[0].Seconds() != 40 {
		t.Fatalf("%+v", got)
	}
	day := start.Unix() / 86400
	ds, _ := s.DaySums(ctx, u.ID, 0, day-1, day+1, 0)
	var tot float64
	for _, d := range ds {
		tot += d.Struggle
	}
	if tot != 40 {
		t.Fatalf("rollup drifted: %v", tot)
	}
	g, _ := s.Global(ctx, time.Now())
	if g.AllStruggle != 40 || g.ActiveNow != 1 {
		t.Fatalf("%+v", g)
	}
	lb, _ := s.Leaderboard(ctx, start.Add(-24*time.Hour), 10)
	if len(lb) != 1 || lb[0].Struggle != 40 {
		t.Fatalf("%+v", lb)
	}
}

func TestCheckpointAppendOnly(t *testing.T) {
	s, ctx := open(t), context.Background()
	now := time.Now()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.AddCheckpoint(ctx, "d", chain.Checkpoint{Seq: 10, Hash: "aa"}, now))
	must(s.AddCheckpoint(ctx, "d", chain.Checkpoint{Seq: 10, Hash: "aa"}, now)) // idempotent
	if err := s.AddCheckpoint(ctx, "d", chain.Checkpoint{Seq: 10, Hash: "bb"}, now); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := s.AddCheckpoint(ctx, "d", chain.Checkpoint{Seq: 5, Hash: "cc"}, now); !errors.Is(err, ErrRegression) {
		t.Fatal(err)
	}
	must(s.AddCheckpoint(ctx, "d", chain.Checkpoint{Seq: 20, Hash: "dd"}, now))
	cps, _ := s.Checkpoints(ctx, "d", 1, 15)
	if len(cps) != 1 || cps[0].Seq != 10 {
		t.Fatalf("%+v", cps)
	}
}

func TestDeviceCodeFlowAndCommits(t *testing.T) {
	s, ctx := open(t), context.Background()
	u, _ := s.LoginIdentity(ctx, Identity{"dev", "1"}, "c", "", "", "", "")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateDeviceCode(ctx, "hash", DeviceCode{UserCode: "ABCD-2345", DeviceName: "x", PublicKey: "k", Expires: time.Now().Add(time.Minute)}))
	dc, err := s.ConsumeDeviceCode(ctx, "hash")
	if err != nil || dc.UserID != 0 {
		t.Fatal(dc, err)
	}
	must(s.ApproveDeviceCode(ctx, "ABCD-2345", u.ID))
	dc, _ = s.ConsumeDeviceCode(ctx, "hash")
	if dc.UserID != u.ID {
		t.Fatal("not approved")
	}
	if _, err := s.ConsumeDeviceCode(ctx, "hash"); !errors.Is(err, ErrNotFound) {
		t.Fatal("code should be single use")
	}
	p, _ := s.UpsertProject(ctx, u.ID, api.ProjectMeta{Key: "github.com/c/abc", Name: "abc"})
	must(s.UpsertCommits(ctx, []Commit{{ProjectID: p.ID, SHA: "deadbeef", TS: time.Now()}}))
	cs, _ := s.Commits(ctx, p.ID, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if len(cs) != 1 {
		t.Fatal(cs)
	}
}

func TestTwoDevicesSameMinuteNotDoubleCounted(t *testing.T) {
	s, ctx := open(t), context.Background()
	u, _ := s.LoginIdentity(ctx, Identity{"dev", "9"}, "cat", "", "", "", "")
	start := time.Now().UTC().Truncate(time.Minute)
	b := ledger.Bucket{Project: "github.com/cat/x", Start: start, CodingSeconds: 30, Prompts: 1}
	s.UpsertBuckets(ctx, u.ID, "laptop", []ledger.Bucket{b})
	b.CodingSeconds, b.Prompts = 45, 2
	s.UpsertBuckets(ctx, u.ID, "desktop", []ledger.Bucket{b})
	b.CodingSeconds = 20 // laptop recompute lowers its own row; desktop still max
	s.UpsertBuckets(ctx, u.ID, "laptop", []ledger.Bucket{b})
	p, _ := s.ProjectByKey(ctx, u.ID, b.Project)
	got, _ := s.Buckets(ctx, u.ID, p.ID, start.Add(-time.Hour), start.Add(time.Hour))
	if len(got) != 1 || got[0].Seconds() != 45 || got[0].Prompts != 4 {
		t.Fatalf("%+v", got)
	}
	day := start.Unix() / 86400
	ds, _ := s.DaySums(ctx, u.ID, 0, day-1, day+1, 0)
	var tot float64
	for _, d := range ds {
		tot += d.Struggle
	}
	if tot != 45 {
		t.Fatalf("rollup %v, want 45", tot)
	}
}

func TestUserLookups(t *testing.T) {
	s, ctx := open(t), context.Background()
	a, _ := s.LoginIdentity(ctx, Identity{"hackclub", "ident!abc"}, "alice", "", "", "U0AAAAAAA", "")
	b, _ := s.LoginIdentity(ctx, Identity{"dev", "b"}, "bob", "", "", "", "")
	if u, err := s.UserBySlackID(ctx, "U0AAAAAAA"); err != nil || u.ID != a.ID {
		t.Fatalf("%+v %v", u, err)
	}
	if _, err := s.UserBySlackID(ctx, "u0aaaaaaa"); !errors.Is(err, ErrNotFound) {
		t.Fatal("slack ids are case-sensitive")
	}
	if _, err := s.UserBySlackID(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatal("empty slack id must not match users without one")
	}
	if u, err := s.UserByIdentity(ctx, "hackclub", "ident!abc"); err != nil || u.ID != a.ID {
		t.Fatalf("%+v %v", u, err)
	}
	if _, err := s.UserByIdentity(ctx, "hackclub", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// linking a new Hack Club Auth identity without a github login must succeed
	if err := s.LinkIdentity(ctx, b.ID, Identity{"hackclub", "ident!bob"}, ""); err != nil {
		t.Fatal(err)
	}
	if u, err := s.UserByIdentity(ctx, "hackclub", "ident!bob"); err != nil || u.ID != b.ID {
		t.Fatalf("%+v %v", u, err)
	}
	if err := s.LinkIdentity(ctx, a.ID, Identity{"hackclub", "ident!bob"}, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("identity owned by another user: %v", err)
	}
	// SetSlackID only fills an empty slot
	s.SetSlackID(ctx, b.ID, "U0BBBBBBB")
	s.SetSlackID(ctx, b.ID, "U0CCCCCCC")
	if u, _ := s.UserByID(ctx, b.ID); u.SlackID != "U0BBBBBBB" {
		t.Fatal(u.SlackID)
	}
}

// Two devices in one minute merge as MAX durations / SUM counts, the hourly
// rollup follows by deltas, and a resend of identical rows changes nothing.
func TestBucketsTwoDevicesAndUnchangedResend(t *testing.T) {
	s, ctx := open(t), context.Background()
	u, _ := s.LoginIdentity(ctx, Identity{"dev", "1"}, "eve", "", "", "", "")
	start := time.Now().UTC().Truncate(time.Minute)
	a := ledger.Bucket{Project: "github.com/eve/x", Start: start, CodingSeconds: 30, AINotCountedSeconds: 10, Prompts: 2}
	b := ledger.Bucket{Project: "github.com/eve/x", Start: start, CodingSeconds: 20, AINotCountedSeconds: 30, Prompts: 1}
	for i := 0; i < 2; i++ {
		if err := s.UpsertBuckets(ctx, u.ID, "d1", []ledger.Bucket{a}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertBuckets(ctx, u.ID, "d2", []ledger.Bucket{b}); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := s.ProjectByKey(ctx, u.ID, a.Project)
	got, _ := s.Buckets(ctx, u.ID, p.ID, start.Add(-time.Hour), start.Add(time.Hour))
	// MAX of the counted seconds (30, 20) and of the AI not counted (10, 30)
	if len(got) != 1 || got[0].Seconds() != 30 || got[0].AINotCountedSeconds != 30 || got[0].Prompts != 3 {
		t.Fatalf("merge: %+v", got)
	}
	g, _ := s.Global(ctx, time.Now())
	if g.AllStruggle != 30 {
		t.Fatalf("all-time rollup %v want 30", g.AllStruggle)
	}
	s.UpsertBuckets(ctx, u.ID, "d1", []ledger.Bucket{a}) // identical resend
	ds, _ := s.DaySums(ctx, u.ID, 0, start.Unix()/86400-1, start.Unix()/86400+1, 0)
	var st float64
	for _, d := range ds {
		st += d.Struggle
	}
	if st != 30 {
		t.Fatalf("resend drifted rollup: %v", st)
	}
}

func TestConcurrentSyncWriters(t *testing.T) {
	s, ctx := open(t), context.Background()
	start := time.Now().UTC().Truncate(time.Minute)
	done := make(chan error, 16)
	for w := 0; w < 16; w++ {
		go func(w int) {
			name := "user" + string(rune('a'+w))
			u, err := s.LoginIdentity(ctx, Identity{"dev", name}, name, "", "", "", "")
			if err != nil {
				done <- err
				return
			}
			for i := 0; i < 20; i++ {
				bs := []ledger.Bucket{{Project: "local:p", Start: start.Add(time.Duration(i) * time.Minute), CodingSeconds: 20, AINotCountedSeconds: 10}}
				if err := s.UpsertBuckets(ctx, u.ID, "d", bs); err != nil {
					done <- err
					return
				}
				if err := s.TouchDevice(ctx, "d", time.Now()); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}(w)
	}
	for w := 0; w < 16; w++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if g, _ := s.Global(ctx, time.Now()); g.AllStruggle != 16*20*20 {
		t.Fatalf("lost updates: %v", g.AllStruggle)
	}
}

// A database migrated by a newer binary is refused.
func TestOpenRefusesNewerSchema(t *testing.T) {
	path := storetest.DSN(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(context.Background(), `INSERT INTO schema_migrations(version) VALUES (?)`, len(migrations)+1); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(path); !errors.Is(err, ErrSchemaAhead) {
		t.Fatalf("Open on a newer schema: %v, want ErrSchemaAhead", err)
	}
}

// A project's name: a folder's name fills in for a project nobody has
// named (also one made before devices sent folder names), a chosen name
// replaces it, and neither a second computer's folder name nor any device
// undoes a chosen name or one set on the web. The slug never changes.
func TestProjectNames(t *testing.T) {
	s, ctx := open(t), context.Background()
	u, _ := s.LoginIdentity(ctx, Identity{"dev", "n1"}, "nia", "", "", "", "")
	key := api.HashedProjectKey("local:my app")
	up := func(m api.ProjectMeta) *Project {
		t.Helper()
		m.Key = key
		p, err := s.UpsertProject(ctx, u.ID, m)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// made by an older device: no name, shown by the key's hash
	old := up(api.ProjectMeta{})
	if old.Name != strings.TrimPrefix(key, "local:") || old.Named {
		t.Fatalf("unnamed project: %+v", old)
	}
	// the next sync brings the folder's name
	if p := up(api.ProjectMeta{Name: "my app", Folder: true}); p.Name != "my app" || p.Slug != old.Slug || p.ID != old.ID {
		t.Fatalf("folder name not picked up: %+v", p)
	}
	// a name chosen on a device replaces it; a folder name does not undo that
	if p := up(api.ProjectMeta{Name: "My App"}); p.Name != "My App" {
		t.Fatalf("chosen name: %+v", p)
	}
	if p := up(api.ProjectMeta{Name: "my app", Folder: true}); p.Name != "My App" {
		t.Fatalf("a folder name replaced a chosen one: %+v", p)
	}
	// the owner's name on the web wins over every device
	if err := s.SetProjectName(ctx, u.ID, old.ID, "Web Name"); err != nil {
		t.Fatal(err)
	}
	for _, m := range []api.ProjectMeta{{Name: "my app", Folder: true}, {Name: "Other"}, {}} {
		if p := up(m); p.Name != "Web Name" || !p.Named {
			t.Fatalf("%+v replaced the web name: %+v", m, p)
		}
	}
	// a new project takes its slug from the folder's name
	p, err := s.UpsertProject(ctx, u.ID, api.ProjectMeta{Key: api.HashedProjectKey("local:Space Game"), Name: "Space Game", Folder: true})
	if err != nil || p.Name != "Space Game" || p.Slug != "space-game" {
		t.Fatalf("new project: %+v %v", p, err)
	}
}
