// Package checks is the device's side of the prompt check
// (pkg/promptcheck). It keeps every prompt's turn in the local store
// (Track), with the check on or off, and sends each finished turn for its
// check with the turns before it as context (Drain): while the account
// has the check on, every one; while it is off, only those the server asks
// for (a pull). It queues and retries with backoff while the server cannot
// be reached, and never drops a prompt.
//
// A turn ends when the AI stops (its Stop hook), when the person sends the
// next prompt, or when the session ends, whichever comes first. Claude
// Code does not run its Stop hook when the person interrupts, so an
// interrupted turn ends at the next prompt or the session's end and is
// judged on what changed until then.
package checks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"seatime/internal/client/scrub"
	"seatime/internal/client/store"
	"seatime/pkg/api"
	"seatime/pkg/event"
	"seatime/pkg/promptcheck"
)

// Interrupted stands in for the AI's reply when the person stopped it
// before it finished.
const Interrupted = "(the person interrupted the AI before it finished replying)"

// NotSeen is the change excerpt of a turn whose start or end the device
// could not read the repository for (no git, too slow, or the daemon
// recorded it).
const NotSeen = "(not seen by the tracker)"

// Keep is how long finished turns keep the AI's reply and the change
// excerpt (the prompt itself is kept for good).
const Keep = 30 * 24 * time.Hour

// Text is a prompt as the device keeps it: cut to promptcheck.MaxPrompt,
// the salt and the prompt hash (promptcheck.Hash), and whether it repeats a
// prompt already sent in the project. For a new prompt Hash is also the
// record's event.Event.CheckHash.
type Text struct {
	Text, Salt, Hash string
	Repeat           bool
}

// NewText salts and hashes a prompt's text (already scrubbed).
func NewText(text string, repeat bool) Text {
	t, _ := promptcheck.Cut(text, promptcheck.MaxPrompt)
	salt := promptcheck.NewSalt()
	return Text{Text: t, Salt: salt, Hash: promptcheck.Hash(salt, t), Repeat: repeat}
}

// SessionKey groups turns: the AI tool's session, or the project when the
// tool gives no session id.
func SessionKey(e event.Event) string {
	if e.SessionID != "" {
		return e.SessionID
	}
	return "project:" + e.Project
}

// Track records the turn bookkeeping for one stored event. p is a
// prompt's text as hashed (empty when the tool gave none); reply is the
// AI's last message at a stop; snap is the repository now, or nil when it
// was not read; info is what the project's folder showed at a prompt
// (Facts), or nil. A prompt's hash joins the device's prompt chain.
func Track(st *store.Store, e event.Event, p Text, reply string, snap *promptcheck.Snapshot, info *promptcheck.Project, now time.Time) error {
	switch e.Kind {
	case event.Prompt, event.AgentStop, event.SessionEnd:
	default:
		return nil
	}
	sess := SessionKey(e)
	last, err := st.LastTurn(sess)
	if err != nil {
		return err
	}
	open := last != nil && last.State == store.TurnOpen
	switch e.Kind {
	case event.Prompt:
		prev := ""
		if last != nil {
			prev = last.Reply
			if open { // the AI did not stop on its own: the person interrupted it
				if err := closeTurn(st, last, "", snap, nil, time.Time{}); err != nil {
					return err
				}
			}
			if open || last.StopTS.IsZero() {
				prev = Interrupted
			}
		}
		if p.Text == "" {
			return nil // no text: nothing to keep or check
		}
		if _, err := st.OpenTurn(store.CheckTurn{Session: sess, Project: e.Project, PromptTS: e.TS, CheckHash: p.Hash,
			Salt: p.Salt, Prompt: p.Text, PrevReply: prev, Start: snap, Info: info, Repeat: p.Repeat}, event.ServerKey(e.Project)); err != nil {
			return err
		}
	case event.AgentStop:
		if open {
			if err := closeTurn(st, last, reply, snap, e.Git, e.TS); err != nil {
				return err
			}
		}
	case event.SessionEnd:
		if open {
			if err := closeTurn(st, last, "", snap, nil, time.Time{}); err != nil {
				return err
			}
		}
	}
	return st.PruneTurns(now, Keep)
}

