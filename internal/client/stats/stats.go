// Package stats turns stored events into what `seatime today`, the local
// dashboard, the status line and the sync loop show. All of them count with
// the one-page rule (pkg/ledger) through Credit, so the hours on this
// machine and the hours the server adds up are the same numbers.
package stats

import (
	"fmt"
	"time"

	"seatime/internal/client/store"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
	"seatime/pkg/promptcheck"
)

// Lead is how much history before a range is fed to the rule: an agent
// run that began before the range is whole, and so is the window after
// your last heartbeat.
const Lead = 24 * time.Hour

// Events are the events the rule needs for [from, to): everything from Lead
// before from, and the last edit or prompt before that, so the first gap
// after a long break is counted as on the device's whole log (each gap
// counts up to 2 minutes, however long the break).
func Events(st *store.Store, from, to time.Time) ([]event.Event, error) {
	evs, err := st.Events(store.Query{Since: from.Add(-Lead), Until: to})
	if err != nil {
		return nil, err
	}
	prev, err := st.Events(store.Query{Until: from.Add(-Lead), Kinds: []event.Kind{event.Edit, event.Prompt}, Limit: 1, Desc: true})
	if err != nil {
		return nil, err
	}
	out := append(prev, evs...)
	if set := PromptChecks(st); set != nil {
		out, _ = set.Apply(out)
	}
	return out, nil
}

// PromptChecks is the prompt check's history and the verdicts on this
// machine, or nil while it was never on (pkg/promptcheck). Events applies
// it, so every count on this machine uses the verdicts as the server does.
// With a server that takes prompt hashes, the history is
// promptcheck.Effective over the server's switch and the account's own
// setting, from this device's chain: a stretch the account had the check
// off counts with the check once every prompt in it has a verdict.
func PromptChecks(st *store.Store) *promptcheck.Set {
	on, periods := st.PromptCheckOn(), st.CheckPeriods()
	if st.ChainOn() {
		sv, _ := st.GetMeta(store.MetaServerCheck)
		periods = promptcheck.Effective(st.Periods(store.MetaServerPeriods), sv == "1", st.Periods(store.MetaUserPeriods), st.ChainFrom(), st.ChainResolve)
		on = (&promptcheck.Set{Periods: periods}).OnAt(time.Now())
	}
	ever := on
	for _, p := range periods {
		ever = ever || p.On
	}
	if !ever {
		return nil
	}
	pass, minutes, err := st.CheckVerdicts()
	if err != nil {
		pass, minutes = map[string]bool{}, nil // nothing known: every checked prompt waits
	}
	return &promptcheck.Set{On: on, Pass: pass, Min: minutes, Periods: periods}
}

// Compute is the rule for evs (already decided by Events): heartbeat/1
// where the prompt check was off, the cap rule (heartbeat/2) where it was
// on (the server's switch periods).
func Compute(set *promptcheck.Set, hopeful bool, evs []event.Event, from, to time.Time) ledger.Result {
	if !set.AnyOn(from, to) {
		return ledger.Compute(evs, from, to)
	}
	return set.CountSplit(from, to,
		func(a, b time.Time) ledger.Result { return ledger.Compute(evs, a, b) },
		func(a, b time.Time) ledger.Result { return ledger.ComputeCap(evs, a, b, set.Cap(hopeful)) })
}

// Pending is the time in [from, to) that waits for the prompt check: what
// would count if every prompt still waiting for its check passed. 0 while
// the check is off.
func Pending(st *store.Store, from, to time.Time) (float64, error) {
	set := PromptChecks(st)
	if set == nil {
		return 0, nil
	}
	evs, err := st.Events(store.Query{Since: from.Add(-Lead), Until: to})
	if err != nil {
		return 0, err
	}
	now, n := set.Apply(evs)
	if n == 0 {
		return 0, nil
	}
	return max(0, Compute(set, true, set.Hopeful(evs), from, to).Total-Compute(set, false, now, from, to).Total), nil
}

// Credit applies the rule to [from, to) with the history Events loads. It
// returns the events it read (for counts).
func Credit(st *store.Store, from, to time.Time) (ledger.Result, []event.Event, error) {
	evs, err := Events(st, from, to)
	if err != nil {
		return ledger.Result{}, nil, err
	}
	return Compute(PromptChecks(st), false, evs, from, to), evs, nil
}

func StartOfDay(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

// Dur renders seconds as "1h 05m" / "7m" / "40s".
func Dur(sec float64) string {
	s := int(sec + 0.5)
	switch {
	case s >= 3600:
		return fmt.Sprintf("%dh %02dm", s/3600, s%3600/60)
	case s >= 60:
		return fmt.Sprintf("%dm", s/60)
	}
	return fmt.Sprintf("%ds", s)
}

// PendingByProject is Pending split by project (local keys).
func PendingByProject(st *store.Store, from, to time.Time) (map[string]float64, error) {
	set := PromptChecks(st)
	if set == nil {
		return nil, nil
	}
	evs, err := st.Events(store.Query{Since: from.Add(-Lead), Until: to})
	if err != nil {
		return nil, err
	}
	now, n := set.Apply(evs)
	out := map[string]float64{}
	if n == 0 {
		return out, nil
	}
	hope, have := Compute(set, true, set.Hopeful(evs), from, to).ByProject, Compute(set, false, now, from, to).ByProject
	for p, sec := range hope {
		if d := sec - have[p]; d > 0.5 {
			out[p] = d
		}
	}
	return out, nil
}
