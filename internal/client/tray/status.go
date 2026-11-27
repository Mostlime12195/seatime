// Package tray is the menu bar icon (`seatime tray`): on macOS a menu bar
// item, on Windows a notification area icon, on Linux a status notifier
// item. It is a second, small process. The daemon holds the data and does
// the work; the icon asks it over the local endpoint (/seatime/tray) what
// to show and passes on what the person clicks. Counting never depends on
// it.
package tray

import (
	"fmt"
	"strings"
)

// Status is what the daemon tells the icon (GET /seatime/tray).
type Status struct {
	// Version is the daemon's version. When it changes, the icon starts
	// again on the new binary.
	Version string `json:"version"`
	// Off: the icon is turned off (`seatime tray off`); it exits.
	Off bool `json:"off,omitempty"`
	// Account is the signed-in username, "" while signed out.
	Account string `json:"account,omitempty"`
	// SignIn is the page on which this computer's sign-in waits for
	// approval, when one does.
	SignIn string `json:"sign_in,omitempty"`
	// Site is the account's page on the website.
	Site string `json:"site,omitempty"`
	// Today is the server's count (nil: not known now).
	Today *Totals `json:"today,omitempty"`
	// Now is the project being worked on, "" when there is none.
	Now string `json:"now,omitempty"`
	// Problem is one fixed code (Problem*), "" when all is well; Tool is
	// the AI tool it is about.
	Problem string `json:"problem,omitempty"`
	Tool    string `json:"tool,omitempty"`
	// Updates: this build can update itself (a release build).
	Updates bool `json:"updates,omitempty"`
}

// Totals are the account's numbers as the website shows them.
type Totals struct {
	Seconds          float64 `json:"seconds"`
	UncheckedSeconds float64 `json:"unchecked_seconds"`
	UncheckedPrompts int     `json:"unchecked_prompts"`
}

const (
	ProblemSignedOut = "signed_out" // the server does not accept this computer
	ProblemServer    = "server"     // the last sync failed
	ProblemSilent    = "silent"     // Tool is in use but its hooks are silent
	ProblemNoTools   = "no_tools"   // no AI tool or editor is connected
)

// Reply is the daemon's answer to a click (POST /seatime/tray/send, /update).
type Reply struct {
	Result  string `json:"result"`            // Sent, Updated, ... below
	Version string `json:"version,omitempty"` // Updated: the new version
}

const (
	Sent      = "sent"
	SignedOut = "signed_out"
	Failed    = "failed"
	Updated   = "updated" // installed; the daemon is starting again on it
	Current   = "current" // already the newest release
	Stayed    = "stayed"  // the new release did not work here
)

// View is the menu, row by row. A row with no text is not shown.
type View struct {
	Today     string
	Unchecked string
	Now       string
	Problem   string
	SignIn    bool // "Sign in"
	Open      bool // "Open SeaTime" can be clicked
	Send      bool // "Send now" can be clicked
	Update    bool // "Check for updates"
	Version   string
}

// Menu lays the menu out for st; nil means the daemon did not answer.
// own is this binary's version.
func Menu(st *Status, own string) View {
	v := View{Version: "SeaTime " + own}
	if st == nil {
		v.Problem = "SeaTime is not running"
		return v
	}
	v.Now = "Now: idle"
	if st.Now != "" {
		v.Now = "Now: " + clip(st.Now, 40)
	}
	if st.Today != nil {
		v.Today = "Today: " + Dur(st.Today.Seconds)
		if u := Unchecked(st.Today.UncheckedSeconds, st.Today.UncheckedPrompts); u != "" {
			v.Unchecked = "Unchecked: " + u
		}
	}
	switch st.Problem {
	case ProblemSignedOut:
		v.Problem, v.SignIn = "Signed out", true
	case ProblemServer:
		v.Problem = "Can't reach the server"
	case ProblemSilent:
		v.Problem = "Not hearing from " + clip(st.Tool, 30)
	case ProblemNoTools:
		v.Problem = "No AI tool or editor connected"
	}
	v.Open = st.Site != ""
	v.Send = st.Account != "" && st.Problem != ProblemSignedOut
	v.Update = st.Updates
	return v
}

// starting is the menu while the daemon has not answered yet.
func (v View) starting() View {
	v.Problem = "Starting…"
	return v
}

// Flash is the short line shown in place of a clicked row once the daemon
// has answered; ok is false when it did not answer at all.
func Flash(r Reply, ok bool, own string) string {
	if !ok {
		return "SeaTime is not running"
	}
	switch r.Result {
	case Sent:
		return "Sent"
	case SignedOut:
		return "Signed out"
	case Updated:
		return "Updated to " + r.Version
	case Current:
		return "Up to date"
	case Stayed:
		return "Stayed on " + own
	}
	return "Did not work"
}

// Dur is a duration as the website writes it: "0m", "<1m", "12m", "2h 05m".
func Dur(sec float64) string {
	if sec <= 0 {
		return "0m"
	}
	m := int(sec/60 + 0.5)
	switch {
	case m == 0:
		return "<1m"
	case m < 60:
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh %02dm", m/60, m%60)
}

// Unchecked is what is not checked yet, as the website writes it: "12m",
// "3 prompts" or "12m + 3 prompts"; "" under a minute with no prompt.
func Unchecked(sec float64, prompts int) string {
	var parts []string
	if sec >= 60 {
		parts = append(parts, Dur(sec))
	}
	if prompts == 1 {
		parts = append(parts, "1 prompt")
	} else if prompts > 1 {
		parts = append(parts, fmt.Sprintf("%d prompts", prompts))
	}
	return strings.Join(parts, " + ")
}

// clip shortens s to n characters.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
