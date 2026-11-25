package presence

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeIdle struct {
	d   time.Duration
	err error
}

func (f fakeIdle) Idle(context.Context) (time.Duration, error) { return f.d, f.err }

// a process tree: 900 terminal app -> 910 shell -> 920 tool -> 930 hook,
// and 800 a game; 950 a tmux client under the terminal app.
type fakeWindows struct {
	front int
	err   error
}

var parents = map[int]int{930: 920, 920: 910, 910: 900, 900: 1, 950: 900, 800: 1}

func (f fakeWindows) FrontPID(context.Context) (int, error) { return f.front, f.err }
func (fakeWindows) Parent(pid int) (int, error) {
	if p, ok := parents[pid]; ok {
		return p, nil
	}
	return 0, errors.New("no such process")
}

type fakeTmux struct{ clients []int }

func (f fakeTmux) Clients(context.Context) ([]int, error) { return f.clients, nil }

func env(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

var ctx = context.Background()

// A desktop: the idle timer says the last input was 2.3 s ago. That is
// recorded as 2300 ms, level "input only"; the front window is not asked
// while the switch is off.
func TestPromptOnADesktop(t *testing.T) {
	p := Probe{Getenv: env(nil), Idle: fakeIdle{d: 2300 * time.Millisecond}, Windows: fakeWindows{front: 900}, Self: 930}
	f := p.AtPrompt(ctx)
	if f.InputBeforeMS == nil || *f.InputBeforeMS != 2300 || f.Presence != InputOnly || f.FrontWindow != "" || f.TerminalIdleS != nil {
		t.Fatalf("%+v", f)
	}
}

// The switch on: the terminal app (900) is in front and is an ancestor of
// the hook (930 -> 920 -> 910 -> 900): "yes". A game (800) in front: "no".
// Inside tmux with no client attached: "no window attached"; with the
// client 950, whose parent is 900, attached: "yes".
func TestFrontWindow(t *testing.T) {
	base := Probe{Getenv: env(nil), Idle: fakeIdle{d: time.Second}, FrontOn: true, Self: 930}
	for _, c := range []struct {
		name string
		p    func(Probe) Probe
		want string
	}{
		{"own terminal in front", func(p Probe) Probe { p.Windows = fakeWindows{front: 900}; return p }, FrontYes},
		{"a game in front", func(p Probe) Probe { p.Windows = fakeWindows{front: 800}; return p }, FrontNo},
		{"tmux, nobody attached", func(p Probe) Probe {
			p.Getenv, p.Windows, p.Tmux = env(map[string]string{"TMUX": "/tmp/tmux-1/default,1,0"}), fakeWindows{front: 900}, fakeTmux{}
			return p
		}, FrontNone},
		{"tmux, attached in the front terminal", func(p Probe) Probe {
			p.Getenv, p.Windows, p.Tmux = env(map[string]string{"TMUX": "x"}), fakeWindows{front: 900}, fakeTmux{clients: []int{950}}
			return p
		}, FrontYes},
	} {
		f := c.p(base).AtPrompt(ctx)
		if f.FrontWindow != c.want || f.Presence != InputFront {
			t.Errorf("%s: %+v, want %q", c.name, f, c.want)
		}
	}
	// the system cannot say: the field is left out and the level is input only
	p := base
	p.Windows = fakeWindows{err: ErrUnsupported}
	if f := p.AtPrompt(ctx); f.FrontWindow != "" || f.Presence != InputOnly {
		t.Errorf("unsupported front window: %+v", f)
	}
}

// The third "can be wrong" case: a tool run over SSH on a
// Mac. The Mac's own timer truthfully says nobody touched it for 10 min
// while the person types on another computer; it must not be used. The
// terminal saw keys 21 s ago, recorded in 8 s steps as 16 s.
func TestSSHUsesTheTerminalOnly(t *testing.T) {
	p := Probe{Getenv: env(map[string]string{"SSH_CONNECTION": "10.0.0.2 52000 10.0.0.9 22"}),
		Idle: fakeIdle{d: 10 * time.Minute}, Terminal: fakeIdle{d: 21 * time.Second}, Self: 930}
	f := p.AtPrompt(ctx)
	if f.InputBeforeMS != nil || f.TerminalIdleS == nil || *f.TerminalIdleS != 16 || f.Presence != TerminalOnly {
		t.Fatalf("%+v", f)
	}
}

// The second "can be wrong" case: a Wayland desktop SeaTime cannot read
// correctly (idle.LinuxSources gives no source there) and no terminal:
// "not measured", with no number at all, never "no input".
func TestNothingReadIsNotMeasured(t *testing.T) {
	p := Probe{Getenv: env(map[string]string{"WAYLAND_DISPLAY": "wayland-0"}), Self: 930}
	if f := p.AtPrompt(ctx); f.Presence != NotMeasured || f.InputBeforeMS != nil || f.TerminalIdleS != nil {
		t.Fatalf("%+v", f)
	}
	failing := Probe{Getenv: env(nil), Idle: fakeIdle{err: errors.New("gdbus: no reply")}, Self: 930}
	if f := failing.AtPrompt(ctx); f.Presence != NotMeasured {
		t.Fatalf("%+v", f)
	}
}

// A machine with no desktop (a server reached without SSH variables, such
// as a local console): the terminal is read. 7 s rounds down to 0.
func TestNoDesktopUsesTheTerminal(t *testing.T) {
	p := Probe{Getenv: env(nil), Terminal: fakeIdle{d: 7 * time.Second}, Self: 930}
	if f := p.AtPrompt(ctx); f.Presence != TerminalOnly || *f.TerminalIdleS != 0 {
		t.Fatalf("%+v", f)
	}
}

func TestLevel(t *testing.T) {
	for _, c := range []struct {
		idle, remote, on, supported bool
		want                        string
	}{
		{true, false, false, true, InputOnly},
		{true, false, true, true, InputFront},
		{true, false, true, false, InputOnly},
		{true, true, false, false, TerminalOnly},
		{false, false, false, false, NotMeasured},
	} {
		if got := Level(c.idle, c.remote, c.on, c.supported); got != c.want {
			t.Errorf("%+v: %q", c, got)
		}
	}
}

func TestParsers(t *testing.T) {
	if id, err := ParseXpropActive([]byte("_NET_ACTIVE_WINDOW(WINDOW): window id # 0x3a00007\n")); err != nil || id != "0x3a00007" {
		t.Errorf("%q %v", id, err)
	}
	if _, err := ParseXpropActive([]byte("_NET_ACTIVE_WINDOW(WINDOW): window id # 0x0")); err == nil {
		t.Error("no active window accepted")
	}
	if pid, err := ParseXpropPID([]byte("_NET_WM_PID(CARDINAL) = 4242\n")); err != nil || pid != 4242 {
		t.Errorf("%d %v", pid, err)
	}
	if pid, err := ParseLsappinfoPID([]byte(`"pid"=812` + "\n")); err != nil || pid != 812 {
		t.Errorf("%d %v", pid, err)
	}
	if ppid, err := ParseProcStatPPID([]byte("1234 (a (b) c) S 1200 1234 1234 0 -1")); err != nil || ppid != 1200 {
		t.Errorf("%d %v", ppid, err)
	}
	if ns := ParseInts([]byte("123\n\nx\n456\n")); len(ns) != 2 || ns[0] != 123 || ns[1] != 456 {
		t.Errorf("%v", ns)
	}
}

// tmux is asked for the clients of this pane's session, by name.
func TestTmuxAsksForThisSession(t *testing.T) {
	var calls [][]string
	tm := tmuxClients{run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if args[0] == "display-message" {
			return []byte("work\n"), nil
		}
		return []byte("950\n951\n"), nil
	}}
	cs, err := tm.Clients(ctx)
	if err != nil || len(cs) != 2 || len(calls) != 2 || calls[1][3] != "work" {
		t.Fatalf("%v %v %v", cs, err, calls)
	}
}

// The tool's window chain for the 5 s front check: ancestors of the hook,
// or of the attached tmux clients; never names.
func TestChain(t *testing.T) {
	p := Probe{Getenv: env(nil), Self: 930, Windows: fakeWindows{}}
	chain := p.Chain(ctx)
	if !InChain(900, chain) || !InChain(920, chain) || InChain(800, chain) || InChain(1, chain) {
		t.Fatalf("chain %v", chain)
	}
	tm := Probe{Getenv: env(map[string]string{"TMUX": "x"}), Self: 930, Windows: fakeWindows{}, Tmux: fakeTmux{[]int{950}}}
	if c := tm.Chain(ctx); !InChain(950, c) || !InChain(900, c) || InChain(920, c) {
		t.Fatalf("tmux chain %v", c)
	}
	if (Probe{Getenv: env(nil), Self: 930}).Chain(ctx) != nil {
		t.Fatal("a chain without a window reader")
	}
}
