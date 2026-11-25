package sim

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"seatime/internal/client/stats"
	"seatime/pkg/audit"
)

// TestLongProjectSplits: a project worked on most evenings
// for 100 days goes up with one `seatime submit`, as back-to-back parts of
// at most 62 days. The parts are verified, carry no flag, and add up to the
// dashboard's total for the project (its days added up) to the second,
// and so does the server.
func TestLongProjectSplits(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end simulation")
	}
	dir := sandbox(t)
	w, err := NewWorld(filepath.Join(dir, "w"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	d, err := w.NewDevice("marathon", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	// No idle timer on this machine (its hours carry the "no idle signal"
	// label).
	d.idleRec = nil
	root := d.Project("", "engine")
	a := NewActor(d, 11).In(root)
	start := w.Now()
	for day := 0; day < 100; day++ {
		dayStart := start.Add(time.Duration(day) * 24 * hr)
		if day%7 != 5 { // a day off a week
			a.In(root) // a new harness session each evening
			a.Start()
			for k := 0; k < 3+a.rng.IntN(4); k++ {
				a.Prompt(90*sec, fmt.Sprintf("day %d step %d: add the next piece of the physics engine and test it against the reference values", day, k))
				a.Agent(R(20*sec), W(50*sec, "src/body.go"), Pass(30*sec, cmdGoTest))
				if k%2 == 1 {
					a.Typing("src/body.go", 4*mn, 1, 45*sec)
				}
			}
			a.Commit()
			a.End()
			a.Wait(10 * mn)
		}
		d.Sleep(dayStart.Add(24 * hr).Sub(w.Now()))
	}
	a.Wait(5 * mn)

	key := ProjectKey("marathon", "engine")
	// the dashboard's total: its day figures (stats.Summarize counts each
	// day from midnight) added up over the 100 days
	var dash float64
	for day := stats.StartOfDay(start, time.UTC); day.Before(w.Now()); day = day.AddDate(0, 0, 1) {
		res, _, err := stats.Credit(d.Store, day, day.AddDate(0, 0, 1))
		if err != nil {
			t.Fatal(err)
		}
		dash += res.ByProject[key]
	}
	subs, err := d.SubmitAll(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) < 2 {
		t.Fatalf("100 days went as %d submission(s)", len(subs))
	}
	var sum float64
	for i, s := range subs {
		if !s.Verified || len(s.Problems) > 0 || s.Export == nil {
			t.Fatalf("part %d: verified %v, problems %v, err %s", i+1, s.Verified, s.Problems, s.Err)
		}
		var lo, hi time.Time
		for _, e := range s.Events {
			if lo.IsZero() || e.TS.Before(lo) {
				lo = e.TS
			}
			if e.TS.After(hi) {
				hi = e.TS
			}
		}
		if hi.Sub(lo) > audit.MaxSpan {
			t.Errorf("part %d spans %v", i+1, hi.Sub(lo))
		}
		t.Logf("part %d: %s to %s, %.1f s credited", i+1, lo.Format("Jan 2 15:04"), hi.Format("Jan 2 15:04"), s.Scored.Seconds())
		sum += s.Scored.Seconds()
	}
	server := serverSeconds(t, w, "marathon")
	t.Logf("dashboard %.1f s, submissions %.1f s, server %.1f s", dash, sum, server)
	if dash < 10*3600 {
		t.Fatalf("the dashboard holds only %.1f h", dash/3600)
	}
	if math.Abs(sum-dash) > 0.5 || math.Abs(server-dash) > 0.5 {
		t.Errorf("submissions %.1f s, server %.1f s, dashboard %.1f s", sum, server, dash)
	}
}
