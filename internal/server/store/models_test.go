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

// rollupAgrees fails unless, for every project and hour, the rows of
// hourly_models add up to hourly's counted seconds and prompts.
func rollupAgrees(t *testing.T, s *DB, uid int64) (seconds float64) {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), `SELECT h.project_id, h.hour, h.struggle, h.prompts,
 COALESCE((SELECT SUM(m.seconds) FROM hourly_models m WHERE m.user_id=h.user_id AND m.project_id=h.project_id AND m.hour=h.hour),0),
 COALESCE((SELECT SUM(m.prompts) FROM hourly_models m WHERE m.user_id=h.user_id AND m.project_id=h.project_id AND m.hour=h.hour),0)
 FROM hourly h WHERE h.user_id=?`, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var pid, hour int64
		var struggle, msec float64
		var prompts, mprompts int
		if err := rows.Scan(&pid, &hour, &struggle, &prompts, &msec, &mprompts); err != nil {
			t.Fatal(err)
		}
		if math.Abs(struggle-msec) > 1e-6 || prompts != mprompts {
			t.Errorf("project %d hour %d: hourly %.3f s, %d prompts; by model %.3f s, %d prompts", pid, hour, struggle, prompts, msec, mprompts)
		}
		seconds += struggle
		n++
	}
	if n == 0 {
		t.Error("no hourly rows")
	}
	return seconds
}

func sumsOf(ms []ModelSum) map[string]ModelSum {
	out := map[string]ModelSum{}
	for _, m := range ms {
		k := m.Model
		if m.Hand {
			k = "hand"
		}
		out[k] = m
	}
	return out
}

