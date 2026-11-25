// Package audit is the one analysis of a segment. seatimed runs it on
// every segment it receives and the device uses Claim for its claim, so
// both numbers come from the same code and agree to the second.
//
// It checks the device's hash chain against the checkpoints the server
// received in real time, drops records that were written after the fact,
// removes time no checkpoint vouches for, and credits the rest with the
// one-page rule (pkg/ledger). It reads nothing but the bundle and the
// checkpoints; the server's index of credited minutes enters through Overlap.
package audit

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
	"seatime/pkg/promptcheck"
)

// Live-anchoring constants.
const (
	// ClockSkew: a record may be timestamped at most this long after the
	// server first received a checkpoint covering it.
	ClockSkew = 5 * time.Minute
	// BackdateSlack: a record may be timestamped at most this long before
	// the server time of the checkpoint that preceded it in the chain (hook
	// races, a heartbeat queued by an editor plugin).
	BackdateSlack = 10 * time.Minute
	// AnchorGap: checkpoints further apart than this do not pin down when
	// the records between them were made (the daemon syncs every ~90 s).
	AnchorGap = 45 * time.Minute
	// AnchorGrace: credit this close before the covering checkpoint arrived
	// is still live (a laptop waking up syncs within ~90 s).
	AnchorGrace = 15 * time.Minute
	// MaxSpan bounds the time one segment covers (the rule works on 5 s
	// slots, so a longer span costs memory for nothing).
	MaxSpan = 62 * 24 * time.Hour
)

// Window is a stretch of time.
type Window struct{ From, To time.Time }

// Input is a bundle and the server's checkpoints for its device.
type Input struct {
	Bundle api.Bundle
	// Checkpoints with Seq >= the bundle's first entry, and Prev, the last
	// one before it (nil if none). Server clock.
	Checkpoints   []api.ServerCheckpoint
	Prev          *api.ServerCheckpoint
	DeviceCreated time.Time
	// PromptCheck is the prompt check's switch and stored verdicts
	// (pkg/promptcheck); nil when the switch is off. With it on, each
	// prompt counts as new only by its verdict.
	PromptCheck *promptcheck.Set
}

// Deduction is recomputed time that is not credited, and why.
type Deduction struct {
	Code    string  // backdated | unanchored | overlap
	Seconds float64 // counted seconds not credited
	Detail  string
}

// Analysis is the outcome.
type Analysis struct {
	Problems     []string
	Events       []event.Event // revealed records (project keys mapped), by time
	Hidden       int           // entries with only a leaf hash
	SeqLo, SeqHi int64
	Head         string
	Backdated    int
	Future       int
	Unanchored   []Window
	// Result is the rule over the kept records (backdated ones dropped).
	Result ledger.Result
	// Raw is the project's total over every revealed record: what the
	// client claims.
	Raw ledger.Bucket
	// Buckets are the project's credited minutes after every deduction.
	Buckets    []ledger.Bucket
	Deductions []Deduction
	// Scored is the credited total (Buckets summed; counts from Raw).
	Scored ledger.Bucket
	// Pending counts prompts still waiting for their check (prompt check
	// on, no verdict yet): they do not open the 10 minutes until one
	// arrives. PendingSeconds is the project's time that would count if
	// they all passed; it is not in any total.
	Pending        int
	PendingSeconds float64
}

func (a *Analysis) problem(f string, args ...any) {
	a.Problems = append(a.Problems, fmt.Sprintf(f, args...))
}

// Credit is the rule over a segment's revealed records, with others,
// the bare times of counted heartbeats in other projects (Bundle.Others),
// splitting the gaps between projects as on the device.
func Credit(evs []event.Event, others ...time.Time) ledger.Result {
	if len(evs) == 0 {
		return ledger.Result{ByProject: map[string]float64{}, Projects: map[string]ledger.Split{}}
	}
	lo, hi := evs[0].TS, evs[0].TS
	for _, e := range evs {
		if e.TS.Before(lo) {
			lo = e.TS
		}
		if e.TS.After(hi) {
			hi = e.TS
		}
	}
	return ledger.Count(ledger.Mark(ledger.Heartbeats(evs), others), lo.Add(-ledger.Timeout), hi.Add(ledger.Timeout))
}

