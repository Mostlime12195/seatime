package sim

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"seatime/internal/client/stats"
)

// TestFiveProjectsEverywhere: the TestFiveProjects heartbeats, sent by
// an editor plugin to a real client that syncs to an in-process server.
// Hand edits in A 14:00, B 14:01, C 14:02, D 14:05, E 14:06, A 14:07: each
// gap goes to the project that ends it, B 1, C 1, D 3 capped at 2, E 1,
// A 1 min, 6 min in all. The dashboard, the server, the submission for A
// and `seatime verify` on its audit package all say 60 s for A, and the
// dashboard and the server 360 s for the day.
func TestFiveProjectsEverywhere(t *testing.T) {
	dir := sandbox(t)
	w, err := NewWorld(filepath.Join(dir, "w"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	d, err := w.NewDevice("fiver", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	day := stats.StartOfDay(w.Now(), time.UTC).AddDate(0, 0, 1)
	w.Advance(day.Add(13*time.Hour + 50*time.Minute).Sub(w.Now()))
	roots := map[string]string{}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		roots[n] = d.Project("", n)
		_ = os.WriteFile(filepath.Join(roots[n], "main.go"), []byte("package main\n"), 0o600)
	}
	for _, x := range []struct {
		p string
		m int
	}{{"a", 0}, {"b", 1}, {"c", 2}, {"d", 5}, {"e", 6}, {"a", 7}} {
		at := day.Add(14*time.Hour + time.Duration(x.m)*time.Minute)
		w.Advance(at.Sub(w.Now()))
		d.Heartbeat(map[string]any{"entity": filepath.Join(roots[x.p], "main.go"), "type": "file", "category": "coding",
			"time": float64(at.Unix()), "user_agent": "wakatime/v1.90 vscode/1.85 vscode-wakatime/24.0"})
	}
	w.Advance(5 * time.Minute)
	if err := d.SyncNow(); err != nil {
		t.Fatal(err)
	}
	keyA := ProjectKey("fiver", "a")

	sum, err := stats.Summarize(d.Store, d.Now(), time.UTC, 0)
	if err != nil {
		t.Fatal(err)
	}
	dashA := 0.0
	for _, p := range sum.TodayByProj {
		if p.Key == keyA {
			dashA = p.Seconds
		}
	}
	if dashA != 60 || sum.Today != 360 {
		t.Errorf("dashboard: A %.1f s, day %.1f s; want 60 and 360", dashA, sum.Today)
	}

	ctx := context.Background()
	u, err := w.Store.UserByName(ctx, "fiver")
	if err != nil {
		t.Fatal(err)
	}
	ps, err := w.Store.ProjectSums(ctx, u.ID, day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	srvA, srvDay := 0.0, 0.0
	for _, p := range ps {
		srvDay += p.Struggle
		if p.Project.Key == keyA {
			srvA = p.Struggle
		}
	}
	if srvA != 60 || srvDay != 360 {
		t.Errorf("server: A %.1f s, day %.1f s; want 60 and 360", srvA, srvDay)
	}

	s := d.Submit(keyA, SubmitOpts{})
	if s.Err != "" || !s.Verified {
		t.Fatalf("segment: %s %v", s.Err, s.Problems)
	}
	if s.Claimed.Seconds() != 60 || s.Scored.Seconds() != 60 {
		t.Errorf("segment: claimed %.1f s, server %.1f s; want 60", s.Claimed.Seconds(), s.Scored.Seconds())
	}
}
