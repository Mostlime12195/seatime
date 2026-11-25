package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
	"seatime/pkg/promptcheck"
)

// The prompt check (pkg/promptcheck, docs/RULE.md definition 2), behind
// SEATIME_PROMPT_CHECK. A device sends a finished turn; the server checks
// that the text matches the prompt hash the device's log committed to,
// asks the model, keeps the yes/no answers, the hash and the model's name,
// and drops the text and the project facts sent with it (the project's
// name, file names and README). Neither is ever logged, stored or put in
// an error.

// SetJudge replaces the model that answers prompt checks (tests use a
// fake). It does not turn the switch on.
func (s *Server) SetJudge(j promptcheck.Judge) { s.judge = j }

// promptCheckOn is the switch (setting prompt_check). A missing key or a
// judge that does not answer does not turn it off: checks then answer 503
// and the devices keep their prompts queued (pending, never decided).
func (s *Server) promptCheckOn() bool { return s.pol().PromptCheck }

// judgeAll asks each judge in the policy's list in turn until one answers.
func (s *Server) judgeAll(ctx context.Context, state string, qs map[string]promptcheck.Question) (promptcheck.Answer, error) {
	var err error
	if s.judge == nil {
		return promptcheck.Answer{}, errors.New("no judge")
	}
	for _, t := range s.pol().Models {
		var ans promptcheck.Answer
		ans, err = s.judge.Judge(ctx, t, state, qs)
		if err == nil {
			s.judgeOK()
			return ans, nil
		}
		s.judgeFailed(err)
		if ctx.Err() != nil {
			break
		}
	}
	if err == nil {
		err = errors.New("no judge model configured")
	}
	return promptcheck.Answer{}, err
}

// waitLater answers that the check cannot be made now: the device keeps
// the prompt queued and tries again (never decided, never dropped).
func waitLater(w http.ResponseWriter, after time.Duration, msg string) {
	w.Header().Set("Retry-After", strconv.Itoa(int(after.Seconds())))
	jsonErr(w, http.StatusServiceUnavailable, msg)
}

