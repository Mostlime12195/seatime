package stats

import (
	"slices"
	"time"

	"seatime/internal/client/store"
	"seatime/pkg/event"
)

// HackatimeTimeout is the heartbeat join Hackatime uses: the gap between
// two consecutive heartbeats counts, up to this long.
const HackatimeTimeout = 2 * time.Minute

// DayCompare is one day of the ground-truth export: the one-page rule, what
// Hackatime would count, today's guideline (Hackatime plus a third of AI
// time) and the person's own labels, side by side. Seconds.
type DayCompare struct {
	Day              time.Time // local midnight
	Rule             float64   // pkg/ledger, the same number as the dashboard
	Hackatime        float64   // editor heartbeats, gaps up to HackatimeTimeout
	AITime           float64   // AI-tool hook events, gaps up to HackatimeTimeout
	ThirdRule        float64   // Hackatime + AITime/3
	Labelled         float64   // hands-on + reading + watching
	ByLabel          map[string]float64
	UnlabelledActive int // blocks with recorded activity and no label
}

// Compare builds the export for the local days from..to (inclusive).
func Compare(st *store.Store, from, to time.Time, loc *time.Location) ([]DayCompare, error) {
	start := StartOfDay(from, loc)
	end := StartOfDay(to, loc).AddDate(0, 0, 1)
	res, evs, err := Credit(st, start, end)
	if err != nil {
		return nil, err
	}
	labels, err := st.Labels(start, end)
	if err != nil {
		return nil, err
	}
	var days []DayCompare
	idx := map[time.Time]int{}
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		idx[d] = len(days)
		days = append(days, DayCompare{Day: d, ByLabel: map[string]float64{}})
	}
	day := func(t time.Time) *DayCompare {
		if i, ok := idx[StartOfDay(t, loc)]; ok {
			return &days[i]
		}
		return nil
	}
	for _, sg := range res.Segments {
		if d := day(sg.Start); d != nil && sg.Counted {
			d.Rule += sg.Seconds()
		}
	}
	// Hackatime: editor heartbeats; AI time: the harness's own events. A gap
	// counts up to the timeout and never across midnight.
	join := func(ts []time.Time, add func(d *DayCompare, sec float64)) {
		slices.SortFunc(ts, func(a, b time.Time) int { return a.Compare(b) })
		for i := 1; i < len(ts); i++ {
			d := day(ts[i])
			if d == nil || day(ts[i-1]) != d {
				continue
			}
			add(d, min(ts[i].Sub(ts[i-1]), HackatimeTimeout).Seconds())
		}
	}
	var beats, ai []time.Time
	active := map[int64]bool{}
	for _, e := range evs {
		if e.TS.Before(start) || !e.TS.Before(end) || !e.Kind.Activity() {
			continue
		}
		active[store.BlockOf(e.TS)] = true
		switch {
		case e.Kind == event.Edit && len(e.Harness) > 7 && e.Harness[:7] == "editor:":
			beats = append(beats, e.TS)
		case e.SessionID != "" && (e.Kind == event.Prompt || e.Kind == event.AgentStart || e.Kind == event.AgentStop || e.Kind == event.Tool):
			ai = append(ai, e.TS)
		}
	}
	join(beats, func(d *DayCompare, s float64) { d.Hackatime += s })
	join(ai, func(d *DayCompare, s float64) { d.AITime += s })
	for b, l := range labels {
		if d := day(store.BlockStart(b)); d != nil {
			d.ByLabel[l] += store.LabelBlock.Seconds()
			if l != store.LabelAway {
				d.Labelled += store.LabelBlock.Seconds()
			}
		}
	}
	for b := range active {
		if _, ok := labels[b]; !ok {
			if d := day(store.BlockStart(b)); d != nil {
				d.UnlabelledActive++
			}
		}
	}
	for i := range days {
		days[i].ThirdRule = days[i].Hackatime + days[i].AITime/3
	}
	return days, nil
}

// Block is one 5-minute block of the labelling day view: what was recorded
// in it (as a reminder, not a suggestion) and the person's label.
type Block struct {
	Start                       time.Time
	Label                       string
	Credited                    float64 // the rule's credited seconds
	Prompts, Edits, Runs, Agent int     // recorded events
	Commits                     int
}

// DayBlocks returns the blocks of a local day from the first to the last
// recorded activity (half an hour either side, whole hours), plus any
// labelled block outside that span.
func DayBlocks(st *store.Store, day time.Time, loc *time.Location) ([]Block, error) {
	start := StartOfDay(day, loc)
	end := start.AddDate(0, 0, 1)
	res, evs, err := Credit(st, start, end)
	if err != nil {
		return nil, err
	}
	labels, err := st.Labels(start, end)
	if err != nil {
		return nil, err
	}
	var lo, hi time.Time
	see := func(t time.Time) {
		if lo.IsZero() || t.Before(lo) {
			lo = t
		}
		if t.After(hi) {
			hi = t
		}
	}
	for _, e := range evs {
		if e.Kind.Activity() && !e.TS.Before(start) && e.TS.Before(end) {
			see(e.TS)
		}
	}
	for b := range labels {
		see(store.BlockStart(b))
	}
	if lo.IsZero() {
		return nil, nil
	}
	lo = maxT(start, lo.Add(-30*time.Minute)).Truncate(time.Hour)
	hi = minT(end, hi.Add(90*time.Minute).Truncate(time.Hour))
	var out []Block
	at := map[int64]int{}
	for t := lo; t.Before(hi); t = t.Add(store.LabelBlock) {
		b := store.BlockOf(t)
		at[b] = len(out)
		out = append(out, Block{Start: t.In(loc), Label: labels[b]})
	}
	for _, sg := range res.Segments {
		if !sg.Counted {
			continue
		}
		for a := sg.Start; a.Before(sg.End); {
			b := minT(store.BlockStart(store.BlockOf(a)+1), sg.End)
			if i, ok := at[store.BlockOf(a)]; ok {
				out[i].Credited += b.Sub(a).Seconds()
			}
			a = b
		}
	}
	for _, e := range evs {
		i, ok := at[store.BlockOf(e.TS)]
		if !ok {
			continue
		}
		switch e.Kind {
		case event.Prompt:
			out[i].Prompts++
		case event.Edit:
			out[i].Edits++
		case event.Run:
			out[i].Runs++
		case event.Commit:
			out[i].Commits++
		case event.AgentStart, event.Tool, event.AgentStop:
			out[i].Agent++
		}
	}
	return out, nil
}

func minT(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxT(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
