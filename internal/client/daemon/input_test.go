package daemon

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/idle"
)

// A prompt's hook asks the running daemon, over its local endpoint, how
// much input came before the prompt. With no daemon, or a daemon with no
// idle timer, it gets no answer rather than "no input"; a page in a
// browser on another host name gets nothing.
func TestHookAsksTheDaemonForInput(t *testing.T) {
	d, h := testDaemon(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	cfg := config.Config{WakaPort: ln.Addr().(*net.TCPAddr).Port}

	if _, ok := InputSince(cfg, time.Now().Add(-time.Minute)); ok {
		t.Fatal("an answer before the daemon reads the idle timer")
	}
	now := time.Now().UTC().Truncate(time.Second)
	t0, last := now, now.Add(-time.Hour)
	d.Idle = &idle.Recorder{Now: func() time.Time { return now }, Active: func() bool { return true },
		Src: idle.Func{N: "fake", F: func(context.Context) (time.Duration, error) { return now.Sub(last), nil }}}
	d.Idle.Tick(context.Background())
	for i := 0; i < 24; i++ { // two minutes; the person types through the second one
		now = now.Add(idle.SliceEvery)
		if i >= 12 {
			last = now.Add(-time.Second)
		}
		d.Idle.Fast(context.Background())
	}
	in, ok := InputSince(cfg, t0)
	if !ok || in.Input != time.Minute || in.Unwatched != 0 || in.Over != 2*time.Minute {
		t.Fatalf("since the start: %+v %v", in, ok)
	}
	if in, ok := InputSince(cfg, time.Time{}); !ok || in.Input != time.Minute || in.Over != idle.MeterKeep {
		t.Fatalf("as far back as it counts: %+v %v", in, ok)
	}
	if w := do(h, req{method: "GET", path: "/seatime/input?since=0", host: "evil.example"}); w.Code != 403 {
		t.Fatalf("another host name: %d", w.Code)
	}
	d.Idle = &idle.Recorder{Src: idle.None{Reason: "no desktop"}}
	if _, ok := InputSince(cfg, t0); ok {
		t.Fatal("an answer with no idle timer")
	}
	srv.Close()
	if _, ok := InputSince(cfg, t0); ok {
		t.Fatal("an answer with no daemon")
	}
}
