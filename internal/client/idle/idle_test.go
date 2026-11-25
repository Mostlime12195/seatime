package idle

import (
	"context"
	"errors"
	"testing"
	"time"

	"seatime/pkg/event"
)

func TestParseIOReg(t *testing.T) {
	out := []byte(`+-o IOHIDSystem  <class IOHIDSystem, id 0x100000245, registered, matched, active, busy 0 (0 ms), retain 23>
    {
      "HIDIdleTimeDelta" = 0
      "HIDIdleTime" = 7364583333
      "HIDParameters" = {"HIDKeyRepeat"=83333333}
    }`)
	d, err := ParseIOReg(out)
	if err != nil || d != 7364583333 {
		t.Fatalf("%v %v", d, err)
	}
	if _, err := ParseIOReg([]byte("nothing here")); err == nil {
		t.Fatal("want error")
	}
	if _, err := ParseIOReg([]byte(`"HIDIdleTime" = <data>`)); err == nil {
		t.Fatal("want error on non-numeric")
	}
}

func TestIORegUsesFixedPathAndPropagatesFailure(t *testing.T) {
	var got string
	s := IOReg{run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		got = name
		return nil, errors.New("exit 1")
	}}
	if _, err := s.Idle(context.Background()); err == nil || got != "/usr/sbin/ioreg" {
		t.Fatal(got, err)
	}
}

func TestGnomeAndXPrintIdleParse(t *testing.T) {
	g := GnomeMutter{run: func(context.Context, string, ...string) ([]byte, error) { return []byte("(uint64 4200,)\n"), nil }}
	if d, err := g.Idle(context.Background()); err != nil || d != 4200*time.Millisecond {
		t.Fatal(d, err)
	}
	x := XPrintIdle{run: func(context.Context, string, ...string) ([]byte, error) { return []byte("1500\n"), nil }}
	if d, err := x.Idle(context.Background()); err != nil || d != 1500*time.Millisecond {
		t.Fatal(d, err)
	}
	bad := GnomeMutter{run: func(context.Context, string, ...string) ([]byte, error) { return []byte("Error"), nil }}
	if _, err := bad.Idle(context.Background()); err == nil {
		t.Fatal("want error")
	}
}

func TestFirstFallsBackToNoneWithReasons(t *testing.T) {
	fail := Func{N: "a", F: func(context.Context) (time.Duration, error) { return 0, errors.New("nope") }}
	ok := Func{N: "b", F: func(context.Context) (time.Duration, error) { return time.Second, nil }}
	if s := First(context.Background(), fail, ok); s.Name() != "b" {
		t.Fatal(s.Name())
	}
	s := First(context.Background(), fail)
	if _, err := s.Idle(context.Background()); s.Name() != "none" || !errors.Is(err, ErrUnavailable) {
		t.Fatal(s.Name(), err)
	}
}

// fake clock + scripted idle readings
type script struct {
	now  time.Time
	idle time.Duration
	err  error
	evs  []event.Event
}

func (s *script) rec() *Recorder {
	return &Recorder{
		Src:  Func{N: "ioreg", F: func(context.Context) (time.Duration, error) { return s.idle, s.err }},
		Now:  func() time.Time { return s.now },
		Emit: func(e event.Event) error { s.evs = append(s.evs, e); return nil },
	}
}

var t0 = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

func ms(t time.Time) int64 { return t.UnixMilli() }

func TestRecorderRecordsNoInputStretches(t *testing.T) {
	s := &script{now: t0}
	r := s.rec()
	ctx := context.Background()
	// 3 active minutes, then the person walks away for 6 minutes, then returns
	for i := 0; i <= 3; i++ {
		s.now, s.idle = t0.Add(time.Duration(i)*time.Minute), 5*time.Second
		r.Tick(ctx)
	}
	left := t0.Add(3 * time.Minute)
	for i := 4; i <= 9; i++ {
		s.now = t0.Add(time.Duration(i) * time.Minute)
		s.idle = s.now.Sub(left)
		r.Tick(ctx)
	}
	s.now, s.idle = t0.Add(10*time.Minute), 2*time.Second
	r.Tick(ctx) // 10 min with input: window written
	if len(s.evs) != 1 {
		t.Fatalf("events: %d", len(s.evs))
	}
	e := s.evs[0]
	if e.Kind != event.Idle || e.IdleSrc != "ioreg" || e.WindowMS != (10*time.Minute).Milliseconds() || !e.TS.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("%+v", e)
	}
	if len(e.NoInput) != 1 || e.NoInput[0][0] != ms(left) || e.NoInput[0][1] != ms(t0.Add(9*time.Minute)) {
		t.Fatalf("no-input stretches %v", e.NoInput)
	}
	if e.Kind.Activity() {
		t.Fatal("idle windows are not activity")
	}
}

