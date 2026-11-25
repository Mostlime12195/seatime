package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/promptcheck"
)

// The prompt chain, the account's own prompt check setting, and the
// server asking devices for saved prompts (pkg/promptcheck chain.go,
// docs/RULE.md "When the AI check is on").
//
// Every prompt a device records is kept on the device, and its hash comes
// here at once, chained per device. While an account has the check off,
// its time counts as if the check were off, and its prompts wait on the
// device. When it is turned back on they are sent for their check too, and
// a stretch it was off switches to the check once all its prompts are
// checked. The server can ask a device for its saved prompts at any time
// (a pull, e.g. for one project when it is shipped): the device sends them
// for their check on its next sync, whatever the setting.

// handlePromptHashes takes a device's new prompt chain entries.
func (s *Server) handlePromptHashes(w http.ResponseWriter, r *http.Request) {
	dev, _, ok := s.authDevice(w, r)
	if !ok {
		return
	}
	var req api.PromptHashesReq
	if err := decodeBody(w, r, &req, 1<<20, 2<<20); err != nil {
		jsonErr(w, 400, "bad request")
		return
	}
	if req.DeviceID != dev.ID {
		jsonErr(w, 403, "device_id does not match token")
		return
	}
	if len(req.Entries) > api.MaxPromptHashes {
		jsonErr(w, 413, fmt.Sprintf("at most %d entries a request", api.MaxPromptHashes))
		return
	}
	now := s.now()
	for i := range req.Entries {
		e := &req.Entries[i]
		switch {
		case !hex64.MatchString(e.Commit) || !hex64.MatchString(e.Link):
			jsonErr(w, 400, fmt.Sprintf("entry %d: bad hash", e.Seq))
			return
		case e.Project == "" || len(e.Project) > 300:
			jsonErr(w, 400, fmt.Sprintf("entry %d: bad project", e.Seq))
			return
		case e.TS.IsZero() || e.TS.After(now.Add(clockSkew)):
			jsonErr(w, 400, fmt.Sprintf("entry %d: bad time", e.Seq))
			return
		}
		e.TS = e.TS.UTC().Truncate(time.Millisecond)
	}
	ctx := r.Context()
	taken, err := s.store.AddPromptChain(ctx, dev.ID, req.Entries, now)
	if err != nil && !errors.Is(err, store.ErrConflict) {
		s.log.Error("prompt chain", "err", err)
		jsonErr(w, 500, "internal error")
		return
	}
	seq, link, herr := s.store.PromptChainHead(ctx, dev.ID)
	if herr != nil {
		s.log.Error("prompt chain head", "err", herr)
		jsonErr(w, 500, "internal error")
		return
	}
	resp := api.PromptHashesResp{Seq: seq, Link: link, Taken: taken}
	if err != nil {
		s.log.Warn("prompt chain break", "device", dev.ID, "head", seq)
		jsonOut(w, 409, resp)
		return
	}
	jsonOut(w, 200, resp)
}

// userCheckPeriods is an account's own setting's history (on when empty).
func (s *Server) userCheckPeriods(ctx context.Context, userID int64) []promptcheck.Period {
	ps, err := s.store.UserCheckPeriods(ctx, userID)
	if err != nil {
		s.log.Error("user check periods", "err", err)
		return nil
	}
	return ps
}

// userCheckOn is the account's own setting now.
func (s *Server) userCheckOn(ctx context.Context, userID int64) bool {
	return promptcheck.UserOnAt(s.userCheckPeriods(ctx, userID), s.now())
}

