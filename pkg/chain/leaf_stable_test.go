package chain

import (
	"encoding/json"
	"testing"
	"time"

	"seatime/pkg/event"
)

func oldEdit() event.Event {
	return event.Event{Seq: 7, TS: time.Date(2026, 9, 24, 9, 2, 16, 123e6, time.UTC), Kind: event.Edit,
		Harness: "editor:vscode", Project: "github.com/me/app", Files: []string{"src/main.go"}, Write: true, Lines: 120, Canon: event.CanonPrivate}
}

// A record written before the event gained fields keeps its leaf: new
// fields are omitempty, so a zero value adds nothing to the canonical form.
// The hash below was computed before lineno and cursorpos existed.
func TestOldRecordLeafUnchanged(t *testing.T) {
	l, err := Leaf(oldEdit())
	if err != nil {
		t.Fatal(err)
	}
	if want := "f6000a66aac25a66524ecb0c59a1492da0f15b12ad8c09c4979b7457b3d34fdc"; l != want {
		t.Fatalf("leaf of a pre-lineno record changed: %s, want %s (old logs would no longer verify)", l, want)
	}
}

// The cursor fields are in the leaf: a record carrying them replays after a
// JSON round trip (what a bundle does), and changing one breaks the chain.
func TestCursorFieldsAreChained(t *testing.T) {
	e := oldEdit()
	e.LineNo, e.CursorPos = 42, 1337
	leaf, _ := Leaf(e)
	if old, _ := Leaf(oldEdit()); leaf == old {
		t.Fatal("lineno/cursorpos not bound by the leaf")
	}
	head, err := Next(Genesis, e)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Hide(e))
	var got event.Event
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.LineNo != 42 || got.CursorPos != 1337 {
		t.Fatalf("fields lost in the round trip: %+v", got)
	}
	cp := []Checkpoint{{Seq: e.Seq, Hash: head}}
	if _, err := Verify(Genesis, []Entry{{Seq: e.Seq, Event: &got}}, cp); err != nil {
		t.Fatalf("revealed record with cursor fields does not verify: %v", err)
	}
	got.CursorPos++
	if _, err := Verify(Genesis, []Entry{{Seq: e.Seq, Event: &got}}, cp); err == nil {
		t.Fatal("a changed cursor position still verifies")
	}
}

// The presence and run facts are in the leaf, and a record carrying them
// replays after a JSON round trip; the old record above keeps its leaf.
func TestPresenceFieldsAreChained(t *testing.T) {
	ms, tidle, yes := int64(2300), 16, true
	p := event.Event{Seq: 8, TS: time.Date(2026, 9, 24, 9, 3, 0, 0, time.UTC), Kind: event.Prompt, Harness: "claude-code",
		Project: "github.com/me/app", Words: 6, Canon: event.CanonPrivate, Presence: "input only", InputBeforeMS: &ms,
		TerminalIdleS: &tidle, FrontWindow: "yes", SessionPrompts: 3, OtherStopHook: &yes}
	s := event.Event{Seq: 9, TS: time.Date(2026, 9, 24, 9, 9, 0, 0, time.UTC), Kind: event.AgentStop, Harness: "claude-code",
		Project: "github.com/me/app", Canon: event.CanonPrivate, PromptInLog: &yes,
		Run: &event.RunFacts{Reads: 4, Writes: 2, Commands: 3, CommandsFailed: 1, FilesInside: 2, LinesWritten: 40},
		Git: &event.GitFacts{FilesChanged: 2, Insertions: 38, Deletions: 2, Head: "0123456789abcdef0123456789abcdef01234567"}}
	idle := event.Event{Seq: 10, TS: time.Date(2026, 9, 24, 9, 10, 0, 0, time.UTC), Kind: event.Idle, Harness: "seatime", IdleSrc: "gnome",
		WindowMS: 600000, Presence: "input only", InputSlices: [][2]int64{{1790240940000, 0b000000000111}}, Canon: event.CanonPrivate}
	head := Genesis
	var entries []Entry
	for _, e := range []event.Event{p, s, idle} {
		var err error
		if head, err = Next(head, e); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(Hide(e))
		var got event.Event
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, Entry{Seq: e.Seq, Event: &got})
	}
	cp := []Checkpoint{{Seq: 10, Hash: head}}
	if _, err := Verify(Genesis, entries, cp); err != nil {
		t.Fatalf("records with presence facts do not verify: %v", err)
	}
	*entries[1].Event.Run = event.RunFacts{Reads: 4, Writes: 2, Commands: 3, FilesInside: 2, LinesWritten: 40} // a failure hidden
	if _, err := Verify(Genesis, entries, cp); err == nil {
		t.Fatal("a changed run fact still verifies")
	}
}