// Rule 12: a tick gap over 90 s (sleep) or a backwards clock closes the window.
func TestRecorderNeverSpansSleepOrClockJump(t *testing.T) {
	s := &script{now: t0, idle: time.Second}
	r := s.rec()
	ctx := context.Background()
	r.Tick(ctx)
	s.now = t0.Add(time.Minute)
	r.Tick(ctx)
	s.now = t0.Add(8 * time.Hour) // laptop slept all night
	s.idle = 8 * time.Hour
	r.Tick(ctx)
	if len(s.evs) != 1 || s.evs[0].WindowMS != 60000 {
		t.Fatalf("sleep must close the window at the last tick: %+v", s.evs)
	}
	s.now = t0.Add(8*time.Hour + time.Minute)
	s.idle = time.Second
	r.Tick(ctx)
	s.now = t0.Add(5 * time.Hour) // clock jumped back three hours
	r.Tick(ctx)
	if len(s.evs) != 2 {
		t.Fatalf("clock jump must close the window: %d", len(s.evs))
	}
	w := s.evs[1]
	if w.WindowMS != 60000 {
		t.Fatalf("window after wake %+v", w)
	}
	// the first tick after waking reports the whole night without input;
	// it is kept (it only removes time) but never ends after the window
	for _, ni := range w.NoInput {
		if ni[1] > ms(t0.Add(8*time.Hour+time.Minute)) || ni[1] < ni[0] {
			t.Fatalf("bad no-input stretch: %v", ni)
		}
	}
	for _, e := range s.evs {
		if e.WindowMS < 0 {
			t.Fatal("negative window")
		}
	}
}

// No idle timer: windows are still written (the daemon was alive), labelled
// "none", with no stretches, so hours are kept and labelled.
func TestRecorderWithoutIdleTimer(t *testing.T) {
	s := &script{now: t0, err: ErrUnavailable}
	r := s.rec()
	for i := 0; i <= 60; i++ {
		s.now = t0.Add(time.Duration(i) * time.Minute)
		r.Tick(context.Background())
	}
	if len(s.evs) != 1 || s.evs[0].IdleSrc != "none" || len(s.evs[0].NoInput) != 0 || s.evs[0].WindowMS != 3600000 {
		t.Fatalf("%+v", s.evs)
	}
	if l := r.Last(); l.OK || l.Src != "none" || l.Err == "" {
		t.Fatalf("%+v", l)
	}
}

func TestRecorderFlushOnShutdownAndQuietHourIsOneEvent(t *testing.T) {
	s := &script{now: t0, idle: 3 * time.Hour}
	r := s.rec()
	for i := 0; i <= 30; i++ {
		s.now = t0.Add(time.Duration(i) * time.Minute)
		s.idle = 3*time.Hour + time.Duration(i)*time.Minute
		r.Tick(context.Background())
	}
	if len(s.evs) != 0 {
		t.Fatal("an untouched machine writes at most hourly")
	}
	r.Flush()
	if len(s.evs) != 1 || len(s.evs[0].NoInput) != 1 || s.evs[0].NoInput[0][0] != ms(t0.Add(-3*time.Hour)) {
		t.Fatalf("%+v", s.evs)
	}
	r.Flush()
	if len(s.evs) != 1 {
		t.Fatal("double flush")
	}
}

// The first "can be wrong" case: on a Wayland session
// with XWayland's DISPLAY set, the X11 counter is never used; GNOME's
// monitor is, when it is there; without it the session is not measured.
// On X11 both are candidates, GNOME first.
func TestLinuxSourcesNeverX11OnWayland(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	all := func(string) bool { return true }
	only := func(bin string) func(string) bool { return func(b string) bool { return b == bin } }
	names := func(ss []Source) (out []string) {
		for _, s := range ss {
			out = append(out, s.Name())
		}
		return out
	}
	cases := []struct {
		name  string
		env   map[string]string
		has   func(string) bool
		want  []string
		empty bool
	}{
		{"wayland, XWayland DISPLAY, GNOME", map[string]string{"WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0"}, all, []string{"gnome"}, false},
		{"wayland by session type", map[string]string{"XDG_SESSION_TYPE": "wayland", "DISPLAY": ":0"}, only("xprintidle"), nil, true},
		{"x11", map[string]string{"XDG_SESSION_TYPE": "x11", "DISPLAY": ":0"}, all, []string{"gnome", "xprintidle"}, false},
		{"x11 without gdbus", map[string]string{"DISPLAY": ":0"}, only("xprintidle"), []string{"xprintidle"}, false},
		{"no desktop", map[string]string{}, all, nil, true},
	}
	for _, c := range cases {
		got, why := LinuxSources(env(c.env), c.has)
		if g := names(got); len(g) != len(c.want) || (len(g) > 0 && g[0] != c.want[0]) || (len(g) > 1 && g[1] != c.want[1]) {
			t.Errorf("%s: %v, want %v", c.name, g, c.want)
		}
		if c.empty && why == "" {
			t.Errorf("%s: no reason given", c.name)
		}
	}
}

