package ledger

import (
	"cmp"
	"slices"
	"time"

	"seatime/pkg/event"
)

// The cap rule (rule heartbeat/2, docs/RULE.md), used only while the
// prompt check is switched on (pkg/promptcheck). It replaces definition
// 3's 10 minutes:
//
//   - A prompt that passed the check counts from when it was sent, for up
//     to its kind's minutes (Cap.Minutes). Its clock stops at the first of
//     those minutes, your next prompt in the project, and the AI being done
//     with you then idle (no keyboard or mouse input for IdleStop): it then
//     counts up to your last input. Your next counted heartbeat in another
//     project stops it too, so a minute is never counted twice.
//   - Hand edits count exactly as under heartbeat/1: each gap before one,
//     up to Timeout, unless a prompt's minutes already cover it.
//   - The agent's heartbeats never count on their own.

// CapVersion names the rule while the prompt check is on.
const CapVersion = "heartbeat/2"

// VersionFor is the rule's name with the prompt check on or off.
func VersionFor(capRule bool) string {
	if capRule {
		return CapVersion
	}
	return Version
}

// IdleStop: after the AI is done, this long with no input stops a
// prompt's clock at the last input.
const IdleStop = 2 * time.Minute

// Cap is the cap rule's input from the prompt check.
type Cap struct {
	// Minutes is how long a prompt (whose New the check has set) may
	// count; 0 when it did not pass.
	Minutes func(e event.Event) time.Duration
}

// MaxReach bounds how far past the last record a prompt can count (the
// count's range is widened by it).
const MaxReach = time.Hour

// ComputeCap counts evs (any order) for [from, to) with the cap rule.
func ComputeCap(evs []event.Event, from, to time.Time, c Cap) Result {
	return CountCap(HeartbeatsCap(evs, c), from, to)
}

// HeartbeatsCap turns events into heartbeats under the cap rule: hand
// edits and prompts that passed are kept (a prompt with Until set), the
// agent's are not.
func HeartbeatsCap(evs []event.Event, c Cap) []Heartbeat {
	hbs := heartbeats(evs, c.Minutes)
	acts := slices.DeleteFunc(slices.Clone(evs), func(e event.Event) bool { return !e.Counts() })
	slices.SortStableFunc(acts, func(a, b event.Event) int { return cmp.Or(a.TS.Compare(b.TS), cmp.Compare(a.Seq, b.Seq)) })
	idle := idleOf(evs)
	for i := range hbs {
		h := &hbs[i]
		h.Kept = h.Yours
		if !h.Prompt || !h.Yours || h.allow <= 0 {
			continue
		}
		h.Until = until(*h, acts, idle)
	}
	return hbs
}

// until is where a prompt's clock stops.
func until(h Heartbeat, acts []event.Event, idle idleData) time.Time {
	end := h.Time.Add(h.allow)
	var stop time.Time
	for _, e := range acts {
		if !e.TS.After(h.Time) {
			continue
		}
		if !e.TS.Before(end) {
			break
		}
		if e.Kind == event.Prompt && e.Project == h.Project {
			return e.TS // your next prompt in the project
		}
		if stop.IsZero() && e.Kind == event.AgentStop && e.SessionID == h.Session && e.Project == h.Project {
			stop = e.TS
		}
	}
	if !stop.IsZero() {
		if t := idle.lastInput(stop); t.Before(end) {
			end = t
		}
	}
	return end
}

// idleData is the idle timer's record: the stretches it watched and the
// stretches with no input.
type idleData struct {
	watched, none [][2]time.Time
}

func idleOf(evs []event.Event) idleData {
	var d idleData
	for _, e := range evs {
		if e.Kind != event.Idle || e.WindowMS <= 0 {
			continue
		}
		d.watched = append(d.watched, [2]time.Time{e.TS.Add(-time.Duration(e.WindowMS) * time.Millisecond), e.TS})
		for _, n := range e.NoInput {
			d.none = append(d.none, [2]time.Time{time.UnixMilli(n[0]).UTC(), time.UnixMilli(n[1]).UTC()})
		}
	}
	slices.SortFunc(d.watched, func(a, b [2]time.Time) int { return a[0].Compare(b[0]) })
	return d
}

// lastInput is where a prompt's clock stops once the AI is done at stop:
// the last input before the first IdleStop or more with no input after
// stop (not before stop itself). Where the idle timer did not
// watch, nothing shows the person was there: the clock stops at the end of
// what it watched, or at stop when it did not watch stop at all.
func (d idleData) lastInput(stop time.Time) time.Time {
	end, covered := stop, false
	for _, w := range d.watched {
		if w[0].After(end.Add(90 * time.Second)) {
			break
		}
		if !w[1].Before(stop) && !w[0].After(end.Add(90*time.Second)) {
			covered = true
			if w[1].After(end) {
				end = w[1]
			}
		}
	}
	if !covered {
		return stop
	}
	for _, n := range d.none {
		t := n[0]
		if t.Before(stop) {
			t = stop
		}
		if n[1].Sub(t) < IdleStop || !t.Before(end) {
			continue // not idle for IdleStop after the AI was done
		}
		end = t
	}
	return end
}

