package segment

import (
	"path/filepath"
	"testing"
	"time"

	"seatime/internal/client/store"
	"seatime/pkg/audit"
	"seatime/pkg/event"
)

func TestSplitTimes(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 18, 0, 0, 0, time.UTC)
	// one evening session an hour long, every day for 150 days
	var ts []time.Time
	for d := 0; d < 150; d++ {
		for m := 0; m < 60; m += 2 {
			ts = append(ts, t0.Add(time.Duration(d)*24*time.Hour+time.Duration(m)*time.Minute))
		}
	}
	rs := splitTimes(ts, Options{Project: "p"})
	if len(rs) != 3 {
		t.Fatalf("%d ranges for 150 days", len(rs))
	}
	if !rs[0].Since.Equal(ts[0].Add(-audit.SplitPad)) || !rs[len(rs)-1].Until.Equal(ts[len(ts)-1].Add(audit.SplitPad)) {
		t.Fatalf("ends not padded: %v .. %v", rs[0].Since, rs[len(rs)-1].Until)
	}
	for i, r := range rs {
		if r.Until.Sub(r.Since) > audit.MaxSpan {
			t.Errorf("range %d spans %v", i, r.Until.Sub(r.Since))
		}
		if i > 0 && !r.Since.Equal(rs[i-1].Until) {
			t.Errorf("range %d does not touch the one before", i)
		}
		// the cut lies in a pause: no record within audit.SplitPad of it
		for _, x := range ts {
			if d := x.Sub(r.Until); i < len(rs)-1 && d > -audit.SplitPad && d < audit.SplitPad {
				t.Errorf("record %v is %v from cut %v", x, d, r.Until)
				break
			}
		}
	}

	// a project that fits is one range, trimmed to its records
	short := ts[:30*30]
	if rs := splitTimes(short, Options{}); len(rs) != 1 || !rs[0].Until.Equal(short[len(short)-1].Add(audit.SplitPad)) {
		t.Fatalf("short project: %+v", rs)
	}
	// a range the user gave is kept at its ends
	since := t0.Add(-24 * time.Hour)
	if rs := splitTimes(short, Options{Since: since}); !rs[0].Since.Equal(since) {
		t.Fatalf("user's since moved: %v", rs[0].Since)
	}
}

// Next sends a stretch of work once it is over: 90 minutes after its last
// record with no new one, plus 75 minutes for the idle window to close; a
// pause of 3 hours or more in the middle is a cut, and each segment starts
// where the last one ended.
func TestNextWaitsForTheStretchToEnd(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const p = "github.com/me/app"
	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	for _, m := range []int{0, 5, 10, 300, 305} { // two stretches, a 290-minute pause between
		st.Append(event.Event{Kind: event.Edit, Project: p, TS: t0.Add(time.Duration(m) * time.Minute)})
	}
	head, _, _ := st.Head()
	// during the first stretch: nothing
	if rs, _, _ := Next(st, p, time.Time{}, head, t0.Add(20*time.Minute)); len(rs) != 0 {
		t.Fatalf("sent while working: %+v", rs)
	}
	// during the second stretch: the first goes, cut in the middle of the pause
	rs, end, err := Next(st, p, time.Time{}, head, t0.Add(306*time.Minute))
	if err != nil || len(rs) != 1 || !end.Equal(t0.Add(155*time.Minute)) || !rs[0].Until.Equal(end) {
		t.Fatalf("first stretch: %+v %v %v", rs, end, err)
	}
	// right after the second stretch: not yet
	if rs, _, _ := Next(st, p, end, head, t0.Add(400*time.Minute)); len(rs) != 0 {
		t.Fatalf("sent too early: %+v", rs)
	}
	// once it settled: from where the first ended
	rs, end2, _ := Next(st, p, end, head, t0.Add(305*time.Minute+audit.SplitPad+audit.Settle))
	if len(rs) != 1 || !rs[0].Since.Equal(end) || !end2.After(t0.Add(305*time.Minute)) {
		t.Fatalf("second stretch: %+v %v", rs, end2)
	}
}