// Slices: a project is active from 10:00:00. The person types at 10:00:02
// and 10:00:31, then stops; sampled every 5 s to 10:01:55. Minute 10:00
// gets slice 0 (0-5 s) and slice 6 (30-35 s): mask 1 + 64 = 65. Minute
// 10:01 was sampled and saw no input: mask 0. A minute never sampled is
// not in the record at all.
func TestInputSlices(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	now := t0
	lastInput := t0.Add(-time.Hour)
	src := Func{N: "fake", F: func(context.Context) (time.Duration, error) { return now.Sub(lastInput), nil }}
	var got []event.Event
	r := &Recorder{Src: src, Now: func() time.Time { return now }, Emit: func(e event.Event) error { got = append(got, e); return nil },
		Presence: func(ok bool) string { return map[bool]string{true: "input only", false: "not measured"}[ok] }}
	r.Tick(context.Background())
	for s := 0; s < 120; s += 5 {
		now = t0.Add(time.Duration(s) * time.Second)
		for _, at := range []int{2, 31} {
			if s-5 < at && at <= s {
				lastInput = t0.Add(time.Duration(at) * time.Second)
			}
		}
		if s == 60 {
			r.Tick(context.Background()) // the minute tick goes on
		}
		r.Slice(context.Background())
	}
	now = t0.Add(2 * time.Minute)
	r.Tick(context.Background())
	r.Flush()
	if len(got) != 1 {
		t.Fatalf("%d records", len(got))
	}
	want := [][2]int64{{t0.UnixMilli(), 65}, {t0.Add(time.Minute).UnixMilli(), 0}}
	if s := got[0].InputSlices; len(s) != 2 || s[0] != want[0] || s[1] != want[1] || got[0].Presence != "input only" {
		t.Fatalf("slices %v presence %q, want %v", s, got[0].Presence, want)
	}
}

// While the front-window check answers, each sampled minute carries which
// slices had the AI tool's window in front; minutes it could not tell are
// left out (not measured).
func TestFrontSlices(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	now := t0
	src := Func{N: "fake", F: func(context.Context) (time.Duration, error) { return time.Second, nil }}
	var got []event.Event
	r := &Recorder{Src: src, Now: func() time.Time { return now }, Emit: func(e event.Event) error { got = append(got, e); return nil },
		Front: func(context.Context) (bool, bool) {
			s := now.Sub(t0)
			return s >= 10*time.Second && s < 20*time.Second, s < time.Minute
		}}
	r.Tick(context.Background())
	for s := 0; s < 120; s += 5 {
		now = t0.Add(time.Duration(s) * time.Second)
		if s == 60 {
			r.Tick(context.Background())
		}
		r.Slice(context.Background())
	}
	now = t0.Add(2 * time.Minute)
	r.Tick(context.Background())
	r.Flush()
	if len(got) != 1 {
		t.Fatalf("%d records", len(got))
	}
	want := [][2]int64{{t0.UnixMilli(), 1<<2 | 1<<3}}
	if f := got[0].FrontSlices; len(f) != 1 || f[0] != want[0] {
		t.Fatalf("front slices %v, want %v", f, want)
	}
}

// Live shows the open window without writing it.
func TestLive(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	now := t0
	src := Func{N: "fake", F: func(context.Context) (time.Duration, error) { return time.Second, nil }}
	emitted := 0
	r := &Recorder{Src: src, Now: func() time.Time { return now }, Emit: func(event.Event) error { emitted++; return nil }}
	if r.Live() != nil {
		t.Fatal("a window before any tick")
	}
	r.Tick(context.Background())
	for s := 5; s <= 30; s += 5 {
		now = t0.Add(time.Duration(s) * time.Second)
		r.Slice(context.Background())
	}
	e := r.Live()
	if e == nil || emitted != 0 || len(e.InputSlices) != 1 || e.WindowMS != 30000 {
		t.Fatalf("live %+v, emitted %d", e, emitted)
	}
}
