package stats

import (
	"sort"
	"strings"
	"time"

	"seatime/internal/client/store"
	"seatime/pkg/ledger"
)

// Named is a total for one project, harness or day.
type Named struct {
	Name    string
	Key     string
	Seconds float64
}

// DayTotal is one calendar day's credited time.
type DayTotal struct {
	Day     time.Time // local midnight
	Seconds float64
}

// Summary is everything the personal dashboard and `seatime today` show,
// computed from the local log with the one-page rule (pkg/ledger). It needs
// no account and no network.
type Summary struct {
	Now         time.Time
	Today       float64 // credited seconds today
	Week        float64 // credited seconds in the last 7 days, today included
	TodayByProj []Named
	WeekByProj  []Named
	WeekByTool  []Named
	Days        []DayTotal // the last 7 days, oldest first
	Streak      int        // days in a row (ending today or yesterday) with at least StreakMin
	GoalHours   float64
	HandsOn     float64 // share of this week's counted time that was hand time
	HandsOnPast float64 // the same share over the 4 weeks before
	HasPast     bool
	Longest     time.Duration // longest focused stretch today (gaps under FocusGap)
	// AINotCounted is today's agent time outside the 10 minutes after one
	// of your heartbeats (docs/RULE.md): shown, never counted.
	AINotCounted float64
	// PendingToday is today's time that waits for the prompt check: it
	// counts once the prompts behind it pass (pkg/promptcheck). Not in
	// Today; 0 while the check is off.
	PendingToday float64
	// CheckOn: the prompt check is on now, so agent time counts only
	// through prompts that passed it.
	CheckOn      bool
	EventsToday  int
	EventsWeek   int
	EverRecorded bool
}

// StreakMin is the least credited time that keeps a day in the streak.
const StreakMin = 15 * time.Minute

// FocusGap is the longest pause inside one focused stretch.
const FocusGap = 2 * time.Minute

// DefaultGoalHours is the weekly goal when the user has not set one.
const DefaultGoalHours = 10

// Summarize computes the dashboard for the 35 days up to now.
func Summarize(st *store.Store, now time.Time, loc *time.Location, goalHours float64) (Summary, error) {
	if goalHours <= 0 {
		goalHours = DefaultGoalHours
	}
	today := StartOfDay(now, loc)
	weekStart := today.AddDate(0, 0, -6)
	pastStart := weekStart.AddDate(0, 0, -28)
	end := now.Add(time.Second)
	res, evs, err := Credit(st, pastStart, end)
	if err != nil {
		return Summary{}, err
	}
	s := Summary{Now: now, GoalHours: goalHours}
	if set := PromptChecks(st); set != nil {
		s.CheckOn = set.OnAt(now)
	}
	if n, _ := st.Count(); n > 0 {
		s.EverRecorded = true
	}
	for _, e := range evs {
		if !e.Kind.Activity() {
			continue
		}
		if !e.TS.Before(today) {
			s.EventsToday++
		}
		if !e.TS.Before(weekStart) {
			s.EventsWeek++
		}
	}
	byDay := map[time.Time]float64{}
	tp, wp, wt := map[string]float64{}, map[string]float64{}, map[string]float64{}
	var handsWeek, handsPast, past float64
	var stretchStart, stretchEnd time.Time
	for _, sg := range res.Segments {
		sec := sg.Seconds()
		if !sg.Counted {
			if !sg.Start.Before(today) {
				s.AINotCounted += sec
			}
			continue
		}
		day := StartOfDay(sg.Start, loc)
		byDay[day] += sec
		hands := sg.Category == ledger.Coding
		if sg.Start.Before(weekStart) {
			past += sec
			if hands {
				handsPast += sec
			}
			continue
		}
		s.Week += sec
		wp[sg.Project] += sec
		wt[toolName(sg.Tool)] += sec
		if hands {
			handsWeek += sec
		}
		if sg.Start.Before(today) {
			continue
		}
		s.Today += sec
		tp[sg.Project] += sec
		if stretchStart.IsZero() || sg.Start.Sub(stretchEnd) > FocusGap {
			stretchStart = sg.Start
		}
		stretchEnd = sg.End
		if d := stretchEnd.Sub(stretchStart); d > s.Longest {
			s.Longest = d
		}
	}
	s.TodayByProj, s.WeekByProj, s.WeekByTool = sorted(tp, true), sorted(wp, true), sorted(wt, false)
	for _, ns := range [][]Named{s.TodayByProj, s.WeekByProj} {
		for i := range ns {
			ns[i].Name = ProjectName(st.Label(ns[i].Key)) // as its folder is called now
		}
	}
	for i := 6; i >= 0; i-- {
		d := today.AddDate(0, 0, -i)
		s.Days = append(s.Days, DayTotal{Day: d, Seconds: byDay[d]})
	}
	if s.Week > 0 {
		s.HandsOn = handsWeek / s.Week
	}
	if past > 0 {
		s.HandsOnPast, s.HasPast = handsPast/past, true
	}
	d := today
	if byDay[d] < StreakMin.Seconds() {
		d = d.AddDate(0, 0, -1) // today is not over yet
	}
	for ; byDay[d] >= StreakMin.Seconds(); d = d.AddDate(0, 0, -1) {
		s.Streak++
	}
	return s, nil
}

func sorted(m map[string]float64, projects bool) []Named {
	var out []Named
	for k, v := range m {
		if v <= 0 {
			continue
		}
		n := Named{Key: k, Name: k, Seconds: v}
		if projects {
			n.Name = ProjectName(k)
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seconds != out[j].Seconds {
			return out[i].Seconds > out[j].Seconds
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// ProjectName is a project key's display name.
func ProjectName(k string) string { return name(k) }

func toolName(h string) string {
	switch {
	case h == "":
		return "other"
	case strings.HasPrefix(h, "editor:"):
		return strings.TrimPrefix(h, "editor:") + " (by hand)"
	case h == "terminal":
		return "terminal"
	}
	return h
}
