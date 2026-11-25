// Package ledger counts time with rule heartbeat/1 (docs/RULE.md): Hackatime's
// heartbeat count, unchanged, plus one rule for which of an AI agent's
// heartbeats count.
//
//  1. Your heartbeats: an editor heartbeat that is not "ai coding" and falls
//     outside an agent run in its project, and a new prompt.
//  2. A new prompt has MinWords words or more and was not sent before in the
//     project (decided on the device, event.Event.New). While the prompt
//     check is on, pkg/promptcheck sets New from the stored verdicts before
//     the count, with no word minimum.
//  3. The agent's heartbeats: one a minute from a prompt to the agent's
//     stop, the stop itself, prompts that are not new, and every "ai coding"
//     editor heartbeat. They count in the Window after one of yours in the
//     same project.
//  4. Every gap between two counted heartbeats counts up to Timeout, and
//     belongs to the project and category of the heartbeat that ends it.
//
// Nothing else changes a number: not the idle timer, not runs, not commits.
package ledger

import (
	"cmp"
	"slices"
	"strings"
	"time"
	"unicode"

	"seatime/pkg/chain"
	"seatime/pkg/event"
)

// Rule is the rule in one paragraph, word for word as docs/RULE.md has it.
const Rule = "**SeaTime counts hours the way Hackatime does: heartbeats, with every gap between two heartbeats counted up to 2 minutes. You make a heartbeat with a hand edit or a new prompt; an agent working on your prompt makes one a minute, and the agent's count only in the 10 minutes after one of yours.**"

// Version names the rule. It is stamped on synced buckets and segments.
const Version = "heartbeat/1"

// The three numbers of the rule (docs/RULE.md; a test keeps them equal).
const (
	// Timeout is Hackatime's heartbeat_timeout_duration (2.minutes) in
	// app/models/concerns/heartbeatable.rb: a gap between two heartbeats
	// counts up to this. It is Hackatime's number, not ours to tune.
	Timeout = 2 * time.Minute
	// Window: the agent's heartbeats count up to this long after one of yours.
	Window = 10 * time.Minute
	// MinWords: a shorter prompt is not new.
	MinWords = 4
)

// Categories, as Hackatime names them.
const (
	Coding   = "coding"
	AICoding = "ai coding"
)

// OtherProject is the project of a bare time marker: a counted heartbeat in
// a project a segment does not reveal (see Mark).
const OtherProject = "\x00other"

// Heartbeat is one heartbeat in time order.
type Heartbeat struct {
	Time    time.Time
	Project string
	Tool    string // the AI tool or editor
	Yours   bool   // a hand edit or a new prompt; otherwise the agent's
	Kept    bool   // counted: yours, or the agent's within Window of yours
	Session string // the AI tool's session, for prompts and the agent's
	// Model is the AI model behind a prompt or one of the agent's
	// heartbeats, as the tool named it: the model on the last record of the
	// prompt's turn that names one (its stop, a tool call), else the one on
	// the prompt itself. Empty for a hand edit, and when no record names one.
	Model  string
	Words  int // a prompt's length in words; 0 for anything else
	Prompt bool
	// Until, under the cap rule (cap.go), is how far a prompt that passed
	// counts forward from its time; zero for anything else and under
	// heartbeat/1.
	Until time.Time
	order int           // input order: Hackatime's ORDER BY time, id
	allow time.Duration // cap rule: a prompt's minutes, before Until is worked out
}

// Category is "coding" for a hand edit and "ai coding" for anything else:
// a new prompt is yours, but the time before it was spent on AI work, so
// it counts as AI hours (a project steered only by prompts has no hand
// hours). The split never changes the total.
func (h Heartbeat) Category() string {
	if h.Yours && !h.Prompt {
		return Coding
	}
	return AICoding
}

// Segment is a counted (or, with Counted false, not counted) stretch: the
// gap before a heartbeat, capped at Timeout, placed just before that
// heartbeat. So a gap lands on the day of the heartbeat that ends it, as in
// Hackatime's range queries, and new time never lands behind older time.
type Segment struct {
	Start, End time.Time
	Project    string
	Tool       string
	Category   string
	Counted    bool
	// Model is the model of the heartbeat the stretch belongs to (see
	// Heartbeat.Model); always empty for hand time.
	Model string
}

// Seconds is the segment's length.
func (s Segment) Seconds() float64 { return s.End.Sub(s.Start).Seconds() }

// Split is one project's seconds.
type Split struct{ Coding, AI, AINotCounted float64 }

// Counted is hand plus AI seconds.
func (s Split) Counted() float64 { return s.Coding + s.AI }

