package promptcheck

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

// NewChat stands in for the AI's previous reply on the first prompt of a
// new chat (a new session id: /clear, a new chat, a fork) in a project that
// already has turns: the check reads the project's turns before it, never
// "start of session" (the undo red team: a new session per resend farmed
// every reworded repeat, and switched off the fake-bug question).
const NewChat = "(the person started a new chat in the same project; its earlier turns are above)"

// ---------------- the activity line, both ways ----------------

var (
	durRe = `(not measured|\d+m\d{2}s|\d+s)`
	actRe = regexp.MustCompile(`^(time since the AI finished|the person interrupted the AI; time since the previous prompt): ` + durRe +
		`; keyboard/mouse input in that time: ` + durRe + `; of which with the AI tool's window in front: ` + durRe +
		`; commands the person ran: (none|(\d+) \(([a-z_, ]+)\)); hand edits in the editor: (none|\d+)$`)
	kindRe = regexp.MustCompile(`^[a-z_]{1,20}$`)
)

// ParseActivity reads an activity line in exactly the format String
// writes. The server renders the line the model reads from what it parsed,
// so the field carries numbers, never text a person wrote; ok is false for
// anything else.
func ParseActivity(s string) (Activity, bool) {
	if s == "start of session" {
		return Activity{Start: true}, true
	}
	m := actRe.FindStringSubmatch(s)
	if m == nil {
		return Activity{}, false
	}
	var a Activity
	a.Interrupted = strings.HasPrefix(m[1], "the person interrupted")
	var ok1, ok2, ok3 bool
	a.Gap, ok1 = parseDur(m[2])
	a.Input, ok2 = parseDur(m[3])
	a.Front, ok3 = parseDur(m[4])
	if !ok1 || !ok2 || !ok3 {
		return Activity{}, false
	}
	if m[5] != "none" {
		n, _ := strconv.Atoi(m[6])
		kinds := strings.Split(m[7], ", ")
		if len(kinds) != n || n > 1000 {
			return Activity{}, false
		}
		for _, k := range kinds {
			if !kindRe.MatchString(k) {
				return Activity{}, false
			}
		}
		a.Commands = kinds
	}
	if m[8] != "none" {
		a.HandEdits, _ = strconv.Atoi(m[8])
	}
	return a, a.String() == s
}

func parseDur(s string) (time.Duration, bool) {
	if s == "not measured" {
		return -1, true
	}
	var mm, ss int
	if strings.Contains(s, "m") {
		if _, err := fmt.Sscanf(s, "%dm%ds", &mm, &ss); err != nil {
			return 0, false
		}
	} else if _, err := fmt.Sscanf(s, "%ds", &ss); err != nil {
		return 0, false
	}
	return time.Duration(mm*60+ss) * time.Second, true
}

// MeasureActivity is what the tracker's records show between from (the AI
// finishing the turn before, or that turn's prompt when it was
// interrupted) and to (the prompt): the person's own runs and hand edits
// in project, and the input the idle records saw. The device builds the
// line from it; the server rebuilds it from a segment's records and
// compares.
func MeasureActivity(evs []event.Event, project string, from, to time.Time, interrupted bool) Activity {
	a := Activity{Interrupted: interrupted, Gap: to.Sub(from), Front: -1, Input: -1}
	var idles []event.Event
	for _, e := range evs {
		if e.Kind == event.Idle {
			idles = append(idles, e)
			continue
		}
		if e.Project != project || e.SessionID != "" || e.TS.Before(from) || !e.TS.Before(to) {
			continue
		}
		switch {
		case e.Kind == event.Run:
			k := e.RunKind
			if k == "" {
				k = event.RunExec
			}
			a.Commands = append(a.Commands, k)
		case e.Kind == event.Edit && !strings.EqualFold(e.Category, ledger.AICoding):
			a.HandEdits++
		}
	}
	in, front, covered, frontCovered := InputIn(idles, from, to)
	if covered {
		a.Input = in
		if frontCovered {
			a.Front = front
		}
	}
	return a
}

// InputIn adds up the 5 s slices with input in [from, to), and of those
// the ones with the AI tool's window in front, and reports whether any
// idle record watched that time for input, and for the front window.
func InputIn(idles []event.Event, from, to time.Time) (in, front time.Duration, covered, frontCovered bool) {
	fronts := map[int64]int64{}
	for _, e := range idles {
		for _, f := range e.FrontSlices {
			fronts[f[0]] |= f[1]
		}
	}
	seen := map[int64]bool{}
	for _, e := range idles {
		for _, sl := range e.InputSlices {
			m := sl[0]
			if seen[m] {
				continue
			}
			fm, fok := fronts[m]
			for i := int64(0); i < 12; i++ {
				at := time.UnixMilli(m + i*int64(5*time.Second/time.Millisecond))
				if at.Before(from) || !at.Before(to) {
					continue
				}
				covered = true
				frontCovered = frontCovered || fok
				if sl[1]&(1<<i) != 0 {
					in += 5 * time.Second
					if fm&(1<<i) != 0 {
						front += 5 * time.Second
					}
				}
			}
			seen[m] = true
		}
	}
	return in, front, covered, frontCovered
}

// ---------------- the state the model reads ----------------

// headerRe matches a line that looks like one of the state's own headers.
var headerRe = regexp.MustCompile(`(?i)^\s*(={2,}|-{3,}\s*(turn|earlier)|turn\s+\d+\b|ai'?s previous reply\s*:|activity before prompt|prompt\s+\d+\s*:|code change after prompt|earlier prompts|end of earlier prompts|project\s+(name|files|readme)\b|end of project)`)

// Neutral marks every line of text a person or the AI wrote that looks
// like one of the state's headers, so a typed prompt cannot forge a turn or
// a tracker-labelled activity line. Other text is unchanged.
func Neutral(s string) string {
	if !headerRe.MatchString(s) && !strings.Contains(s, "\n") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if headerRe.MatchString(l) {
			lines[i] = "(quoted) " + l
		}
	}
	return strings.Join(lines, "\n")
}

// StateWith is State with, in front of the turns, the project's section
// (Project: name, file names, README; an older device sends none) and the
// project's earlier prompts that already counted, in their own fenced
// section (text only, oldest first): a reworded repeat of one of them
// reads as asking again, however many throwaway prompts or new chats came
// between.
func StateWith(p *Project, earlier []string, turns []Turn) string {
	var b strings.Builder
	b.WriteString(p.block())
	if len(earlier) > 0 {
		b.WriteString("=== EARLIER PROMPTS IN THIS PROJECT THAT ALREADY COUNTED (oldest first; text only) ===\n")
		for i, p := range earlier {
			fmt.Fprintf(&b, "EARLIER %d: %s\n", i+1, strings.ReplaceAll(Neutral(p), "\n", " "))
		}
		b.WriteString("=== END OF EARLIER PROMPTS ===\n\n")
	}
	return b.String() + State(turns)
}

// ---------------- what a verdict was judged on ----------------

// Evidence is what a verdict was judged on, without any text: the
// activity line as numbers, and the prompt hashes of the turns shown as
// context and of the earlier prompts. The server keeps it with the verdict
// and, when the records arrive, checks it against the records: a client that
// claims "start of session", hides the turns before, or invents input it
// did not have shows up there.
type Evidence struct {
	Activity *Activity `json:"activity,omitempty"` // nil: the line was not readable
	Context  []string  `json:"context,omitempty"`  // prompt hashes of the context turns, oldest first ("" for a repeat)
	Earlier  []string  `json:"earlier,omitempty"`  // prompt hashes of the earlier prompts shown
}