func closeTurn(st *store.Store, t *store.CheckTurn, reply string, end *promptcheck.Snapshot, g *event.GitFacts, stop time.Time) error {
	diff := NotSeen
	if t.Start != nil && end != nil {
		diff, _ = promptcheck.TurnDiff(*t.Start, *end)
	}
	diff = scrub.Text(diff) // secrets blanked, as in prompts
	reply, _ = promptcheck.Cut(scrub.Text(reply), promptcheck.MaxReply)
	ins, del, known := 0, 0, false
	if g != nil {
		ins, del, known = g.Insertions, g.Deletions, true
	}
	return st.CloseTurn(t.ID, reply, diff, ins, del, known, stop)
}

// Snapshot reads the repository at root for a turn's change excerpt:
// `git diff HEAD` by file, and the untracked files' first lines. nil when
// root is not a git checkout or git is slow or missing.
func Snapshot(ctx context.Context, root string) *promptcheck.Snapshot {
	if root == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 600*time.Millisecond)
	defer cancel()
	out, err := capped(exec.CommandContext(ctx, "git", "-C", root, "diff", "--no-color", "--no-ext-diff", "HEAD"), 256<<10)
	if err != nil {
		return nil
	}
	sn := &promptcheck.Snapshot{Files: promptcheck.ParseDiff(string(out)), Untracked: map[string]string{}}
	for p, sec := range sn.Files {
		if scrub.SecretPath(p) { // a changed .env or key file: say it changed, never what is in it
			sn.Files[p] = fmt.Sprintf("%s: a file that holds secrets changed (contents not sent, fingerprint %s)\n", p, shortHash(sec))
		}
	}
	if out, err := capped(exec.CommandContext(ctx, "git", "-C", root, "ls-files", "--others", "--exclude-standard"), 64<<10); err == nil {
		for i, p := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if p == "" || i >= 50 {
				break
			}
			if scrub.SecretPath(p) {
				sn.Untracked[p] = "a file that holds secrets (contents not read)"
				continue
			}
			sn.Untracked[p] = head(filepath.Join(root, p))
		}
	}
	return sn
}

// shortHash tells two versions of a secret file apart without showing them.
func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

