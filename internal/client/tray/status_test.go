package tray

import (
	"strings"
	"testing"
)

func TestMenu(t *testing.T) {
	// the daemon does not answer: one line, nothing to click
	v := Menu(nil, "v1.2.3")
	if v.Problem != "SeaTime is not running" || v.Today != "" || v.Now != "" || v.Open || v.Send || v.Update || v.SignIn {
		t.Errorf("no daemon: %+v", v)
	}
	if v.Version != "SeaTime v1.2.3" {
		t.Errorf("version row: %q", v.Version)
	}

	// working, signed in
	st := &Status{Version: "v1.2.3", Account: "ada", Site: "https://s/@ada", Now: "app", Updates: true,
		Today: &Totals{Seconds: 8100, UncheckedSeconds: 720, UncheckedPrompts: 3}}
	v = Menu(st, "v1.2.3")
	want := View{Today: "Today: 2h 15m", Unchecked: "Unchecked: 12m + 3 prompts", Now: "Now: app", Open: true, Send: true, Update: true, Version: "SeaTime v1.2.3"}
	if v != want {
		t.Errorf("working:\n got %+v\nwant %+v", v, want)
	}

	// nothing unchecked: no row; nothing going on: idle
	st.Today, st.Now = &Totals{Seconds: 20, UncheckedSeconds: 59}, ""
	if v = Menu(st, "v1"); v.Today != "Today: <1m" || v.Unchecked != "" || v.Now != "Now: idle" {
		t.Errorf("quiet: %+v", v)
	}

	// the server's numbers are not known: no numbers, the rest stays
	st.Today = nil
	if v = Menu(st, "v1"); v.Today != "" || v.Unchecked != "" || !v.Send || !v.Open {
		t.Errorf("no totals: %+v", v)
	}

	// signed out: say so, offer to sign in, nothing to send
	v = Menu(&Status{Problem: ProblemSignedOut, Account: "ada", Site: "https://s/@ada", SignIn: "https://s/device?code=X"}, "v1")
	if v.Problem != "Signed out" || !v.SignIn || v.Send || !v.Open {
		t.Errorf("signed out: %+v", v)
	}
	if v = Menu(&Status{Problem: ProblemSignedOut}, "v1"); !v.SignIn || v.Send || v.Open {
		t.Errorf("never signed in: %+v", v)
	}

	for code, text := range map[string]string{ProblemServer: "Can't reach the server", ProblemSilent: "Not hearing from Cursor", ProblemNoTools: "No AI tool or editor connected"} {
		if v = Menu(&Status{Account: "ada", Problem: code, Tool: "Cursor"}, "v1"); v.Problem != text || v.SignIn {
			t.Errorf("%s: %+v", code, v)
		}
	}

	// a development build does not offer to update
	if v = Menu(&Status{Account: "ada"}, "dev"); v.Update {
		t.Error("a development build offers an update")
	}
}

// The menu says nothing about how time is counted or checked: only short
// rows, no sentence and no time of day.
func TestMenuIsShort(t *testing.T) {
	long := strings.Repeat("a-very-long-project-name-", 5)
	v := Menu(&Status{Account: "ada", Now: long, Problem: ProblemSilent, Tool: long, Today: &Totals{Seconds: 1e6, UncheckedSeconds: 1e5, UncheckedPrompts: 1234}}, "v1.2.3")
	for _, row := range []string{v.Today, v.Unchecked, v.Now, v.Problem, v.Version} {
		if n := len([]rune(row)); n > 48 {
			t.Errorf("row of %d characters: %q", n, row)
		}
		if strings.Contains(row, ". ") {
			t.Errorf("row reads as a sentence: %q", row)
		}
	}
}

func TestDurAndUnchecked(t *testing.T) {
	for sec, want := range map[float64]string{0: "0m", 20: "<1m", 59.9 * 60: "1h 00m", 12 * 60: "12m", 3900: "1h 05m"} {
		if got := Dur(sec); got != want {
			t.Errorf("Dur(%v) = %q, want %q", sec, got, want)
		}
	}
	cases := []struct {
		sec     float64
		prompts int
		want    string
	}{{0, 0, ""}, {59, 0, ""}, {60, 0, "1m"}, {0, 1, "1 prompt"}, {720, 3, "12m + 3 prompts"}}
	for _, c := range cases {
		if got := Unchecked(c.sec, c.prompts); got != c.want {
			t.Errorf("Unchecked(%v, %d) = %q, want %q", c.sec, c.prompts, got, c.want)
		}
	}
}

func TestFlash(t *testing.T) {
	cases := []struct {
		r    Reply
		ok   bool
		want string
	}{
		{Reply{}, false, "SeaTime is not running"},
		{Reply{Result: Sent}, true, "Sent"},
		{Reply{Result: SignedOut}, true, "Signed out"},
		{Reply{Result: Failed}, true, "Did not work"},
		{Reply{Result: Updated, Version: "v1.2.4"}, true, "Updated to v1.2.4"},
		{Reply{Result: Current}, true, "Up to date"},
		{Reply{Result: Stayed}, true, "Stayed on v1.2.3"},
	}
	for _, c := range cases {
		if got := Flash(c.r, c.ok, "v1.2.3"); got != c.want {
			t.Errorf("Flash(%+v, %v) = %q, want %q", c.r, c.ok, got, c.want)
		}
	}
}

// One icon at a time: the second cannot take the lock, and Running and
// PID see the first.
func TestOneIcon(t *testing.T) {
	t.Setenv("SEATIME_HOME", t.TempDir())
	if Running() || PID() != 0 {
		t.Fatal("an icon runs before any started")
	}
	release, ok := acquire(0)
	if !ok {
		t.Fatal("the first icon cannot start")
	}
	if !Running() || PID() == 0 {
		t.Error("the running icon is not seen")
	}
	if _, ok := acquire(0); ok {
		t.Error("a second icon started")
	}
	release()
	if Running() || PID() != 0 {
		t.Error("the icon still counts as running after it quit")
	}
	Hide()
	if !Hidden() {
		t.Error("hidden is not remembered")
	}
	Unhide()
	if Hidden() {
		t.Error("hidden is not forgotten")
	}
}