// CreditWith is Credit under the prompt check (pkg/promptcheck): where pc
// says the check was on (its switch periods), the cap rule
// (ledger.CountCap, rule heartbeat/2) over records whose prompts pc has
// already decided (Set.Apply or Set.Hopeful, matching hopeful), with
// until[i] the forward reach of others[i]; where it was off, Credit's
// rule. With pc nil, or off for the whole range, it is Credit.
func CreditWith(pc *promptcheck.Set, hopeful bool, evs []event.Event, others, until []time.Time) ledger.Result {
	if len(evs) == 0 {
		return ledger.Result{ByProject: map[string]float64{}, Projects: map[string]ledger.Split{}}
	}
	lo, hi := promptcheck.Span(evs)
	if !pc.AnyOn(lo, hi) {
		return Credit(evs, others...)
	}
	from, to := lo.Add(-ledger.Timeout), hi.Add(ledger.MaxReach)
	on := func(a, b time.Time) ledger.Result {
		return ledger.CountCap(ledger.MarkCap(ledger.HeartbeatsCap(evs, pc.Cap(hopeful)), others, until), a, b)
	}
	off := func(a, b time.Time) ledger.Result {
		return ledger.Count(ledger.Mark(ledger.Heartbeats(evs), others), a, b)
	}
	return pc.CountSplit(from, to, off, on)
}

// Claim is the client's claim for project over its revealed records and
// the bundle's markers.
func Claim(evs []event.Event, project string, others ...time.Time) ledger.Bucket {
	return ClaimWith(nil, evs, project, others, nil)
}

// ClaimWith is Claim under the prompt check (see CreditWith).
func ClaimWith(pc *promptcheck.Set, evs []event.Event, project string, others, until []time.Time) ledger.Bucket {
	t := ledger.Sum(ledger.Buckets(CreditWith(pc, false, evs, others, until), evs), project)
	t.Project = project
	return t
}

