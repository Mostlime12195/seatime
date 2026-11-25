package sim

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Persona is one simulated person (honest) or attack.
type Persona struct {
	Name   string
	Honest bool
	Desc   string
	// Truth is the fair credit, in struggle hours (honest
	// personas only), computed from the persona's real work time when Frac
	// is set (Truth = Frac × work hours) so jittered timings still hold.
	Frac [2]float64
	// Gate personas run in the fast `go test` regression subset.
	Gate bool
	// Run drives the world and reports what happened.
	Run func(w *World, seed uint64) (*Outcome, error)
	// Want is the verdict an attack must reach in the regression gate
	// (honest personas must pass). "" = report only.
	Want string
}

// Outcome is what one persona produced.
type Outcome struct {
	Subs     []*SubResult
	Credited float64 // hours a program would enter (sum of submissions handed in, or compat API hours)
	Presence float64
	Work     float64 // honest: real work hours; attack: attacker's real human effort hours
	Copycat  float64 // attack: effort for someone reusing a published script (0 = same as Work)
	Effort   string  // how the attacker's effort was counted
	Compat   float64 // hours on the Hackatime-compatible API (bucket path), -1 = n/a
	Flags    []string
	Notes    []string
	// Blocked marks an attack stopped before any submission existed
	// (e.g. the local endpoint refused it).
	Blocked bool
}

// Result is a persona's row in the report.
type Result struct {
	P          *Persona
	O          *Outcome
	Err        error
	Truth      [2]float64
	Pass       bool    // honest: credited within Truth, verified, not refused
	Multiplier float64 // attack: credited / effort
	Verdict    string  // attack: blocked | rejected | discounted | succeeds
	Elapsed    time.Duration
}

// FlagCode is the code part of "code: detail".
func FlagCode(f string) string {
	c, _, _ := strings.Cut(f, ":")
	return strings.TrimSpace(c)
}

func (o *Outcome) collect() {
	seen := map[string]bool{}
	for _, f := range o.Flags {
		seen[f] = true
	}
	for _, s := range o.Subs {
		if s == nil {
			continue
		}
		var fl []string
		if s.Err != "" {
			fl = append(fl, "refused: "+s.Err)
		} else if !s.Verified {
			for _, p := range s.Problems {
				fl = append(fl, "unverified: "+p)
			}
		}
		for _, f := range fl {
			if !seen[f] {
				seen[f] = true
				o.Flags = append(o.Flags, f)
			}
		}
	}
}

// rejected: the server refused a submission or did not verify it.
func (o *Outcome) rejected() bool {
	for _, f := range o.Flags {
		if strings.HasPrefix(f, "refused") || strings.HasPrefix(f, "unverified") {
			return true
		}
	}
	return false
}

// FlagCodes returns the distinct flag codes, sorted.
func (o *Outcome) FlagCodes() []string {
	m := map[string]bool{}
	for _, f := range o.Flags {
		m[FlagCode(f)] = true
	}
	return sortedKeys(m)
}

// RunPersona runs p in a fresh world under dir.
func RunPersona(p *Persona, dir string, seed uint64) *Result {
	return runPersona(p, filepath.Join(dir, p.Name), seed, WorldOpts{}, nil, false)
}

// runPersona runs p in a fresh world in wdir. after (if set) runs against the
// live world once the persona is done; keep leaves wdir (the server's
// seatimed.db) in place for the caller.
func runPersona(p *Persona, wdir string, seed uint64, wo WorldOpts, after func(w *World, o *Outcome), keep bool) *Result {
	t0 := time.Now()
	r := &Result{P: p}
	_ = os.RemoveAll(wdir)
	w, err := NewWorldWith(wdir, wo)
	if err != nil {
		r.Err = err
		return r
	}
	defer func() {
		w.Close()
		if !keep {
			_ = os.RemoveAll(wdir)
		}
	}()
	o, err := p.Run(w, seed)
	if err == nil && o != nil && after != nil {
		after(w, o)
	}
	r.O, r.Err, r.Elapsed = o, err, time.Since(t0)
	if err != nil || o == nil {
		return r
	}
	o.collect()
	if p.Honest {
		r.Truth = [2]float64{p.Frac[0] * o.Work, p.Frac[1] * o.Work}
		allOK := len(o.Subs) > 0
		for _, s := range o.Subs {
			if s == nil || !s.Verified {
				allOK = false
			}
		}
		r.Pass = allOK && o.Credited >= r.Truth[0]-0.005 && o.Credited <= r.Truth[1]+0.005 && !o.rejected()
		return r
	}
	if o.Work > 0 {
		r.Multiplier = o.Credited / o.Work
	}
	switch {
	case o.Blocked || o.Credited < 0.05:
		r.Verdict = "blocked"
	case o.rejected():
		r.Verdict = "rejected"
	case r.Multiplier <= 1.0:
		r.Verdict = "discounted"
	default:
		r.Verdict = "succeeds"
	}
	return r
}