// MarkCap is Mark for the cap rule: each bare marker can carry how far
// that heartbeat counts forward (until[i]; zero when it does not), so the
// gaps of the revealed project split as on the device.
func MarkCap(hbs []Heartbeat, others, until []time.Time) []Heartbeat {
	out := slices.Clone(hbs)
	for i, t := range others {
		m := Heartbeat{Time: t.UTC(), Project: OtherProject, Kept: true, Yours: true, order: -1}
		if i < len(until) && until[i].After(t) {
			m.Until, m.Prompt = until[i].UTC(), true
		}
		out = append(out, m)
	}
	sortHeartbeats(out)
	return out
}

// CountCap sums the cap rule's time for [from, to). Going through the
// kept heartbeats in time order: the time since the previous one belongs
// first to the prompt whose clock is running (up to its Until), then, for
// a hand edit, up to Timeout of what is left before the edit. A kept
// heartbeat in another project stops the running clock. The agent's time
// that nothing covers is the time not counted.
func CountCap(hbs []Heartbeat, from, to time.Time) Result {
	r := Result{From: from, To: to, Heartbeats: hbs, ByProject: map[string]float64{}, Projects: map[string]Split{}}
	type clock struct {
		project, tool, model string
		until                time.Time
	}
	var cur *clock
	var prev time.Time
	var counted [][2]time.Time
	add := func(a, b time.Time, project, tool, category, model string) {
		if !b.After(a) {
			return
		}
		counted = append(counted, [2]time.Time{a, b})
		if project != OtherProject {
			r.addSegment(a, b, project, tool, category, model, true)
		}
	}
	for _, h := range hbs {
		if !h.Kept {
			continue
		}
		covered := prev
		if cur != nil && !prev.IsZero() {
			covered = minT(cur.until, h.Time)
			add(prev, covered, cur.project, cur.tool, AICoding, cur.model)
		}
		if h.Yours && !h.Prompt && !prev.IsZero() { // a hand edit (or a bare marker)
			a := h.Time.Add(-min(h.Time.Sub(prev), Timeout))
			if a.Before(covered) {
				a = covered
			}
			add(a, h.Time, h.Project, h.Tool, Coding, "")
		}
		switch {
		case h.Until.After(h.Time):
			cur = &clock{h.Project, h.Tool, h.Model, h.Until}
		case cur != nil && cur.project != h.Project:
			cur = nil
		}
		prev = h.Time
	}
	if cur != nil && !prev.IsZero() {
		add(prev, cur.until, cur.project, cur.tool, AICoding, cur.model)
	}
	// the agent's time nothing covers: not counted
	slices.SortFunc(counted, func(a, b [2]time.Time) int { return a[0].Compare(b[0]) })
	var prevAll *Heartbeat
	for i := range hbs {
		h := &hbs[i]
		if !h.Kept && !h.Yours && prevAll != nil && h.Project != OtherProject {
			a := h.Time.Add(-min(h.Time.Sub(prevAll.Time), Timeout))
			for _, g := range uncovered(a, h.Time, counted) {
				r.addSegment(g[0], g[1], h.Project, h.Tool, AICoding, h.Model, false)
			}
		}
		prevAll = h
	}
	slices.SortStableFunc(r.Segments, func(a, b Segment) int { return a.Start.Compare(b.Start) })
	return r
}

// uncovered is [a, b) minus the sorted spans.
func uncovered(a, b time.Time, spans [][2]time.Time) [][2]time.Time {
	var out [][2]time.Time
	for _, s := range spans {
		if !s[1].After(a) {
			continue
		}
		if !s[0].Before(b) {
			break
		}
		if s[0].After(a) {
			out = append(out, [2]time.Time{a, s[0]})
		}
		if s[1].After(a) {
			a = s[1]
		}
		if !a.Before(b) {
			return out
		}
	}
	if b.After(a) {
		out = append(out, [2]time.Time{a, b})
	}
	return out
}

func minT(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// addSegment adds [a, b), clipped to the result's range.
func (r *Result) addSegment(a, b time.Time, project, tool, category, model string, counted bool) {
	if a.Before(r.From) {
		a = r.From
	}
	if b.After(r.To) {
		b = r.To
	}
	if !b.After(a) {
		return
	}
	s := Segment{Start: a, End: b, Project: project, Tool: tool, Category: category, Model: model, Counted: counted}
	r.Segments = append(r.Segments, s)
	sec, p := s.Seconds(), r.Projects[project]
	switch {
	case !counted:
		p.AINotCounted += sec
		r.AINotCounted += sec
	case category == Coding:
		p.Coding += sec
		r.Coding += sec
	default:
		p.AI += sec
		r.AI += sec
	}
	r.Projects[project] = p
	if counted {
		r.Total += sec
		r.ByProject[project] += sec
	}
}