func capped(cmd *exec.Cmd, n int) ([]byte, error) {
	var b bytes.Buffer
	cmd.Stdout = &limitWriter{&b, n}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

type limitWriter struct {
	b *bytes.Buffer
	n int
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if room := w.n - w.b.Len(); room > 0 {
		w.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// head describes an untracked file: its line count and first lines.
func head(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "unreadable"
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "not a regular file"
	} else if fi.Size() > 1<<20 {
		return fmt.Sprintf("%d bytes", fi.Size())
	}
	b := make([]byte, 1<<20)
	n, _ := f.Read(b)
	b = b[:n]
	if bytes.IndexByte(b, 0) >= 0 {
		return fmt.Sprintf("binary, %d bytes", n)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	first := lines[:min(len(lines), 20)]
	s := fmt.Sprintf("%d lines:\n+%s\n", len(lines), strings.Join(first, "\n+"))
	s, _ = promptcheck.Cut(s, 1<<10)
	return s
}

// Activity is what the tracker saw before turn t's prompt, from the local
// log and live (the idle recorder's open window, not yet written): since
// the AI finished the project's turn before (prev, in any chat), or since
// its prompt when it was interrupted. Time no idle record watched is "not
// measured". Only a project with no earlier turn is "start of session".
// The numbers come from promptcheck.MeasureActivity, which the server
// reruns on a submission's records.
func Activity(st *store.Store, t store.CheckTurn, prev *store.CheckTurn, live []event.Event) (promptcheck.Activity, error) {
	if prev == nil {
		return promptcheck.Activity{Start: true}, nil
	}
	from, interrupted := prev.StopTS, false
	if from.IsZero() {
		from, interrupted = prev.PromptTS, true
	}
	to := t.PromptTS
	evs, err := st.Events(store.Query{Since: from, Until: to, Project: t.Project, Kinds: []event.Kind{event.Run, event.Edit}})
	if err != nil {
		return promptcheck.Activity{}, err
	}
	idles, err := st.Events(store.Query{Since: from, Until: to.Add(2 * time.Hour), Kinds: []event.Kind{event.Idle}})
	if err != nil {
		return promptcheck.Activity{}, err
	}
	all := append(append(evs, idles...), live...)
	return promptcheck.MeasureActivity(all, t.Project, from, to, interrupted), nil
}

// Earlier is how many earlier prompts that passed a request carries when
// the server has not said (its setting prompt_check_earlier).
const Earlier = 20

// Request builds the check request for a queued turn, with live the idle
// recorder's open window (see Activity). Its context is the project's
// turns before it, in any chat (a new chat's first prompt shows NewChat
// where the AI's reply would be), and in front of them up to the server's
// number of earlier prompts of the project that passed, text only. Each
// carries its prompt hash and salt, so the server can tie the context to
// the log. With it go the project facts read when the prompt was sent.
func Request(st *store.Store, deviceID string, t store.CheckTurn, live []event.Event) (api.PromptCheckReq, error) {
	before, err := st.TurnsBeforeProject(t.Project, t.PromptTS, t.ID, promptcheck.MaxContextTurns+1)
	if err != nil {
		return api.PromptCheckReq{}, err
	}
	var prev *store.CheckTurn
	if n := len(before); n > 0 {
		prev = &before[n-1]
	}
	act, err := Activity(st, t, prev, live)
	if err != nil {
		return api.PromptCheckReq{}, err
	}
	req := api.PromptCheckReq{DeviceID: deviceID, CheckHash: t.CheckHash, Salt: t.Salt,
		Turn:    promptcheck.Turn{PrevReply: replyBefore(t, prev), Activity: act.String(), Prompt: t.Prompt, Diff: t.Diff}.Capped(true),
		Project: t.Info,
		GitIns:  t.GitIns, GitDel: t.GitDel, GitKnown: t.GitKnown, FrontMeasured: act.Front >= 0, PromptTS: t.PromptTS}
	ctxTurns := before
	if len(ctxTurns) > promptcheck.MaxContextTurns {
		ctxTurns = ctxTurns[len(ctxTurns)-promptcheck.MaxContextTurns:]
	}
	off := len(before) - len(ctxTurns)
	ids := []int64{t.ID}
	for i, c := range ctxTurns {
		var p *store.CheckTurn
		if j := off + i - 1; j >= 0 {
			p = &before[j]
		}
		a, err := Activity(st, c, p, live)
		if err != nil {
			return api.PromptCheckReq{}, err
		}
		// the prompt in full, as hashed: the server checks it, then cuts it
		ct := promptcheck.Turn{PrevReply: replyBefore(c, p), Activity: a.String(), Diff: c.Diff}.Capped(false)
		ct.Prompt, ct.CheckHash, ct.Salt = c.Prompt, c.CheckHash, c.Salt
		req.Context = append(req.Context, ct)
		ids = append(ids, c.ID)
	}
	if n := st.MetaInt(store.MetaEarlier, Earlier); n > 0 {
		// the device does not know which of them counted: the server keeps
		// those it passed, so it gets more than it will show the judge
		earlier, err := st.CheckedBefore(t.Project, t.PromptTS, ids, min(3*n, 60))
		if err != nil {
			return api.PromptCheckReq{}, err
		}
		for _, e := range earlier {
			req.Earlier = append(req.Earlier, promptcheck.Turn{Prompt: e.Prompt, CheckHash: e.CheckHash, Salt: e.Salt})
		}
	}
	return req, nil
}

// replyBefore is the AI's reply before turn t: its own chat's, or NewChat
// when t opened a new chat after other turns of the project.
func replyBefore(t store.CheckTurn, prev *store.CheckTurn) string {
	if prev != nil && t.PrevReply == "" && prev.Session != t.Session {
		return promptcheck.NewChat
	}
	return t.PrevReply
}

// Sender sends one check request; status is the HTTP status (0 when the
// server was not reached).
type Sender func(ctx context.Context, req api.PromptCheckReq) (resp api.PromptCheckResp, status int, err error)

// Backoff is the wait before the n-th retry: 30 s doubling to an hour,
// with jitter.
func Backoff(tries int) time.Duration {
	d := 30 * time.Second << min(tries, 7)
	d = min(d, time.Hour)
	return d - time.Duration(rand.Int63n(int64(d/5)+1))
}

// ErrOff: the server says the prompt check is off.
var ErrOff = errors.New("the server's prompt check is off")

// Wanted reports, from what the last sync said, which queued turns to send
// now: all of them while the account has the check on, else those a pull
// asks for. nil: none.
func Wanted(st *store.Store) func(store.CheckTurn) bool {
	if st.PromptCheckOn() {
		return func(store.CheckTurn) bool { return true }
	}
	pulls := st.Pulls()
	if len(pulls) == 0 {
		return nil
	}
	return func(t store.CheckTurn) bool {
		k := event.ServerKey(t.Project)
		for _, p := range pulls {
			if (p.Project == "" || p.Project == k) && !t.PromptTS.Before(p.From) && t.PromptTS.Before(p.To) {
				return true
			}
		}
		return false
	}
}

// retryAfter is the wait a server answer asked for (0: none).
func retryAfter(err error) time.Duration {
	var ra interface{ RetryAfter() time.Duration }
	if errors.As(err, &ra) {
		return ra.RetryAfter()
	}
	return 0
}

// Drain sends the turns waiting for their check that Wanted takes, at most
// limit, and keeps the verdicts. Nothing waiting is ever dropped or
// decided because the server cannot answer: while nothing is wanted, on a
// 404, a 409 (its hash has not reached the server), a 5xx or no answer at
// all, the turn only waits, with Backoff. A 429 or 503 holds every check
// for as long as the server asked (store.PauseChecks), without counting it
// against the turn. It stops at the first failure; that turn waits and the
// rest wait for the next round. live is the idle recorder's open window,
// when the caller has one.
func Drain(ctx context.Context, st *store.Store, deviceID string, send Sender, now time.Time, limit int, live ...event.Event) (int, error) {
	want := Wanted(st)
	if want == nil || st.ChecksPaused(now) {
		return 0, nil // paused: the turns wait
	}
	due, err := st.DueChecks(now, limit, want)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, t := range due {
		req, err := Request(st, deviceID, t, live)
		if err != nil {
			return done, err
		}
		resp, status, err := send(ctx, req)
		switch {
		case err == nil && resp.CheckHash == t.CheckHash:
			if err := st.CheckDone(t.ID, t.CheckHash, promptcheck.Verdict{}); err != nil {
				return done, err
			}
			done++
		case status == 404:
			// off for the account, or a proxy's hiccup: only a successful
			// sync may say it is off; the turn waits
			_ = st.RetryCheck(t.ID, now.Add(Backoff(t.Tries)))
			return done, ErrOff
		case status == 409:
			// its hash has not reached the server yet: the next push sends it
			_ = st.RetryCheck(t.ID, now.Add(30*time.Second))
			return done, err
		case status == 429 || status == 503:
			wait := retryAfter(err)
			if wait <= 0 {
				wait = Backoff(0)
			}
			_ = st.PauseChecks(now.Add(min(wait, 25*time.Hour)))
			return done, err
		case status == 400 || status == 413 || status == 422:
			// the server will never take this one (it does not match the
			// log, or is the wrong shape): keep a failing verdict so it is
			// not sent again; the server closed it too, so both count it
			// the same
			v := promptcheck.Verdict{Pass: false, Model: fmt.Sprintf("refused by the server (%d)", status)}
			if err := st.CheckDone(t.ID, t.CheckHash, v); err != nil {
				return done, err
			}
		default:
			if err == nil {
				err = errors.New("the server answered for another prompt")
			}
			_ = st.RetryCheck(t.ID, now.Add(Backoff(t.Tries)))
			return done, err
		}
	}
	return done, nil
}
