package emit

import (
	"maps"
	"time"

	"seatime/internal/adapters/claudecode"
	"seatime/internal/client/logx"
	"seatime/internal/client/store"
	"seatime/pkg/event"
)

// A prompt is a record only when a person sent it. Some AI tools run
// their prompt hook for messages they make themselves (Claude Code: a
// background task or subagent reporting back, a message from another
// session or a channel, a scheduled task) and tell the hook nothing about
// the sender. Their own log does, but only once the hook has returned.
//
// So such a prompt is kept back (store.Held), with what the hook read at
// that moment, and recorded when the log says who sent it: at the
// session's next record, or by the daemon a moment later. Sent by a
// person, it is recorded as it came, with the time it was sent. Sent by
// the tool, no prompt is recorded, only the run it started. A log that
// holds the prompt and does not say (an older tool) leaves the prompt as
// it came; so does a log that never holds it within HeldWait.

// HeldWait is how long the daemon waits for a tool's log to hold a prompt
// before it records the prompt as it came.
const HeldWait = 2 * time.Minute

// heldSettle is how long the daemon leaves a prompt whose log lines name
// no sender, in case the line that does is still being written.
const heldSettle = 3 * time.Second

type holdMark struct{ log, id string }

// takeHold takes the adapter's hold mark off a record.
func takeHold(e *event.Event) *holdMark {
	m, ok := e.Extra[event.HoldKey].(map[string]any)
	if _, has := e.Extra[event.HoldKey]; has {
		e.Extra = maps.Clone(e.Extra)
		delete(e.Extra, event.HoldKey)
		if len(e.Extra) == 0 {
			e.Extra = nil
		}
	}
	if !ok {
		return nil
	}
	log, _ := m["log"].(string)
	id, _ := m["id"].(string)
	if log == "" || id == "" {
		return nil
	}
	return &holdMark{log: log, id: id}
}

// hold keeps a prompt back, with what only this moment can tell: who was
// at the computer, and the project's folder before the AI changes it.
func (p *Pipeline) hold(raw event.Event, m *holdMark, root, key string, now time.Time) error {
	if p.Presence != nil && raw.Presence == "" {
		f := p.Presence()
		raw.Presence, raw.InputBeforeMS, raw.TerminalIdleS, raw.FrontWindow = f.Presence, f.InputBeforeMS, f.TerminalIdleS, f.FrontWindow
	}
	h := store.Held{Session: raw.SessionID, PromptID: m.id, Log: m.log, At: now, Prompt: raw}
	if root != "" {
		if p.Snap != nil {
			h.Snap = p.Snap(root)
		}
		if p.Facts != nil && raw.Text != "" {
			h.Info = p.Facts(root, projectName(p.Store.Label(key)))
		}
	}
	return p.Store.Hold(h)
}

// heldPoll is how long a record of a session waits for the tool's log to
// name the sender of a prompt that was held a moment ago.
var heldPoll = 600 * time.Millisecond

// releaseSession decides the prompts a session holds back, oldest first,
// before its next record is stored. The tool has normally written the
// prompt to its log by then. When it has not (a subagent's tool call can
// come in the moment between), the prompt stays held and the record goes
// in ahead of it: a log that does not hold a prompt yet says nothing
// about who sent it.
func (p *Pipeline) releaseSession(session string, now time.Time) {
	rows, err := p.Store.HeldRows(session)
	if err != nil {
		return
	}
	for _, h := range rows {
		if !p.settle(h, now) {
			return // it stays held, and so do the session's later ones
		}
	}
}

// settle decides one held prompt at a later record of its session; false
// when it stays held.
func (p *Pipeline) settle(h store.Held, now time.Time) bool {
	young := now.Sub(h.At) < heldSettle
	for deadline := time.Now().Add(heldPoll); ; time.Sleep(50 * time.Millisecond) {
		kind, seen := p.sender(h)
		switch {
		case seen && kind != "", seen && !young, !seen && now.Sub(h.At) >= HeldWait:
			return p.finish(h, kind, now, true)
		case young && time.Now().Before(deadline):
			// held a moment ago: the line that names the sender may be on its way
		case seen:
			return p.finish(h, "", now, true) // its lines are there, and none names a sender
		default:
			return false
		}
	}
}

func (p *Pipeline) sender(h store.Held) (kind string, seen bool) {
	if p.Sender == nil {
		return "", true // nothing to ask: as it came
	}
	return p.Sender(h.Log, h.PromptID)
}

// ReleaseHeld is the daemon's turn: it records every held prompt whose
// sender the tool's log now gives, and those the log never held within
// HeldWait. It returns how many prompts it decided.
func (p *Pipeline) ReleaseHeld(now time.Time) int {
	rows, err := p.Store.HeldRows("")
	if err != nil {
		return 0
	}
	n := 0
	waiting := map[string]bool{} // sessions whose oldest held prompt still waits
	for _, h := range rows {
		if waiting[h.Session] {
			continue
		}
		kind, seen := p.sender(h)
		age := now.Sub(h.At)
		if !seen && age < HeldWait || seen && kind == "" && age < heldSettle || !p.finish(h, kind, now, false) {
			waiting[h.Session] = true
			continue
		}
		n++
	}
	return n
}

// finish records a held prompt, or drops it, by the sender the tool's log
// gave ("": it gave none, so the prompt is recorded as it came). It
// reports whether the prompt is decided. wait: when another process is
// at it, give that one a moment to finish.
func (p *Pipeline) finish(h store.Held, kind string, now time.Time, wait bool) bool {
	if ok, err := p.Store.ClaimHeld(h.ID, now); err != nil || !ok {
		for i := 0; wait && err == nil && i < 20; i++ {
			time.Sleep(50 * time.Millisecond)
			if left, lerr := p.Store.HeldRows(h.Session); lerr != nil || len(left) == 0 || left[0].ID != h.ID {
				return true // the other process finished it
			}
		}
		return false
	}
	again := !h.Claimed.IsZero() // a release that did not finish: skip what it stored
	put := func(e event.Event, got *store.Held) bool {
		if again {
			if dup, err := p.Store.HasEvent(e); err == nil && dup {
				return true
			}
		}
		if _, err := p.emit(e, nil, got); err != nil {
			logx.Printf("held prompt: %v", err)
			return false
		}
		return true
	}
	if kind == "" || kind == claudecode.Human {
		if !put(h.Prompt, &h) {
			return false // stays claimed; taken up again after store.HeldClaim
		}
	} else {
		logx.Printf("%s: a message the tool sent itself (%s) is not recorded as a prompt", h.Prompt.Harness, kind)
	}
	for _, e := range h.After {
		if !put(e, nil) {
			return false
		}
	}
	if err := p.Store.DropHeld(h.ID); err != nil {
		logx.Printf("held prompt: %v", err)
	}
	return true
}
