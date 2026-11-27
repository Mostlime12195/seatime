package stats

import (
	"fmt"
	"strings"
	"time"

	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

// Counting is the answer to "is SeaTime counting right now?".
type Counting struct {
	Yes     bool
	Project string // when Yes
	Key     string // when Yes: the project's key in the local log
	Reason  string // one plain clause: why not, or what is being counted
	// Note is a line shown under it, such as an AI tool SeaTime cannot hear.
	Note string
}

// Line is the status line, e.g. "counting now: yes, on app (hand editing)".
func (c Counting) Line() string {
	if c.Yes {
		s := "counting now: yes, on " + c.Project
		if c.Reason != "" {
			s += " (" + c.Reason + ")"
		}
		return s
	}
	return "counting now: no, because " + c.Reason
}

// CountingInput is what the decision reads.
type CountingInput struct {
	Now    time.Time
	Recent []event.Event // the last ~30 minutes of the log, any order
	// Hooked: at least one harness hooked or the editor proxy on.
	Hooked bool
	// Silent: harness labels the canary found running with silent hooks.
	Silent []string
	// CheckOn: the prompt check is on now (PromptChecks).
	CheckOn bool
}

// NotHeard is the note when an AI tool edits a project through the editor
// plugin ("ai coding") and no prompt was heard there in the last Window.
// With the prompt check on, such time does not count at all (docs/RULE.md).
func NotHeard(project string, checkOn bool) string {
	if checkOn {
		return fmt.Sprintf("An AI tool is editing %s and SeaTime cannot hear your prompts to it, so its time does not count. Only time after a prompt that SeaTime hears can count. Run `seatime doctor` to see which tools it hears.", project)
	}
	return fmt.Sprintf("An AI tool is editing %s and SeaTime cannot hear your prompts to it. Its time counts for %s after each hand edit. Run `seatime doctor` to see which tools it hears.", project, minWords(ledger.Window))
}

func minWords(d time.Duration) string { return fmt.Sprintf("%g minutes", d.Minutes()) }

// CountingNow applies the rule (pkg/ledger) to the recent log: the minute
// now counts if your last heartbeat was under ledger.Timeout ago, or an
// agent is working within ledger.Window of it.
func CountingNow(in CountingInput) Counting {
	var evs []event.Event
	open := map[string]event.Event{} // sessions whose agent has not stopped
	for _, e := range in.Recent {
		if e.TS.After(in.Now) {
			continue
		}
		evs = append(evs, e)
		switch e.Kind {
		case event.Prompt:
			open[e.SessionID] = e
		case event.AgentStop, event.SessionEnd:
			delete(open, e.SessionID)
		}
	}
	for sid, p := range open {
		if sid != "" { // the agent is still working: its run reaches now
			evs = append(evs, event.Event{Kind: event.Tool, SessionID: sid, Project: p.Project, Harness: p.Harness, TS: in.Now})
		}
	}
	var yours, agent *ledger.Heartbeat
	hbs := ledger.Heartbeats(evs)
	for i := range hbs {
		if hbs[i].Yours {
			yours = &hbs[i]
		} else if hbs[i].Kept {
			agent = &hbs[i]
		}
	}
	c := Counting{Note: notHeard(in.Now, evs, in.CheckOn)}
	switch {
	case agent != nil && yours != nil && agent.Project == yours.Project && in.Now.Sub(yours.Time) <= ledger.Window && in.Now.Sub(agent.Time) <= ledger.Timeout:
		c.Yes, c.Project, c.Key = true, name(yours.Project), yours.Project
		c.Reason = "the agent's time counts until " + yours.Time.Add(ledger.Window).In(in.Now.Location()).Format("15:04") + " unless you say something new"
	case yours != nil && in.Now.Sub(yours.Time) <= ledger.Timeout:
		c.Yes, c.Project, c.Key, c.Reason = true, name(yours.Project), yours.Project, "your last heartbeat was under 2 minutes ago"
	case len(in.Silent) > 0:
		c.Reason = strings.Join(in.Silent, " and ") + " is running but SeaTime is not hearing from it (run `seatime doctor`)"
	case !in.Hooked:
		c.Reason = "no AI tool or editor is hooked up yet (run `seatime setup`)"
	case yours == nil:
		c.Reason = "no hand edit or new prompt in the last 30 min"
	default:
		c.Reason = fmt.Sprintf("your last heartbeat was at %s", yours.Time.In(in.Now.Location()).Format("15:04"))
	}
	return c
}

// notHeard is NotHeard for the project of the latest "ai coding" heartbeat
// in the last Window, if no prompt was heard in that project in the Window
// before it.
func notHeard(now time.Time, evs []event.Event, checkOn bool) string {
	var ai *event.Event
	for i := range evs {
		e := &evs[i]
		if e.Kind == event.Edit && strings.EqualFold(e.Category, ledger.AICoding) && now.Sub(e.TS) <= ledger.Window && (ai == nil || e.TS.After(ai.TS)) {
			ai = e
		}
	}
	if ai == nil {
		return ""
	}
	for _, e := range evs {
		if e.Kind == event.Prompt && e.Project == ai.Project && ai.TS.Sub(e.TS) <= ledger.Window {
			return ""
		}
	}
	return NotHeard(name(ai.Project), checkOn)
}

func name(p string) string {
	if p == "" {
		return "no project yet"
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return strings.TrimPrefix(p, "local:")
}
