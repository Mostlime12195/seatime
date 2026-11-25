package sim

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"seatime/internal/client/stats"
	"seatime/internal/client/syncer"
)

// TestDashboardMatchesServer: the hours a person sees on their own machine
// (the local dashboard, internal/client/stats) and the hours the server adds
// up from what that machine synced (profile, project pages, the
// Hackatime-shaped API) are the same numbers, per project and per day.
//
// The day mixes what moves minutes after they were first synced: an agent
// turn the person comes back to (credit appears for minutes already sent),
// a hop between two projects (minutes change project), and a long stretch
// of agent tool calls with nobody at the keyboard. The idle record written
// when its window closes changes nothing (rule heartbeat/1).
func TestDashboardMatchesServer(t *testing.T) {
	dir := sandbox(t)
	w, err := NewWorld(filepath.Join(dir, "w"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	d, err := w.NewDevice("parity", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	a := NewActor(d, 7).In(d.Project("", "game"))
	b := NewActor(d, 8).In(d.Project("", "site"))

	a.Start()
	a.Prompt(90*sec, "add a pause menu with resume and quit buttons and keep the score visible behind it")
	a.Agent(R(20*sec), W(40*sec, "src/menu.js"), Pass(30*sec, "npm test"))
	a.Typing("src/menu.js", 6*mn, 3, 40*sec)
	b.Start()
	b.Prompt(60*sec, "make the header sticky on mobile and shrink the logo when scrolling")
	b.Agent(W(30*sec, "style.css"))
	a.Prompt(40*sec, "the quit button should ask for confirmation first")
	b.Typing("index.html", 3*mn, 4, 30*sec)
	a.Agent(W(50*sec, "src/menu.js"), Pass(20*sec, "npm test"))
	a.Typing("src/menu.js", 4*mn, 2, 40*sec)
	// a long run nobody watches: tool calls for 70 minutes, no input
	a.Prompt(30*sec, "refactor the level loader into modules and add tests for each one")
	steps := []Step{}
	for i := 0; i < 35; i++ {
		steps = append(steps, W(2*mn, "src/level.js"))
	}
	a.Agent(steps...)
	a.Wait(25 * mn)
	a.Prompt(2*mn, "explain what changed in the loader and why the tests are split that way")
	a.Agent(R(30*sec), R(40*sec))
	a.Typing("src/level.js", 5*mn, 2, 40*sec)
	a.End()
	b.End()
	// The person leaves. A file watcher keeps the editor sending heartbeats
	// for 15 minutes, then an agent left running keeps calling tools for 50
	// more (no act of the person's, so no credit, but records that keep the
	// device syncing). The watcher's minutes count as hand editing until the
	// idle record, written when its window closes about an hour after the
	// person left, says nobody touched the machine. Under heartbeat/1 the
	// watcher's heartbeats are hand heartbeats and keep their time.
	for i := 0; i < 30; i++ {
		a.Wait(30 * sec)
		a.Heartbeat("src/level.js", true)
	}
	synced := serverSeconds(t, w, "parity")
	b.NoHands = true // a scheduled job sends this one; nobody is at the machine
	b.Send("go through the stylesheet and remove every rule nothing uses")
	left := []Step{}
	for i := 0; i < 25; i++ {
		left = append(left, W(2*mn, "style.css"))
	}
	b.Agent(left...)
	a.Wait(80 * mn) // every idle window closes and syncs
	if err := d.SyncNow(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	now := d.Now()
	sum, err := stats.Summarize(d.Store, now, time.UTC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Today < 20*60 || len(sum.TodayByProj) != 2 {
		t.Fatalf("the day did not record what it should: %.0f s over %v", sum.Today, sum.TodayByProj)
	}
	if synced > sum.Today+0.5 {
		t.Fatalf("the server held %.0f s before the end of the day, more than the day's %.0f s", synced, sum.Today)
	}
	u, err := w.Store.UserByName(ctx, "parity")
	if err != nil {
		t.Fatal(err)
	}
	from := stats.StartOfDay(now, time.UTC)
	ps, err := w.Store.ProjectSums(ctx, u.ID, from, from.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	server := map[string]float64{}
	var serverTotal float64
	for _, p := range ps {
		server[p.Project.Key] += p.Struggle
		serverTotal += p.Struggle
	}
	var dashTotal float64
	for _, p := range sum.TodayByProj {
		dashTotal += p.Seconds
		got := server[syncer.ServerKey(p.Key)]
		t.Logf("%s: dashboard %.1f s, server %.1f s", p.Name, p.Seconds, got)
		if math.Abs(got-p.Seconds) > 0.5 {
			t.Errorf("%s: dashboard %.1f s, server %.1f s", p.Name, p.Seconds, got)
		}
	}
	if math.Abs(dashTotal-sum.Today) > 0.5 {
		t.Errorf("unattributed time on the dashboard: today %.1f s, projects %.1f s", sum.Today, dashTotal)
	}
	if math.Abs(serverTotal-sum.Today) > 0.5 {
		t.Errorf("day total: dashboard %.1f s, server %.1f s", sum.Today, serverTotal)
	}
	day := from.Unix() / 86400
	ds, err := w.Store.DaySums(ctx, u.ID, 0, day, day, 0)
	if err != nil || len(ds) != 1 || math.Abs(ds[0].Struggle-sum.Today) > 0.5 {
		t.Errorf("server day sum %+v (err %v), dashboard %.1f s", ds, err, sum.Today)
	}
}

func serverSeconds(t *testing.T, w *World, user string) float64 {
	t.Helper()
	ctx := context.Background()
	u, err := w.Store.UserByName(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := w.Store.ProjectSums(ctx, u.ID, time.Unix(0, 0), w.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var sec float64
	for _, p := range ps {
		sec += p.Struggle
	}
	return sec
}