// FormatTable renders results as a markdown table.
func FormatTable(rs []*Result) string {
	var b strings.Builder
	honest := false
	for _, r := range rs {
		if r.P.Honest {
			honest = true
		}
	}
	if honest {
		b.WriteString("| persona | work h | credited h | presence h | fair range h | flags | result |\n|---|---|---|---|---|---|---|\n")
		for _, r := range rs {
			if !r.P.Honest {
				continue
			}
			if r.Err != nil || r.O == nil {
				fmt.Fprintf(&b, "| %s | | | | | | ERROR %v |\n", r.P.Name, r.Err)
				continue
			}
			res := "FAIL"
			if r.Pass {
				res = "pass"
			}
			fmt.Fprintf(&b, "| %s | %.2f | %.2f | %.2f | %.2f–%.2f | %s | %s |\n", r.P.Name, r.O.Work, r.O.Credited, r.O.Presence,
				r.Truth[0], r.Truth[1], strings.Join(r.O.FlagCodes(), ", "), res)
		}
		b.WriteString("\n")
	}
	var atk []*Result
	for _, r := range rs {
		if !r.P.Honest {
			atk = append(atk, r)
		}
	}
	if len(atk) > 0 {
		b.WriteString("| attack | effort h | credited h | multiplier | copycat | flags | verdict |\n|---|---|---|---|---|---|---|\n")
		for _, r := range atk {
			if r.Err != nil || r.O == nil {
				fmt.Fprintf(&b, "| %s | | | | | | ERROR %v |\n", r.P.Name, r.Err)
				continue
			}
			cc := "same"
			if r.O.Copycat > 0 {
				cc = fmt.Sprintf("%.1f×", r.O.Credited/r.O.Copycat)
			}
			fmt.Fprintf(&b, "| %s | %.2f | %.2f | %s | %s | %s | %s |\n", r.P.Name, r.O.Work, r.O.Credited, fmtMult(r.Multiplier, r.O), cc,
				strings.Join(r.O.FlagCodes(), ", "), r.Verdict)
		}
	}
	s := b.String()

	return s
}

func fmtMult(m float64, o *Outcome) string {
	if o.Work <= 0 && o.Credited > 0 {
		return "∞"
	}
	return fmt.Sprintf("%.2f×", m)
}

// Details renders per-persona flag details and notes.
func Details(rs []*Result) string {
	var b strings.Builder
	for _, r := range rs {
		if r.O == nil {
			continue
		}
		fmt.Fprintf(&b, "### %s\n%s\n", r.P.Name, r.P.Desc)
		if r.O.Effort != "" {
			fmt.Fprintf(&b, "- effort: %s\n", r.O.Effort)
		}
		for _, s := range r.O.Subs {
			if s == nil {
				continue
			}
			st := "refused"
			if s.Export != nil {
				st = s.Export.Status
			}
			fmt.Fprintf(&b, "- submission %s %s: %s, %.2f h entered (struggle %.2f h, presence %.2f h)\n", s.ID, s.Project, st, s.Hours(),
				s.Scored.Seconds()/3600, s.Scored.Seconds()/3600)
		}
		if r.O.Compat >= 0 {
			fmt.Fprintf(&b, "- Hackatime-compatible API shows %.2f h\n", r.O.Compat)
		}
		fl := append([]string(nil), r.O.Flags...)
		sort.Strings(fl)
		for _, f := range fl {
			fmt.Fprintf(&b, "- flag: %s\n", f)
		}
		for _, n := range r.O.Notes {
			fmt.Fprintf(&b, "- note: %s\n", n)
		}
		b.WriteString("\n")
	}
	return b.String()
}