// Revealed returns the bundle's revealed records, sorted by time, with
// local project keys mapped to the bundle's server key.
func Revealed(b api.Bundle) []event.Event {
	var out []event.Event
	for _, en := range b.Entries {
		if en.Event == nil {
			continue
		}
		out = append(out, mapProject(*en.Event, b.Project))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out
}

func mapProject(e event.Event, key string) event.Event {
	if e.Project != key && api.ProjectKeyMatches(e.Project, key) {
		e.Project = key
	}
	return e
}

// Analyze checks the bundle against the checkpoints and credits it.
func Analyze(in Input) *Analysis {
	b := in.Bundle
	a := &Analysis{}
	a.Raw.Project, a.Scored.Project = b.Project, b.Project
	if len(b.Entries) == 0 {
		a.problem("bundle has no entries")
		return a
	}
	a.SeqLo, a.SeqHi = b.Entries[0].Seq, b.Entries[len(b.Entries)-1].Seq
	cps := slices.Clone(in.Checkpoints)
	sort.Slice(cps, func(i, j int) bool { return cps[i].Seq < cps[j].Seq })
	cps = slices.DeleteFunc(cps, func(c api.ServerCheckpoint) bool { return c.Seq < a.SeqLo || c.Seq > a.SeqHi })
	if len(cps) == 0 {
		a.problem("no server-held checkpoint falls within seq %d..%d, so nothing anchors this range", a.SeqLo, a.SeqHi)
	}
	var cc []chain.Checkpoint
	for _, c := range cps {
		cc = append(cc, chain.Checkpoint{Seq: c.Seq, Hash: c.Hash})
	}
	head, err := chain.Verify(b.StartHash, b.Entries, cc)
	if err != nil {
		a.problem("%v", err)
	}
	a.Head = head
	if a.SeqLo <= 1 {
		if b.StartHash != chain.Genesis {
			a.problem("range starts at seq 1 but start_hash is not the genesis hash")
		}
	} else if p := in.Prev; p != nil && p.Seq == a.SeqLo-1 && p.Hash != b.StartHash {
		a.problem("start_hash does not match the checkpoint stored for seq %d", a.SeqLo-1)
	}
	if n := len(cps); n > 0 && a.SeqHi > cps[n-1].Seq {
		a.problem("%d entries after the last synced checkpoint (seq %d) are unanchored; send a range that ends at a synced checkpoint", a.SeqHi-cps[n-1].Seq, cps[n-1].Seq)
	}

	// Real-time anchoring. Record n was appended after the checkpoint before
	// it (server time lo) and before the first checkpoint covering it (hi):
	// TS > hi+skew is clock-forged (a problem), TS < lo-slack is backdated
	// (not credited), and a wide [lo, hi] around project records means the
	// device was not syncing, so nothing shows they were made live.
	all := cps
	if in.Prev != nil {
		all = append([]api.ServerCheckpoint{*in.Prev}, cps...)
	}
	var keep []event.Event
	for _, en := range b.Entries {
		if en.Event == nil {
			a.Hidden++
			continue
		}
		e := mapProject(*en.Event, b.Project)
		a.Events = append(a.Events, e)
		i := sort.Search(len(all), func(i int) bool { return all[i].Seq >= en.Seq })
		lo := in.DeviceCreated
		if i > 0 {
			lo = all[i-1].ServerTS
		}
		if i < len(all) {
			hi := all[i].ServerTS
			if e.TS.After(hi.Add(ClockSkew)) {
				a.Future++
			}
			if hi.Sub(lo) > AnchorGap && e.Project == b.Project {
				if n := len(a.Unanchored); n == 0 || !a.Unanchored[n-1].From.Equal(lo) {
					a.Unanchored = append(a.Unanchored, Window{lo, hi.Add(-AnchorGrace)})
				}
			}
		}
		if !lo.IsZero() && e.TS.Before(lo.Add(-BackdateSlack)) {
			a.Backdated++
			continue
		}
		keep = append(keep, e)
	}
	if a.Future > 0 {
		a.problem("%d records are timestamped after the server first received a checkpoint covering them (backfilled or clock-forged)", a.Future)
	}
	sort.SliceStable(a.Events, func(i, j int) bool { return a.Events[i].TS.Before(a.Events[j].TS) })
	sort.SliceStable(keep, func(i, j int) bool { return keep[i].TS.Before(keep[j].TS) })
	if n := len(a.Events); n > 1 {
		if span := a.Events[n-1].TS.Sub(a.Events[0].TS); span > MaxSpan {
			a.problem("not counted: the records span %.0f days, more than the %.0f one segment may cover; send a shorter range",
				span.Hours()/24, MaxSpan.Hours()/24)
			return a
		}
	}
	pc := in.PromptCheck
	if want := pc.Version(promptcheck.Span(a.Events)); b.ScoreVersion != want {
		if want == ledger.CapVersion {
			a.problem("this server checks prompts with an AI model to count time (rule %s), and the SeaTime that sent these records counts with %q: SeaTime needs an update", want, b.ScoreVersion)
		} else {
			a.problem("rule version mismatch: client %q, server %q", b.ScoreVersion, want)
		}
	}

	var hopeful []event.Event
	if pc.AnyOn(promptcheck.Span(a.Events)) {
		hopeful = pc.Hopeful(a.Events)
		a.Events, a.Pending = pc.Apply(a.Events)
		keep, _ = pc.Apply(keep)
	}
	raw := CreditWith(pc, false, a.Events, b.Others, b.OthersUntil)
	if a.Pending > 0 {
		a.PendingSeconds = max(0, CreditWith(pc, true, hopeful, b.Others, b.OthersUntil).ByProject[b.Project]-raw.ByProject[b.Project])
	}
	a.Raw = ledger.Sum(ledger.Buckets(raw, a.Events), b.Project)
	a.Raw.Project = b.Project
	for _, c := range []struct {
		name         string
		claimed, got float64
	}{{"hand", b.Claimed.CodingSeconds, a.Raw.CodingSeconds}, {"AI", b.Claimed.AISeconds, a.Raw.AISeconds}} {
		if math.Abs(c.claimed-c.got) > 1 {
			a.problem("claimed %s time %.0fs differs from the recomputation %.0fs", c.name, c.claimed, c.got)
		}
	}

	a.Result = raw
	kept := a.Events
	if a.Backdated > 0 {
		a.Result, kept = CreditWith(pc, false, keep, b.Others, b.OthersUntil), keep
	}
	var bs []ledger.Bucket
	for _, bk := range ledger.Buckets(a.Result, kept) {
		if bk.Project == b.Project {
			bs = append(bs, bk)
		}
	}
	if a.Backdated > 0 {
		t := ledger.Sum(bs, "")
		d := Deduction{Code: "backdated", Seconds: max(0, a.Raw.Seconds()-t.Seconds())}
		d.Detail = fmt.Sprintf("%d records are timestamped more than %s before a checkpoint this device had already sent (clock wound back, or written after the fact); %.2fh that depends on them is not credited",
			a.Backdated, BackdateSlack, d.Seconds/3600)
		a.Deductions = append(a.Deductions, d)
	}
	if len(a.Unanchored) > 0 {
		un := Deduction{Code: "unanchored"}
		var widest time.Duration
		for _, w := range a.Unanchored {
			widest = max(widest, w.To.Sub(w.From)+AnchorGrace)
		}
		bs = slices.DeleteFunc(bs, func(bk ledger.Bucket) bool {
			for _, w := range a.Unanchored {
				if !bk.Start.Before(w.From) && bk.Start.Before(w.To) {
					un.Seconds += bk.Seconds()
					return true
				}
			}
			return false
		})
		if un.Seconds > 0 {
			un.Detail = fmt.Sprintf("%.2fh was recorded while this device sent no checkpoints (up to %s without one: offline, or the daemon was stopped); nothing shows it happened live, so it is not credited",
				un.Seconds/3600, widest.Round(time.Minute))
			a.Deductions = append(a.Deductions, un)
		}
	}
	a.Buckets = bs
	a.total()
	return a
}

// Overlap removes, minute by minute, time earlier segments of the same
// account already hold (claimed maps a unix minute to seconds): no minute
// holds more than 60 s across segments. subs names those segments.
func (a *Analysis) Overlap(claimed map[int64]float64, subs []string) {
	ov := Deduction{Code: "overlap"}
	for i := range a.Buckets {
		bk := &a.Buckets[i]
		sec := bk.Seconds()
		theirs := claimed[bk.Start.Unix()/60]
		if theirs <= 0 || sec <= 0 {
			continue
		}
		excess := min(sec, max(0, sec+theirs-60))
		if excess <= 0 {
			continue
		}
		f := 1 - excess/sec
		ov.Seconds += excess
		bk.ScaleCounted(f)
	}
	if ov.Seconds <= 0 {
		return
	}
	ov.Detail = fmt.Sprintf("%.2fh of these minutes were already credited to this account by other records %v (another device or project, or the same records sent again); it is not credited again",
		ov.Seconds/3600, subs)
	a.Deductions = append(a.Deductions, ov)
	a.total()
}

func (a *Analysis) total() {
	t := ledger.Sum(a.Buckets, "")
	t.Project = a.Raw.Project
	t.Prompts, t.Edits, t.Commits, t.Runs = a.Raw.Prompts, a.Raw.Edits, a.Raw.Commits, a.Raw.Runs
	t.AINotCountedSeconds = a.Raw.AINotCountedSeconds
	a.Scored = t
}

// Minutes is the credited wall time per unix minute (for the server's
// index of credited minutes).
func (a *Analysis) Minutes() map[int64]float64 {
	out := map[int64]float64{}
	for _, bk := range a.Buckets {
		if sec := bk.Seconds(); sec > 0 {
			out[bk.Start.Unix()/60] += sec
		}
	}
	return out
}

// Hours is the number for an application's hours field given the server's
// status: the credited hours, or 0 when the segment failed.
func Hours(scoredSec float64, status string) float64 {
	if status == "failed" {
		return 0
	}
	return ledger.Hours(scoredSec)
}

// ---- signatures ----

// Digest is what a device signs: sha256 of the bundle JSON with Sig="".
func Digest(b api.Bundle) [32]byte {
	b.Sig = ""
	raw, _ := json.Marshal(b)
	return sha256.Sum256(raw)
}

// CheckSignature verifies the bundle's ed25519 signature.
func CheckSignature(pubB64 string, b api.Bundle) error {
	raw, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("bad ed25519 public key")
	}
	sig, err := base64.StdEncoding.DecodeString(b.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("bad signature encoding")
	}
	d := Digest(b)
	if !ed25519.Verify(ed25519.PublicKey(raw), d[:], sig) {
		return fmt.Errorf("signature does not match the device key")
	}
	return nil
}
