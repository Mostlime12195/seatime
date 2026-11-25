// Package adapters connects AI harnesses (and other sources) to seatime.
// Contract owned by lead; the adapters agent implements one subpackage per
// harness and registers it in registry.go. The client calls these.
package adapters

import (
	"context"

	"seatime/pkg/event"
)

// Sink receives normalized events. The client's implementation fills Seq, the
// hash chain, project attribution (from CWD when Project is empty) and prompt
// text features (Novelty/SimPrev/TextHash/Chars/Words) — adapters need only set
// Kind, TS, Harness, SessionID, CWD, Model and the kind-specific raw fields.
type Sink interface {
	Emit(e event.Event) error
}

// Adapter is one harness integration.
type Adapter interface {
	// Name is the stable id used in `seatime hook <name> <event>` and in Event.Harness.
	Name() string
	// Detect reports whether the harness appears installed for this user.
	Detect() bool
	// Installed reports whether seatime's hooks/plugin are currently installed.
	Installed() (bool, error)
	// Install writes the harness hook config/plugin so the harness invokes
	// `<seatimeBin> hook <name> <hookEvent>` (stdin = harness hook payload).
	// Must be idempotent, preserve the user's other config, and back up files it edits.
	Install(seatimeBin string) error
	Uninstall() error
	// HandleHook parses one hook invocation and emits zero or more events.
	// Must be fast and must not write to stdout unless the harness protocol
	// expects it (return the bytes to print, or nil).
	HandleHook(hookEvent string, stdin []byte, sink Sink) (stdout []byte, err error)
}

// Watcher is optionally implemented by adapters that tail harness logs instead
// of (or in addition to) hooks. The daemon runs Watch until ctx is done and is
// responsible for nothing else; the adapter must persist its own read offsets
// under stateDir to avoid duplicate events across restarts.
type Watcher interface {
	Watch(ctx context.Context, stateDir string, sink Sink) error
}