// Result is the rule's output for [From, To).
type Result struct {
	From, To   time.Time
	Heartbeats []Heartbeat
	Segments   []Segment
	Split                         // the whole range
	Total      float64            // counted seconds (Coding + AI)
	ByProject  map[string]float64 // counted seconds per project; sums to Total
	Projects   map[string]Split
}

// Compute counts evs (any order) for [from, to).
func Compute(evs []event.Event, from, to time.Time) Result {
	return Count(Heartbeats(evs), from, to)
}

// Mark adds bare time markers for counted heartbeats in other projects (a
// segment carries them so the gaps split between projects as on the
// device). At an equal time a marker goes first. A marker can only shorten
// a gap of the revealed project, never lengthen one.
func Mark(hbs []Heartbeat, others []time.Time) []Heartbeat {
	out := slices.Clone(hbs)
	for _, t := range others {
		out = append(out, Heartbeat{Time: t.UTC(), Project: OtherProject, Kept: true, order: -1})
	}
	sortHeartbeats(out)
	return out
}

// Heartbeats turns events into heartbeats (definitions 1 to 3), sorted, with
// Kept set.
func Heartbeats(evs []event.Event) []Heartbeat { return heartbeats(evs, nil) }

func heartbeats(evs []event.Event, minutes func(event.Event) time.Duration) []Heartbeat {
	acts := slices.DeleteFunc(slices.Clone(evs), func(e event.Event) bool { return !e.Counts() })
	slices.SortStableFunc(acts, func(a, b event.Event) int { return cmp.Or(a.TS.Compare(b.TS), cmp.Compare(a.Seq, b.Seq)) })
	var out []Heartbeat
	add := func(t time.Time, e event.Event, yours bool) {
		out = append(out, Heartbeat{Time: t.UTC(), Project: e.Project, Tool: e.Harness, Yours: yours, Session: e.SessionID, Model: e.Model, order: len(out)})
	}
	// agent runs, per session and project: a prompt starts one, the stop ends
	// it; with no stop it ends at the session's next prompt or end, or its
	// last event
	// A run's model is the one its records name last (a tool call, the
	// stop: what answered the prompt), else the prompt's own; the prompt's
	// heartbeat (hb) and the run's minutes take it.
	type run struct {
		start, end time.Time
		e          event.Event
		hb         int
		model      string
	}
	var runs []run
	open := map[string]*run{}
	end := func(k string, t time.Time) {
		if r := open[k]; r != nil {
			r.end = t
			if r.model != "" {
				r.e.Model = r.model
			}
			out[r.hb].Model = r.e.Model
			runs = append(runs, *r)
			delete(open, k)
		}
	}
	seen := map[string]bool{}
	for _, e := range acts {
		k := e.SessionID + "\x00" + e.Project
		switch e.Kind {
		case event.Prompt:
			end(k, e.TS)
			add(e.TS, e, isNew(e, seen))
			out[len(out)-1].Prompt, out[len(out)-1].Words = true, e.Words
			if minutes != nil && out[len(out)-1].Yours {
				out[len(out)-1].allow = minutes(e)
			}
			open[k] = &run{start: e.TS, end: e.TS, e: e, hb: len(out) - 1}
		case event.AgentStop:
			if r := open[k]; r != nil {
				if e.Model != "" {
					r.model = e.Model
				}
				e.Model = cmp.Or(r.model, r.e.Model)
				add(e.TS, e, false)
			}
			end(k, e.TS)
		case event.SessionEnd:
			end(k, e.TS)
		default:
			if r := open[k]; r != nil && e.SessionID != "" {
				r.end = e.TS
				if e.Model != "" && e.Kind != event.AgentStart {
					r.model = e.Model
				}
			}
		}
	}
	for k, r := range open {
		end(k, r.end)
	}
	for _, r := range runs {
		for t := r.start.Add(time.Minute); t.Before(r.end); t = t.Add(time.Minute) {
			add(t, r.e, false)
		}
	}
	for _, e := range acts {
		if e.Kind != event.Edit {
			continue
		}
		i := slices.IndexFunc(runs, func(r run) bool {
			return r.e.Project == e.Project && !e.TS.Before(r.start) && !e.TS.After(r.end)
		})
		if i >= 0 && e.Model == "" {
			e.Model = runs[i].e.Model // an edit during an agent's run counts as the agent's
		}
		add(e.TS, e, i < 0 && e.SessionID == "" && !strings.EqualFold(e.Category, AICoding))
	}
	sortHeartbeats(out)
	last := map[string]time.Time{}
	for i := range out {
		h := &out[i]
		if h.Yours {
			last[h.Project] = h.Time
		}
		l, ok := last[h.Project]
		h.Kept = ok && h.Time.Sub(l) <= Window
	}
	return out
}