// The model rollup follows every change to the minutes: two devices in one
// minute (the one that counted more decides the split, prompts add up), a
// minute sent again with other numbers, and a window replaced.
func TestModelRollupAddsUpToHourly(t *testing.T) {
	s, ctx := open(t), context.Background()
	u, _ := s.LoginIdentity(ctx, Identity{"dev", "1"}, "bob", "", "", "", "")
	m0 := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	at := func(i int) time.Time { return m0.Add(time.Duration(i) * time.Minute) }
	a := []ledger.Bucket{
		{Project: "p", Start: at(0), CodingSeconds: 60},
		{Project: "p", Start: at(1), CodingSeconds: 20, AISeconds: 30, Prompts: 2, Models: []ledger.ModelUse{{Model: "opus", Seconds: 30, Prompts: 1}}},
		{Project: "p", Start: at(2), AISeconds: 60, Prompts: 1, Models: []ledger.ModelUse{{Model: "opus", Seconds: 45}, {Model: "sonnet", Seconds: 15, Prompts: 1}}},
		{Project: "q", Start: at(2), AISeconds: 30, Prompts: 1},
		{Project: "p", Start: at(70), AISeconds: 50, Models: []ledger.ModelUse{{Model: "sonnet", Seconds: 50}}},
	}
	if err := s.UpsertBuckets(ctx, u.ID, "devA", a); err != nil {
		t.Fatal(err)
	}
	if got := rollupAgrees(t, s, u.ID); math.Abs(got-250) > 1e-6 {
		t.Fatalf("counted %.1f s", got)
	}
	// a second device in minute 1 counted more: its split replaces devA's, the prompts add up
	b := []ledger.Bucket{{Project: "p", Start: at(1), AISeconds: 55, Prompts: 1, Models: []ledger.ModelUse{{Model: "gpt", Seconds: 55, Prompts: 1}}}}
	if err := s.UpsertBuckets(ctx, u.ID, "devB", b); err != nil {
		t.Fatal(err)
	}
	rollupAgrees(t, s, u.ID)
	p, _ := s.ProjectByKey(ctx, u.ID, "p")
	ms, err := s.ModelSums(ctx, u.ID, p.ID, m0, m0.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := sumsOf(ms)
	for k, want := range map[string]ModelSum{"hand": {Seconds: 60}, "opus": {Seconds: 45, Prompts: 1}, "sonnet": {Seconds: 65, Prompts: 1},
		"gpt": {Seconds: 55, Prompts: 1}, "": {Seconds: 0, Prompts: 1}} {
		if math.Abs(got[k].Seconds-want.Seconds) > 1e-6 || got[k].Prompts != want.Prompts {
			t.Errorf("%q: %.1f s, %d prompts; want %.1f s, %d", k, got[k].Seconds, got[k].Prompts, want.Seconds, want.Prompts)
		}
	}
	// minute 2 sent again with less; then devA's first hour replaced by one minute
	a[2].AISeconds, a[2].Models = 20, []ledger.ModelUse{{Model: "opus", Seconds: 20}}
	if err := s.UpsertBuckets(ctx, u.ID, "devA", a[2:3]); err != nil {
		t.Fatal(err)
	}
	rollupAgrees(t, s, u.ID)
	if err := s.DeleteBuckets(ctx, u.ID, "devA", at(0), at(60), a[:1]); err != nil {
		t.Fatal(err)
	}
	if got := rollupAgrees(t, s, u.ID); math.Abs(got-(60+55+50)) > 1e-6 {
		t.Fatalf("after the window was replaced: %.1f s", got)
	}
	all, _ := s.ModelSums(ctx, u.ID, 0, m0, m0.Add(3*time.Hour))
	got = sumsOf(all)
	if math.Abs(got["hand"].Seconds-60) > 1e-6 || math.Abs(got["gpt"].Seconds-55) > 1e-6 || math.Abs(got["opus"].Seconds) > 1e-6 || math.Abs(got["sonnet"].Seconds-50) > 1e-6 {
		t.Fatalf("after the window was replaced: %+v", all)
	}
}

// Migration 7 on a database with time in it: every hour's old time goes in
// as hand time and "no model" time that add up to what hourly holds.
func TestModelRollupBackfill(t *testing.T) {
	dsn := storetest.DSN(t)
	all := migrations
	migrations = all[:6]
	s, err := Open(dsn)
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u, _ := s.LoginIdentity(ctx, Identity{"dev", "1"}, "bob", "", "", "", "")
	p, err := s.UpsertProject(ctx, u.ID, api.ProjectMeta{Key: "p"})
	if err != nil {
		t.Fatal(err)
	}
	hour := time.Now().Unix()/3600 - 5
	type row struct {
		min             int64
		dev             string
		struggle, agent float64
		prompts         int
	}
	// minute 0 from two devices (the larger wins), minute 1 all AI, minute 61 by hand
	old := []row{{0, "a", 40, 10, 1}, {0, "b", 55, 55, 2}, {1, "a", 60, 60, 0}, {61, "a", 30, 0, 0}}
	for _, r := range old {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO buckets(user_id,project_id,start_min,device_id,human,struggle,dark_flow,agent,prompts) VALUES (?,?,?,?,?,?,0,?,?)`,
			u.ID, p.ID, hour*60+r.min, r.dev, r.struggle, r.struggle, r.agent, r.prompts); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []struct {
		hour     int64
		struggle float64
		prompts  int
	}{{hour, 115, 3}, {hour + 1, 30, 0}} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO hourly(user_id,project_id,hour,human,struggle,prompts) VALUES (?,?,?,?,?,?)`, u.ID, p.ID, h.hour, h.struggle, h.struggle, h.prompts); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	if s, err = Open(dsn); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	if got := rollupAgrees(t, s, u.ID); math.Abs(got-145) > 1e-6 {
		t.Fatalf("counted %.1f s", got)
	}
	ms, _ := s.ModelSums(ctx, u.ID, p.ID, time.Unix(0, 0), time.Now())
	got := sumsOf(ms)
	if len(ms) != 2 || math.Abs(got["hand"].Seconds-30) > 1e-6 || math.Abs(got[""].Seconds-115) > 1e-6 || got[""].Prompts != 3 {
		t.Fatalf("backfill: %+v", ms)
	}
	// and new time on top keeps adding up
	if err := s.UpsertBuckets(ctx, u.ID, "a", []ledger.Bucket{{Project: "p", Start: time.Unix((hour*60+1)*60, 0).UTC(), AISeconds: 30,
		Models: []ledger.ModelUse{{Model: "opus", Seconds: 30}}}}); err != nil {
		t.Fatal(err)
	}
	rollupAgrees(t, s, u.ID)
}
