package stats

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"seatime/internal/client/store"
	"seatime/pkg/event"
)

// seedTruthWeek writes the same small day on six days of a week (none on
// Thursday) and labels it, except on Wednesday:
//
//	10:00-10:30  editor heartbeats every minute
//	11:00        prompt (20 words), agent runs to 11:02 (a tool call at 11:01)
//	11:05        prompt (4 words), agent runs to 11:06, session ends
//
// Labels: 10:00-10:25 hands-on, 10:30-10:55 away, 11:00 watching, 11:05 reading.
func seedTruthWeek(t *testing.T, st *store.Store, mon time.Time) {
	t.Helper()
	add := func(e event.Event) {
		if _, err := st.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	for d := 0; d < 7; d++ {
		if d == 3 {
			continue
		}
		day := mon.AddDate(0, 0, d)
		at := func(h, m, s int) time.Time {
			return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
		}
		for m := 0; m <= 30; m++ {
			add(event.Event{Kind: event.Edit, Harness: "editor:vscode", Project: "github.com/kid/game", TS: at(10, m, 0)})
		}
		ai := func(k event.Kind, ts time.Time, words, chars int) {
			add(event.Event{Kind: k, Harness: "claude-code", SessionID: "s", Project: "github.com/kid/game", TS: ts, Words: words, Chars: chars})
		}
		ai(event.Prompt, at(11, 0, 0), 20, 120)
		ai(event.AgentStart, at(11, 0, 0), 0, 0)
		ai(event.Tool, at(11, 1, 0), 0, 0)
		ai(event.AgentStop, at(11, 2, 0), 0, 0)
		ai(event.Prompt, at(11, 5, 0), 4, 30)
		ai(event.AgentStart, at(11, 5, 0), 0, 0)
		ai(event.AgentStop, at(11, 6, 0), 0, 0)
		ai(event.SessionEnd, at(11, 6, 0), 0, 0)
		if d == 2 {
			continue // a day not labelled yet
		}
		ls := map[int64]string{}
		for m := 0; m < 60; m += 5 {
			l := store.LabelHandsOn
			if m >= 30 {
				l = store.LabelAway
			}
			ls[store.BlockOf(at(10, m, 0))] = l
		}
		ls[store.BlockOf(at(11, 0, 0))] = store.LabelWatching
		ls[store.BlockOf(at(11, 5, 0))] = store.LabelReading
		if err := st.SetLabels(ls); err != nil {
			t.Fatal(err)
		}
	}
}

// The export for a seeded week matches a table computed by hand.
//
// Rule heartbeat/1 (pkg/ledger), per day, in minutes:
//
//	hand edits 10:00-10:30: 30 gaps of 1 min                     30
//	10:30 to the 11:00 prompt (20 words, new), capped              2
//	the agent: 11:01 and its stop 11:02                            2
//	11:02 to the 11:05 prompt (4 words, new), capped               2
//	the agent's stop 11:06                                         1
//	                                                              37
//
// plus the gap from the day before's 11:06 to this day's first edit at
// 10:00, capped at 2 and placed before 10:00: 39 min on every day but
// Monday, the first, which has no day before (37). Friday's gap comes
// from Wednesday.
//
// Hackatime: 30 gaps of 60 s = 30 min. AI time: hook events at 11:00,
// 11:00, 11:01, 11:02, 11:05, 11:05, 11:06: gaps 0+60+60+120 (180 capped)
// +0+60 = 300 s; the 1/3 rule = 30 + 5/3 = 31.667 min. Labels: 30 min
// hands-on, 5 reading, 5 watching, 30 away; at work = 40 min.
func TestTruthExportMatchesHandTable(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mon := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	seedTruthWeek(t, st, mon)
	got, err := Compare(st, mon, mon.AddDate(0, 0, 6), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		rule, hack, third, labelled, hands, reading, watching, away float64
		unlabelled                                                  int
	}
	labelled := row{39, 30, 30 + 5.0/3, 40, 30, 5, 5, 30, 0}
	monday := labelled
	monday.rule = 37
	want := []row{monday, labelled, {39, 30, 30 + 5.0/3, 0, 0, 0, 0, 0, 9}, {}, labelled, labelled, labelled}
	if len(got) != 7 {
		t.Fatalf("%d days", len(got))
	}
	for i, w := range want {
		g := got[i]
		m := func(sec float64) float64 { return sec / 60 }
		have := row{m(g.Rule), m(g.Hackatime), m(g.ThirdRule), m(g.Labelled), m(g.ByLabel[store.LabelHandsOn]), m(g.ByLabel[store.LabelReading]),
			m(g.ByLabel[store.LabelWatching]), m(g.ByLabel[store.LabelAway]), g.UnlabelledActive}
		close := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
		if !close(have.rule, w.rule) || !close(have.hack, w.hack) || !close(have.third, w.third) || !close(have.labelled, w.labelled) ||
			!close(have.hands, w.hands) || !close(have.reading, w.reading) || !close(have.watching, w.watching) || !close(have.away, w.away) || have.unlabelled != w.unlabelled {
			t.Errorf("%s: got %+v, want %+v", g.Day.Format("Mon 01-02"), have, w)
		}
	}
}
