package spool

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"seatime/pkg/event"
)

func ev(text string) event.Event {
	return event.Event{Kind: event.Prompt, Harness: "claude-code", SessionID: "s1", Text: text,
		TS: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)}
}

func TestWriteDrainRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, []event.Event{ev("a"), ev("b")}); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, []event.Event{ev("c")}); err != nil {
		t.Fatal(err)
	}
	if n, _ := Count(dir); n != 2 {
		t.Fatalf("count %d", n)
	}
	var got []string
	n, err := Drain(dir, func(e event.Event) error { got = append(got, e.Text); return nil })
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("order %v", got)
	}
	if n, _ := Count(dir); n != 0 {
		t.Fatal("drained files must be removed")
	}
}

func TestDrainKeepsFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	Write(dir, []event.Event{ev("a")})
	if _, err := Drain(dir, func(event.Event) error { return errors.New("db locked") }); err == nil {
		t.Fatal("want error")
	}
	if n, _ := Count(dir); n != 1 {
		t.Fatal("a failed drain must keep the file for the next try")
	}
}

func TestPartialAndJunkFilesAreSafe(t *testing.T) {
	dir := t.TempDir()
	// a writer that died before the rename leaves only a temp file: never read
	os.WriteFile(filepath.Join(dir, ".tmp-123"), []byte(`{"kind":"prompt"`), 0o600)
	// a damaged file: bad lines are skipped, good ones kept
	os.WriteFile(filepath.Join(dir, "0000000000000000001-1.jsonl"),
		[]byte("not json\n{\"kind\":\"prompt\",\"text\":\"ok\"}\n{\"text\":\"no kind\"}\n{\"kind\":\"agent_st"), 0o600)
	var got []string
	n, err := Drain(dir, func(e event.Event) error { got = append(got, e.Text); return nil })
	if err != nil || n != 1 || got[0] != "ok" {
		t.Fatalf("n=%d err=%v got=%v", n, err, got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".tmp-123")); err != nil {
		t.Fatal("a fresh temp file belongs to a live writer and must be left alone")
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(filepath.Join(dir, ".tmp-123"), old, old)
	Drain(dir, func(event.Event) error { return nil })
	if _, err := os.Stat(filepath.Join(dir, ".tmp-123")); err == nil {
		t.Fatal("stale temp files are cleaned up")
	}
}

func TestSpoolIsBounded(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < MaxFiles; i++ {
		os.WriteFile(filepath.Join(dir, filepathName(i)), []byte("{}\n"), 0o600)
	}
	if err := Write(dir, []event.Event{ev("x")}); !errors.Is(err, ErrFull) {
		t.Fatalf("want ErrFull, got %v", err)
	}
	if err := Write(t.TempDir(), nil); err != nil {
		t.Fatal("nothing to write is not an error")
	}
}

func filepathName(i int) string { return time.Unix(0, int64(i)).Format("150405.000000000") + ".jsonl" }

func TestDrainMissingDir(t *testing.T) {
	n, err := Drain(filepath.Join(t.TempDir(), "nope"), func(event.Event) error { return nil })
	if n != 0 || err != nil {
		t.Fatal(n, err)
	}
}
