package adapters

import (
	"context"

	"seatime/internal/adapters/aider"
	"seatime/internal/adapters/amp"
	"seatime/internal/adapters/antigravity"
	"seatime/internal/adapters/claudecode"
	"seatime/internal/adapters/clinefam"
	"seatime/internal/adapters/codex"
	"seatime/internal/adapters/common"
	"seatime/internal/adapters/copilot"
	"seatime/internal/adapters/cursor"
	"seatime/internal/adapters/gemini"
	"seatime/internal/adapters/opencode"
	"seatime/internal/adapters/windsurf"
)

// Subpackages cannot import this package (import cycle), so they implement the
// structurally identical common.Adapter; wrap adapts them to Adapter/Watcher.
type wrap struct{ common.Adapter }

func (w wrap) HandleHook(hookEvent string, stdin []byte, sink Sink) ([]byte, error) {
	return w.Adapter.HandleHook(hookEvent, stdin, sink)
}

type wrapW struct {
	wrap
	w common.Watcher
}

func (w wrapW) Watch(ctx context.Context, stateDir string, sink Sink) error {
	return w.w.Watch(ctx, stateDir, sink)
}

func wrapAdapter(a common.Adapter) Adapter {
	if cw, ok := a.(common.Watcher); ok {
		return wrapW{wrap{a}, cw}
	}
	return wrap{a}
}

// All returns every registered harness adapter, in priority order.
func All() []Adapter {
	return []Adapter{
		wrapAdapter(claudecode.New()),
		wrapAdapter(codex.New()),
		wrapAdapter(cursor.New()),
		wrapAdapter(gemini.New()),
		wrapAdapter(opencode.New()),
		// 2026 additions: hook-based first, then log/transcript watchers.
		wrapAdapter(copilot.New()),
		wrapAdapter(windsurf.New()),
		wrapAdapter(antigravity.New()),
		wrapAdapter(opencode.NewKilo()),
		wrapAdapter(aider.New()),
		wrapAdapter(clinefam.New(clinefam.Cline)),
		wrapAdapter(clinefam.New(clinefam.Roo)),
		wrapAdapter(clinefam.New(clinefam.Kilo)),
		wrapAdapter(amp.New()),
	}
}

// ByName returns the adapter with the given Name(), or nil.
func ByName(name string) Adapter {
	for _, a := range All() {
		if a.Name() == name {
			return a
		}
	}
	return nil
}
