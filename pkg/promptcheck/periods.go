package promptcheck

import (
	"slices"
	"time"

	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

// Period is a stretch of the switch's history: from From until the next
// period, the check was on or off. The server records one at each change
// of its prompt_check setting and sends the whole list to devices, so the
// rule is decided by when the work happened, not by the switch now:
// flipping it on never recounts earlier work under the cap rule (its
// prompts carry no hash and would count 0), and flipping it off never
// turns heartbeat/2 segments into a version mismatch.
type Period struct {
	From time.Time `json:"from"`
	On   bool      `json:"on"`
}

// OnAt reports whether the cap rule applies at t. A Set without periods
// (from an older server or audit package) applies its switch to all time;
// before the first period the check was off.
func (s *Set) OnAt(t time.Time) bool {
	if s == nil {
		return false
	}
	if len(s.Periods) == 0 {
		return s.On
	}
	on := false
	for _, p := range s.Periods {
		if p.From.After(t) {
			break
		}
		on = p.On
	}
	return on
}

// Window is a stretch of time under one rule.
type Window struct {
	From, To time.Time
	On       bool
}

// Windows cuts [from, to) at the switch's changes.
func (s *Set) Windows(from, to time.Time) []Window {
	if !to.After(from) {
		return nil
	}
	cur := Window{From: from, On: s.OnAt(from)}
	var out []Window
	if s != nil {
		for _, p := range s.Periods {
			if !p.From.After(from) || !p.From.Before(to) || p.On == cur.On {
				continue
			}
			cur.To = p.From
			out = append(out, cur)
			cur = Window{From: p.From, On: p.On}
		}
	}
	cur.To = to
	return append(out, cur)
}

// AnyOn reports whether the cap rule applies anywhere in [from, to].
func (s *Set) AnyOn(from, to time.Time) bool {
	for _, w := range s.Windows(from, to.Add(time.Nanosecond)) {
		if w.On {
			return true
		}
	}
	return false
}

// Version is the rule a count over [from, to] uses: heartbeat/2 when the
// check was on for any of it (the windows it was off are counted with
// heartbeat/1 inside it), else heartbeat/1. Device and server compute it
// from the same records and periods, so they agree.
func (s *Set) Version(from, to time.Time) string {
	return ledger.VersionFor(s.AnyOn(from, to))
}

// Span is the time range of evs.
func Span(evs []event.Event) (lo, hi time.Time) {
	for i, e := range evs {
		if i == 0 || e.TS.Before(lo) {
			lo = e.TS
		}
		if i == 0 || e.TS.After(hi) {
			hi = e.TS
		}
	}
	return lo, hi
}

// CountSplit counts [from, to) window by window: off windows with off,
// on windows with on, and joins the results.
func (s *Set) CountSplit(from, to time.Time, off, on func(from, to time.Time) ledger.Result) ledger.Result {
	ws := s.Windows(from, to)
	if len(ws) == 1 {
		if ws[0].On {
			return on(from, to)
		}
		return off(from, to)
	}
	rs := make([]ledger.Result, 0, len(ws))
	for _, w := range ws {
		if w.On {
			rs = append(rs, on(w.From, w.To))
		} else {
			rs = append(rs, off(w.From, w.To))
		}
	}
	return ledger.Join(rs, from, to)
}

// UserOnAt reports whether an account had its own prompt check setting on
// at t. The setting is on unless the person turned it off: before the first
// change, and with no history at all, it is on.
func UserOnAt(user []Period, t time.Time) bool {
	on := true
	for _, p := range user {
		if p.From.After(t) {
			break
		}
		on = p.On
	}
	return on
}

// Resolver says, for one device's prompts sent in [from, to) that are not
// repeats, how many its prompt chain holds (n) and how many of those are
// settled: they have a verdict, or the server closed them without one
// (their text did not match, or never came).
type Resolver func(from, to time.Time) (n, settled int)

// Forever ends the last stretch of a history.
var Forever = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// Effective is the history a device's records are counted with
// (docs/RULE.md "When the AI check is on"): the check applies at t when the
// server had it on (server, or serverOn when the server sent no history)
// and either the account had its own setting on (user) or t lies in a
// stretch the account had it off whose prompts were all checked since.
//
// A stretch is a longest run of time with the server's check on and the
// account's setting off, cut where the device's chain starts (chainFrom):
// earlier records carry no prompt hashes, so they never switch. It switches
// to the check once the device's chain holds at least one of its prompts
// and resolve says every one is settled. Until then it is counted as if the
// check were off. A device with no chain (an older client; chainFrom zero)
// never switches a stretch.
func Effective(server []Period, serverOn bool, user []Period, chainFrom time.Time, resolve Resolver) []Period {
	sv := &Set{On: serverOn, Periods: server}
	points := []time.Time{time.UnixMilli(0).UTC()}
	for _, p := range server {
		points = append(points, p.From)
	}
	for _, p := range user {
		points = append(points, p.From)
	}
	if !chainFrom.IsZero() {
		points = append(points, chainFrom)
	}
	slices.SortFunc(points, func(a, b time.Time) int { return a.Compare(b) })
	points = slices.CompactFunc(points, func(a, b time.Time) bool { return a.Equal(b) })

	type seg struct {
		from    time.Time
		on, off bool // on: the check applies; off: a stretch that may switch
	}
	segs := make([]seg, len(points))
	for i, t := range points {
		s, u := sv.OnAt(t), UserOnAt(user, t)
		segs[i] = seg{from: t, on: s && u, off: s && !u && !chainFrom.IsZero() && !t.Before(chainFrom)}
	}
	for i := 0; i < len(segs); {
		if !segs[i].off {
			i++
			continue
		}
		j := i
		for j < len(segs) && segs[j].off {
			j++
		}
		to := Forever
		if j < len(segs) {
			to = segs[j].from
		}
		if resolve != nil {
			if n, settled := resolve(segs[i].from, to); n > 0 && settled >= n {
				for k := i; k < j; k++ {
					segs[k].on = true
				}
			}
		}
		i = j
	}
	var out []Period
	for i, s := range segs {
		if i == 0 || s.on != out[len(out)-1].On {
			out = append(out, Period{From: s.from, On: s.on})
		}
	}
	return out
}
