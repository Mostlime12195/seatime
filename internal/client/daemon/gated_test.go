package daemon

import (
	"testing"
	"time"

	"seatime/internal/client/config"
	"seatime/pkg/event"
)

type countSink struct{ n int }

func (c *countSink) Emit(event.Event) error { c.n++; return nil }

func TestGatedSinkHonoursOptOut(t *testing.T) {
	sandbox(t)
	in := &countSink{}
	g := &gatedSink{name: "aider", inner: in}
	g.Emit(event.Event{Kind: event.Prompt})
	if in.n != 1 {
		t.Fatal("not opted out: events flow")
	}
	cfg := config.Load()
	cfg.SetOptOut("aider", true)
	config.Save(cfg)
	g.checked = time.Time{} // the real check is throttled to 5 s
	g.Emit(event.Event{Kind: event.Prompt})
	if in.n != 1 {
		t.Fatal("opted-out watcher must be silenced")
	}
	cfg.SetOptOut("aider", false)
	config.Save(cfg)
	g.checked = time.Time{}
	g.Emit(event.Event{Kind: event.Prompt})
	if in.n != 2 {
		t.Fatal("re-enabled watcher must flow again")
	}
}