func (s *Server) handlePromptCheck(w http.ResponseWriter, r *http.Request) {
	if !s.promptCheckOn() {
		jsonErr(w, 404, "the prompt check is switched off on this server")
		return
	}
	dev, user, ok := s.authDevice(w, r)
	if !ok {
		return
	}
	if !s.lim.Allow("check:"+dev.ID, 0.5, 30) {
		jsonErr(w, 429, "rate limited")
		return
	}
	var req api.PromptCheckReq
	if err := decodeBody(w, r, &req, 1<<20, 2<<20); err != nil {
		jsonErr(w, 400, "bad request") // the decoder's message can quote the body
		return
	}
	if req.DeviceID != dev.ID {
		jsonErr(w, 403, "device_id does not match token")
		return
	}
	if s.clientTooOld(dev.ClientVersion) { // the device keeps the turn queued and retries after updating
		jsonErr(w, http.StatusUpgradeRequired, s.updateClientMsg())
		return
	}
	ctx := r.Context()
	// the prompt's entry in the device's chain (promptchain.go): its time
	// and project are the chain's, and text that does not match closes it
	row, chained, err := s.chainEntry(ctx, dev.ID, req.CheckHash, req.PromptTS)
	if err != nil {
		s.log.Error("prompt chain entry", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	refuse := func(code int, msg string) {
		if row != nil {
			if err := s.store.SetChainStatus(ctx, dev.ID, row.Commit, store.ChainMismatch); err != nil {
				s.log.Error("prompt chain status", "err", err)
			}
		}
		jsonErr(w, code, msg)
	}
	if msg := checkShape(&req); msg != "" {
		refuse(400, msg)
		return
	}
	if row == nil && chained {
		// the device keeps a chain but this prompt's hash has not come yet:
		// it sends the hash, then the check again
		w.Header().Set("Retry-After", "30")
		jsonErr(w, 409, "the prompt's hash has not reached the server yet; the device sends it, then tries again")
		return
	}
	if row != nil {
		switch {
		case row.Repeat:
			jsonErr(w, 422, "a repeated prompt is not checked")
			return
		case row.Status != store.ChainOpen:
			jsonErr(w, 422, "this prompt was closed without a check ("+row.Status+")")
			return
		case !req.PromptTS.IsZero() && !req.PromptTS.UTC().Truncate(time.Millisecond).Equal(row.TS):
			refuse(422, "the prompt's time does not match its hash's")
			return
		}
		req.PromptTS = row.TS
	} else if now := s.now(); req.PromptTS.After(now.Add(clockSkew)) || !req.PromptTS.IsZero() &&
		(req.PromptTS.Before(now.Add(-time.Duration(s.pol().LateDays*float64(24*time.Hour)))) || req.PromptTS.Before(dev.Created.Add(-clockSkew))) {
		req.PromptTS = time.Time{} // unknown: the verdict counts in submissions, not in late syncs
	}
	if promptcheck.Hash(req.Salt, req.Turn.Prompt) != req.CheckHash {
		refuse(422, "the prompt text does not match the prompt hash")
		return
	}
	if !promptcheck.Consistent(req.Turn.Diff, req.GitIns, req.GitDel, req.GitKnown) {
		refuse(422, "the change excerpt adds or removes more lines than git counted")
		return
	}
	// a context turn or earlier prompt that names its prompt hash must match
	// it: the verdict keeps those hashes, and a submission shows whether the
	// check saw the turns that really came before
	for _, t := range append(slices.Clone(req.Context), req.Earlier...) {
		if t.CheckHash != "" && (!hex64.MatchString(t.CheckHash) || promptcheck.Hash(t.Salt, t.Prompt) != t.CheckHash) {
			refuse(422, "an earlier turn's text does not match its prompt hash")
			return
		}
	}
	if v, err := s.store.PromptVerdict(ctx, dev.ID, req.CheckHash); err == nil {
		jsonOut(w, 200, verdictResp(req.CheckHash, v))
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Error("prompt verdict", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	// the account's own setting; a pull asks regardless of it
	if !s.userCheckOn(ctx, user.ID) && (row == nil || !s.pullCovers(ctx, user.ID, row)) {
		jsonErr(w, 404, "the prompt check is off for this account")
		return
	}
	pol := s.pol()
	rules := pol.Rules
	// what a paid call may cost: each refusal is "try later", so the
	// prompt stays queued and its time pending, never decided
	now := s.now()
	day := utcDay(now)
	if !s.lim.Allow(fmt.Sprintf("checku:%d", user.ID), pol.UserPerMin/60, max(1, int(pol.UserPerMin*3))) {
		w.Header().Set("Retry-After", "60")
		jsonErr(w, 429, "too many checks from this account; the device tries again shortly")
		return
	}
	untilTomorrow := now.Truncate(24*time.Hour).Add(24*time.Hour).Sub(now) + time.Minute
	if n, err := s.store.CheckSpend(ctx, day, user.ID); err != nil {
		s.log.Error("check spend", "err", err)
		jsonErr(w, 500, "internal error")
		return
	} else if n >= pol.UserCallsDay {
		w.Header().Set("Retry-After", strconv.Itoa(int(untilTomorrow.Seconds())))
		jsonErr(w, 429, "this account's checks for today are used up; they wait on the device and go through tomorrow")
		return
	}
	if _, _, spent, err := s.store.CheckSpendDay(ctx, day); err != nil {
		s.log.Error("check spend", "err", err)
		jsonErr(w, 500, "internal error")
		return
	} else if spent >= pol.BudgetUSDDay {
		waitLater(w, untilTomorrow, "the server's daily budget for checks is used up; checks wait on the device and go through tomorrow")
		return
	}
	if s.inflight.Add(1) > int64(pol.Concurrency) {
		s.inflight.Add(-1)
		waitLater(w, 30*time.Second, "the server is busy with other checks; the device tries again shortly")
		return
	}
	defer s.inflight.Add(-1)
	// the call can outlast the server's write timeout: give this response
	// the judge's whole budget
	_ = http.NewResponseController(w).SetWriteDeadline(now.Add(pol.Timeout + 30*time.Second))

	// over-long fields are cut and surplus context turns dropped (oldest
	// first), never refused: a server that tightens its caps must not fail
	// an older device's checks for good
	ctxTurns := req.Context
	if n := rules.Caps.ContextTurns; len(ctxTurns) > n {
		ctxTurns = ctxTurns[len(ctxTurns)-n:]
	}
	// A device does not know which of its prompts passed, so it sends the
	// project's earlier prompts as they come: only those this server passed
	// are shown to the judge.
	var earlierTurns []promptcheck.Turn
	if len(req.Earlier) > 0 {
		hs := make([]string, 0, len(req.Earlier))
		for _, t := range req.Earlier {
			if t.CheckHash != "" {
				hs = append(hs, t.CheckHash)
			}
		}
		passes, err := s.store.PromptPasses(ctx, dev.ID, hs)
		if err != nil {
			s.log.Error("earlier prompts", "err", err)
			jsonErr(w, 500, "internal error")
			return
		}
		for _, t := range req.Earlier {
			if passes[t.CheckHash].Pass {
				earlierTurns = append(earlierTurns, t)
			}
		}
	}
	if n := pol.Earlier; len(earlierTurns) > n {
		earlierTurns = earlierTurns[len(earlierTurns)-n:]
	}
	ev := promptcheck.Evidence{}
	turns := make([]promptcheck.Turn, 0, len(ctxTurns)+1)
	for _, t := range ctxTurns {
		t.Activity, _ = trackerLine(t.Activity)
		turns = append(turns, rules.Cut(t, false))
		ev.Context = append(ev.Context, t.CheckHash)
	}
	judged := req.Turn
	var act *promptcheck.Activity
	judged.Activity, act = trackerLine(judged.Activity)
	ev.Activity = act
	turns = append(turns, rules.Cut(judged, true))
	var earlier []string
	for _, t := range earlierTurns {
		p, _ := promptcheck.Cut(t.Prompt, rules.Caps.EarlierCap())
		earlier = append(earlier, p)
		ev.Earlier = append(ev.Earlier, t.CheckHash)
	}
	jctx, cancel := context.WithTimeout(ctx, pol.Timeout)
	defer cancel()
	hash, promptTS := req.CheckHash, req.PromptTS.UTC()
	// the question that needs the front-window time is skipped only when
	// the activity line itself says it was not measured (the request's
	// flag alone does not switch it off; a submission checks the line)
	var skip []string
	if rules.Unbacked != "" && act != nil && !act.Start && act.Front < 0 {
		skip = append(skip, rules.Unbacked)
	}
	// what the device read in the project's folder (none from an older
	// device): cut to the caps, read by the model, dropped with the text
	proj := rules.CutProject(req.Project)
	facts := proj != nil && *proj != promptcheck.Project{} // for the log: whether the device sent any, never what
	ans, err := s.judgeAll(jctx, promptcheck.StateWith(proj, earlier, turns), rules.Ask(len(turns), skip...))
	req, proj, turns, ctxTurns, earlierTurns, earlier, judged = api.PromptCheckReq{}, nil, nil, nil, nil, nil, promptcheck.Turn{} // the text is not needed past the call
	if serr := s.store.AddCheckSpend(ctx, day, user.ID, ans.Cost, err != nil); serr != nil {
		s.log.Error("check spend", "err", serr)
	}
	if err != nil {
		s.log.Warn("prompt check failed", "device", dev.ID, "err", err)
		waitLater(w, 5*time.Minute, "the check could not be made; the prompt stays queued on the device and is sent again later")
		return
	}
	labels := rules.LabelsOf(ans.P)
	for _, k := range skip {
		delete(labels, k) // not asked: whatever came back for it is not an answer
	}
	if !rules.Complete(labels, skip...) {
		s.log.Warn("prompt check: the model left questions unanswered", "device", dev.ID, "answered", len(labels))
		s.judgeFailed(errors.New("the model left questions unanswered"))
		waitLater(w, 5*time.Minute, "the check came back incomplete; the prompt stays queued on the device and is sent again later")
		return
	}
	v := rules.Verdict(labels, ans.Model, skip...)
	if err := s.store.PutPromptVerdict(ctx, dev.ID, hash, v, promptTS, s.now(), ev); err != nil {
		s.log.Error("store prompt verdict", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	s.log.Info("prompt check", "device", dev.ID, "pass", v.Pass, "kind", v.Kind, "minutes", v.Minutes, "rules", v.Rules, "model", v.Model, "project_facts", facts, "cost_usd", ans.Cost)
	jsonOut(w, 200, verdictResp(hash, &v))
}

// trackerLine renders an activity line from the numbers it parses to, so
// the model reads the tracker's format and nothing a person typed;
// a line that does not parse is shown as not readable.
func trackerLine(s string) (string, *promptcheck.Activity) {
	if s == "" {
		return "", nil
	}
	a, ok := promptcheck.ParseActivity(s)
	if !ok {
		return "(not readable)", nil
	}
	return a.String(), &a
}

// verdictResp is the answer to a device: that the prompt was checked. The
// decision and the minutes stay on the server, which counts the prompt's
// time itself when the prompt's records arrive (segments.go).
func verdictResp(hash string, _ *promptcheck.Verdict) api.PromptCheckResp {
	return api.PromptCheckResp{CheckHash: hash}
}

// checkShape refuses a request that is not a prompt check's shape. Size
// is not a reason (the body limit bounds it; the caps cut it). The
// message never quotes the request.
func checkShape(req *api.PromptCheckReq) string {
	switch {
	case !hex64.MatchString(req.CheckHash):
		return "bad check_hash"
	case req.Salt == "" || len(req.Salt) > 128:
		return "bad salt"
	case req.Turn.Prompt == "":
		return "no prompt"
	case len(req.Context) > 100:
		return "too many earlier turns"
	case req.GitIns < 0 || req.GitDel < 0:
		return "bad git counts"
	}
	return ""
}

// recordPeriod adds to the switch's history when the switch changes. The
// first record covers all earlier time with the switch as it is, so
// nothing already counted changes.
func (s *Server) recordPeriod(ctx context.Context, before, after *policy) error {
	ps, err := s.store.RulePeriods(ctx)
	if err != nil {
		return err
	}
	switch {
	case len(ps) == 0:
		err = s.store.AddRulePeriod(ctx, promptcheck.Period{From: time.UnixMilli(0).UTC(), On: after.PromptCheck}, "start")
	case ps[len(ps)-1].On != after.PromptCheck:
		at := s.now().UTC().Truncate(time.Millisecond)
		if !at.After(ps[len(ps)-1].From) {
			at = ps[len(ps)-1].From.Add(time.Millisecond)
		}
		err = s.store.AddRulePeriod(ctx, promptcheck.Period{From: at, On: after.PromptCheck}, "setting")
	}
	if err != nil {
		return err
	}
	if ps, err = s.store.RulePeriods(ctx); err != nil {
		return err
	}
	s.periods.Store(&ps)
	return nil
}

// rulePeriods is the switch's history.
func (s *Server) rulePeriods() []promptcheck.Period {
	if p := s.periods.Load(); p != nil {
		return *p
	}
	return nil
}

// everOn reports whether the check was ever on (else no count needs the
// verdicts).
func (s *Server) everOn() bool {
	for _, p := range s.rulePeriods() {
		if p.On {
			return true
		}
	}
	return s.promptCheckOn()
}

// promptChecks is the switch's history and the stored verdicts for a
// bundle's prompts, for the count (nil when the check was never on).
func (s *Server) promptChecks(ctx context.Context, deviceID string, b *api.Bundle) (*promptcheck.Set, error) {
	if !s.everOn() {
		return nil, nil
	}
	var hashes []string
	for _, en := range b.Entries {
		if e := en.Event; e != nil && e.Kind == event.Prompt && e.CheckHash != "" {
			hashes = append(hashes, e.CheckHash)
		}
	}
	periods := s.rulePeriods()
	if dev, err := s.store.DeviceByID(ctx, deviceID); err == nil {
		// the account's own setting, and the stretches it was off that
		// were checked since (promptchain.go)
		if periods, err = s.effectivePeriods(ctx, deviceID, dev.UserID); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	set := &promptcheck.Set{On: s.promptCheckOn(), Pass: map[string]bool{}, Min: map[string]float64{}, Periods: periods}
	set.On = set.OnAt(s.now())
	if len(hashes) == 0 {
		return set, nil
	}
	closed, err := s.store.ClosedPrompts(ctx, deviceID, MaxClosed)
	if err != nil {
		return nil, err
	}
	for h := range closed {
		set.Pass[h] = false // its text did not match, or never came
	}
	vs, err := s.store.PromptPasses(ctx, deviceID, hashes)
	if err != nil {
		return nil, err
	}
	for h, v := range vs {
		set.Pass[h] = v.Pass
		if v.Pass {
			set.Min[h] = v.Minutes
		}
	}
	return set, nil
}

// lateVerdicts takes, from the buckets older than the sync floor, those a
// passing verdict covers (from its prompt's time, for up to its kind's
// minutes): a verdict that came back late may raise them. The rest go
// back. Only while the prompt check is on.
func (s *Server) lateVerdicts(ctx context.Context, deviceID string, before []ledger.Bucket) (rest, raise []ledger.Bucket, err error) {
	if !s.everOn() || len(before) == 0 {
		return before, nil, nil
	}
	lo, hi := before[0].Start, before[0].Start
	for _, b := range before {
		lo, hi = minTime(lo, b.Start), maxTime(hi, b.Start)
	}
	longest := max(s.pol().Rules.Longest(), 60*time.Minute) // older verdicts keep the minutes they were given
	ps, err := s.store.PassingPrompts(ctx, deviceID, lo.Add(-longest-time.Minute), hi.Add(time.Minute))
	if err != nil {
		return nil, nil, err
	}
	for _, b := range before {
		covered := false
		for _, p := range ps {
			n := promptcheck.Mins(p.Minutes)
			if n <= 0 { // a verdict stored without minutes: as on the device
				n = promptcheck.FallbackMinutes
			}
			if !b.Start.Before(p.TS.Truncate(time.Minute)) && b.Start.Before(p.TS.Add(n)) {
				covered = true
				break
			}
		}
		if covered {
			raise = append(raise, b)
		} else {
			rest = append(rest, b)
		}
	}
	return rest, raise, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// utcDay is the day key of the spend table.
func utcDay(t time.Time) string { return t.UTC().Format("2006-01-02") }
