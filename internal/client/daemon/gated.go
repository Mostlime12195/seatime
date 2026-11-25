package daemon

import (
	"sync"
	"time"

	"seatime/internal/adapters"
	"seatime/internal/client/config"
	"seatime/pkg/event"
)

// gatedSink drops a watcher's events while the user has opted that harness out
// (`seatime uninstall --harness X` writes the opt-out to config.json). Watcher-style
// adapters (Aider, Cline, Amp, ...) have no hooks to remove, so this is what makes
// the opt-out real; the config is re-read at most every 5 seconds.
type gatedSink struct {
	name  string
	inner adapters.Sink

	mu      sync.Mutex
	checked time.Time
	out     bool
}

func (g *gatedSink) Emit(e event.Event) error {
	g.mu.Lock()
	if time.Since(g.checked) > 5*time.Second {
		g.checked = time.Now()
		g.out = config.Load().OptedOut(g.name)
	}
	out := g.out
	g.mu.Unlock()
	if out {
		return nil
	}
	return g.inner.Emit(e)
}