// sortHeartbeats orders by time; at an equal time the agent's go before
// yours (an agent stops, then you type), then input order.
func sortHeartbeats(hbs []Heartbeat) {
	slices.SortStableFunc(hbs, func(a, b Heartbeat) int {
		yours := func(h Heartbeat) int {
			if h.Yours {
				return 1
			}
			return 0
		}
		return cmp.Or(a.Time.Compare(b.Time), cmp.Compare(yours(a), yours(b)), cmp.Compare(a.order, b.order))
	})
}

// Count sums the gaps (definition 4) for [from, to). Segments are clipped to
// the range, so counts of adjacent ranges add up.
func Count(hbs []Heartbeat, from, to time.Time) Result {
	r := Result{From: from, To: to, Heartbeats: hbs, ByProject: map[string]float64{}, Projects: map[string]Split{}}
	var prevAll, prevKept *Heartbeat
	for i := range hbs {
		h := &hbs[i]
		if h.Kept {
			if prevKept != nil && h.Project != OtherProject {
				r.segment(*prevKept, *h, true)
			}
			prevKept = h
		} else if prevAll != nil && !h.Yours {
			r.segment(*prevAll, *h, false)
		}
		prevAll = h
	}
	return r
}

func (r *Result) segment(prev, h Heartbeat, counted bool) {
	b := h.Time
	a := b.Add(-min(b.Sub(prev.Time), Timeout))
	if a.Before(r.From) {
		a = r.From
	}
	if b.After(r.To) {
		b = r.To
	}
	if !b.After(a) {
		return
	}
	s := Segment{Start: a, End: b, Project: h.Project, Tool: h.Tool, Category: h.Category(), Counted: counted}
	if s.Category != Coding {
		s.Model = h.Model
	}
	r.Segments = append(r.Segments, s)
	sec, p := s.Seconds(), r.Projects[h.Project]
	switch {
	case !counted:
		p.AINotCounted += sec
		r.AINotCounted += sec
	case s.Category == Coding:
		p.Coding += sec
		r.Coding += sec
	default:
		p.AI += sec
		r.AI += sec
	}
	r.Projects[h.Project] = p
	if counted {
		r.Total += sec
		r.ByProject[h.Project] += sec
	}
}

// isNew is definition 2. The device decides it once (e.New); for a record
// without that decision it is made here, in time order, from the text or
// its fingerprint.
func isNew(e event.Event, seen map[string]bool) bool {
	if e.New != nil {
		return *e.New
	}
	key, words := e.TextHash, e.Words
	if e.Text != "" && !chain.IsCommitted(e.Text) {
		key = Normalize(e.Text)
		words = len(strings.Fields(key))
	}
	if words < MinWords {
		return false
	}
	if key == "" {
		return true
	}
	k := e.Project + "\x00" + key
	if seen[k] {
		return false
	}
	seen[k] = true
	return true
}

// Normalize is how prompts are compared: lower case, letters and digits
// only, single spaces. "It still does not work!" and "it still does not
// work" are the same prompt.
func Normalize(s string) string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	return strings.Join(f, " ")
}

// NewPrompt is definition 2 for one prompt's text, given whether its
// normalized form was sent before in the project.
func NewPrompt(text string, sentBefore bool) bool {
	return !sentBefore && len(strings.Fields(Normalize(text))) >= MinWords
}

// Join puts together results counted over consecutive ranges (the prompt
// check's switch periods) into one result for [from, to).
func Join(rs []Result, from, to time.Time) Result {
	out := Result{From: from, To: to, ByProject: map[string]float64{}, Projects: map[string]Split{}}
	for _, r := range rs {
		out.Segments = append(out.Segments, r.Segments...)
		for _, h := range r.Heartbeats {
			if !h.Time.Before(r.From) && h.Time.Before(r.To) {
				out.Heartbeats = append(out.Heartbeats, h)
			}
		}
		out.Coding += r.Coding
		out.AI += r.AI
		out.AINotCounted += r.AINotCounted
		out.Total += r.Total
		for p, v := range r.ByProject {
			out.ByProject[p] += v
		}
		for p, v := range r.Projects {
			x := out.Projects[p]
			x.Coding += v.Coding
			x.AI += v.AI
			x.AINotCounted += v.AINotCounted
			out.Projects[p] = x
		}
	}
	slices.SortStableFunc(out.Segments, func(a, b Segment) int { return a.Start.Compare(b.Start) })
	return out
}