// effectivePeriods is the history one device's records are counted with:
// the server's switch, the account's setting, and the stretches it was off
// whose prompts have all been checked since (promptcheck.Effective).
func (s *Server) effectivePeriods(ctx context.Context, deviceID string, userID int64) ([]promptcheck.Period, error) {
	from, err := s.store.ChainFrom(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	var rerr error
	ps := promptcheck.Effective(s.rulePeriods(), s.promptCheckOn(), s.userCheckPeriods(ctx, userID), from,
		func(a, b time.Time) (int, int) {
			n, settled, err := s.store.ChainResolve(ctx, deviceID, a, b)
			if err != nil {
				rerr = err
			}
			return n, settled
		})
	return ps, rerr
}

// combinedPeriods is the server's switch and the account's setting
// together, without the stretches that switched: what an older device,
// which keeps no chain, counts with.
func (s *Server) combinedPeriods(ctx context.Context, userID int64) []promptcheck.Period {
	return promptcheck.Effective(s.rulePeriods(), s.promptCheckOn(), s.userCheckPeriods(ctx, userID), time.Time{}, nil)
}

// pullCovers reports whether an open pull of the account asks for the
// chained prompt row.
func (s *Server) pullCovers(ctx context.Context, userID int64, row *store.ChainRow) bool {
	ps, err := s.store.PromptPulls(ctx, userID, true)
	if err != nil {
		s.log.Error("prompt pulls", "err", err)
		return false
	}
	for _, p := range ps {
		if (api.PromptPull{Project: p.Project, From: p.From, To: p.To}).Covers(row.Project, row.TS) {
			return true
		}
	}
	return false
}

// MaxClosed is how many closed prompts a sync answer lists.
const MaxClosed = 2000

// chainResp is the sync answer's part for the prompt chain. It also settles
// the account's pulls: one whose prompts are all settled is closed, one
// past its deadline has what never came marked missing and is closed.
func (s *Server) chainResp(ctx context.Context, dev *store.Device, userID int64) (api.PromptChainResp, error) {
	r := api.PromptChainResp{Chain: 1, ServerCheck: s.promptCheckOn(), ServerPeriods: s.rulePeriods(), UserPeriods: s.userCheckPeriods(ctx, userID)}
	if err := s.settlePulls(ctx, userID); err != nil {
		return r, err
	}
	ps, err := s.store.PromptPulls(ctx, userID, true)
	if err != nil {
		return r, err
	}
	for _, p := range ps {
		r.Pulls = append(r.Pulls, api.PromptPull{Project: p.Project, From: p.From, To: p.To})
	}
	closed, err := s.store.ClosedPrompts(ctx, dev.ID, MaxClosed)
	if err != nil {
		return r, err
	}
	for h, st := range closed {
		r.Closed = append(r.Closed, api.ClosedPrompt{Commit: h, Status: st})
	}
	return r, nil
}

// settlePulls closes an account's pulls that are done or past their deadline.
func (s *Server) settlePulls(ctx context.Context, userID int64) error {
	ps, err := s.store.PromptPulls(ctx, userID, true)
	if err != nil {
		return err
	}
	now := s.now()
	for _, p := range ps {
		if now.After(p.Deadline) {
			n, err := s.store.MarkPullMissing(ctx, p)
			if err != nil {
				return err
			}
			if n > 0 {
				s.log.Warn("prompt pull: prompts never came", "pull", p.ID, "user", userID, "missing", n)
			}
			if err := s.store.ClosePromptPull(ctx, p.ID); err != nil {
				return err
			}
			continue
		}
		if n, settled, err := s.store.PullResolve(ctx, p); err != nil {
			return err
		} else if n > 0 && settled >= n && now.Sub(p.Created) > time.Hour {
			// done; kept open an hour so a device that has not pushed its
			// newest hashes yet still sees it
			if err := s.store.ClosePromptPull(ctx, p.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// PullWait is how long a pull waits for a device before what has not come
// is marked missing.
const PullWait = 14 * 24 * time.Hour

// ShipPull asks an account's devices for the saved prompts a shipped
// project's count needs: every prompt of the project, and every prompt of
// any project sent in a stretch the account had the check off that the
// project's prompts fall in (a stretch switches to the check only when all
// its prompts are checked). It returns the pulls it stored.
func ShipPull(ctx context.Context, st store.Store, userID int64, project, by string, now time.Time, wait time.Duration) ([]store.PromptPull, error) {
	if wait <= 0 {
		wait = PullWait
	}
	mk := func(proj string, from, to time.Time) store.PromptPull {
		return store.PromptPull{UserID: userID, Project: proj, From: from, To: to, Created: now, Deadline: now.Add(wait), By: by}
	}
	pulls := []store.PromptPull{mk(project, time.UnixMilli(0).UTC(), now.Add(time.Minute))}
	user, err := st.UserCheckPeriods(ctx, userID)
	if err != nil {
		return nil, err
	}
	// the stretches the account had it off, as [from, to)
	type span struct{ from, to time.Time }
	var off []span
	for i, p := range user {
		if p.On {
			continue
		}
		to := now.Add(time.Minute)
		if i+1 < len(user) {
			to = user[i+1].From
		}
		off = append(off, span{p.From, to})
	}
	for _, o := range off {
		n, _, err := st.PullResolve(ctx, mk(project, o.from, o.to))
		if err != nil {
			return nil, err
		}
		if n > 0 {
			pulls = append(pulls, mk("", o.from, o.to))
		}
	}
	for i := range pulls {
		id, err := st.AddPromptPull(ctx, pulls[i])
		if err != nil {
			return nil, err
		}
		pulls[i].ID = id
	}
	return pulls, nil
}

// handleSettingsPromptCheck saves the account's own prompt check setting.
func (s *Server) handleSettingsPromptCheck(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.postGuard(w, r)
	if !ok {
		return
	}
	on := r.PostFormValue("prompt_check") == "1"
	if err := s.store.SetUserCheck(r.Context(), u.ID, on, s.now()); err != nil {
		s.log.Error("set user check", "err", err)
		http.Error(w, "internal error", 500)
		return
	}
	msg := "The prompt check is on."
	if !on {
		msg = "The prompt check is off. Your prompts stay on your computer."
	}
	http.Redirect(w, r, "/settings?ok="+url.QueryEscape(msg)+"#prompt-check", http.StatusSeeOther)
}

// chainEntry is the device's chain entry for a prompt hash (nil when it
// has none), and whether this prompt should have one: the device keeps a
// chain and the prompt is not older than its start (a prompt recorded
// before the device's update, still waiting for its check, has none).
func (s *Server) chainEntry(ctx context.Context, deviceID, commit string, promptTS time.Time) (*store.ChainRow, bool, error) {
	if !hex64.MatchString(commit) {
		return nil, false, nil
	}
	row, err := s.store.PromptChainEntry(ctx, deviceID, commit)
	switch {
	case err == nil:
		return row, true, nil
	case !errors.Is(err, store.ErrNotFound):
		return nil, false, err
	}
	from, err := s.store.ChainFrom(ctx, deviceID)
	if err != nil || from.IsZero() {
		return nil, false, err
	}
	return nil, promptTS.IsZero() || !promptTS.Before(from), nil
}

// handleAdminPromptPull asks an account's devices for what shipping one
// project needs (ShipPull), from /admin: the same as `seatimed prompts
// pull -user NAME -project KEY`.
func (s *Server) handleAdminPromptPull(w http.ResponseWriter, r *http.Request) {
	_, admin, ok := s.adminOnly(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if ss, _ := s.session(w, r); !s.csrfOK(r, ss) {
		http.Error(w, "bad csrf token (reload the page and try again)", 403)
		return
	}
	ctx := r.Context()
	name, project := strings.TrimSpace(r.PostFormValue("username")), strings.TrimSpace(r.PostFormValue("project"))
	u, err := s.store.UserByName(ctx, strings.TrimPrefix(name, "@"))
	if err != nil || project == "" || len(project) > 300 {
		http.Redirect(w, r, "/admin?ok="+url.QueryEscape("Give an existing username and a project key.")+"#pulls", http.StatusSeeOther)
		return
	}
	pulls, err := ShipPull(ctx, s.store, u.ID, project, admin.Username, s.now(), PullWait)
	if err != nil {
		s.log.Error("prompt pull", "err", err)
		http.Error(w, "internal error", 500)
		return
	}
	s.log.Info("prompt pull", "user", u.ID, "pulls", len(pulls), "by", admin.Username)
	msg := fmt.Sprintf("Asked @%s's devices for %d range(s) of saved prompts. They send them on their next sync.", u.Username, len(pulls))
	http.Redirect(w, r, "/admin?ok="+url.QueryEscape(msg)+"#pulls", http.StatusSeeOther)
}
