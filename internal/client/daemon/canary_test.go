package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"seatime/internal/client/canary"
	"seatime/internal/client/config"
	"seatime/internal/client/emit"
	"seatime/internal/client/store"
	"seatime/pkg/event"
)

// Claude Code in use, hooks silent: the daemon records it for status, fills
// the gap from the transcript once (labelled), and clears it when hooks are
// heard again.
func TestCanaryBackfillsOnceAndClears(t *testing.T) {
	sandbox(t)
	home := os.Getenv("SEATIME_FAKE_HOME")
	f := false
	config.Save(config.Config{AutoHook: &f})
	now := time.Now().UTC().Truncate(time.Second)
	tr := filepath.Join(home, ".claude", "projects", "-w-app", "s.jsonl")
	os.MkdirAll(filepath.Dir(tr), 0o755)
	ts := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	os.WriteFile(tr, []byte(
		`{"type":"user","timestamp":"`+ts(-5*time.Minute)+`","sessionId":"s","cwd":"/w/app","message":{"content":"fix the build"}}`+"\n"+
			`{"type":"assistant","timestamp":"`+ts(-4*time.Minute)+`","sessionId":"s","message":{"content":"ok"}}`+"\n"), 0o600)
	st, err := store.Open(config.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := &Daemon{Store: st, Pipeline: emit.New(st, home), Probes: canary.Probes()}
	n, err := d.CanaryOnce(now)
	if err != nil || n != 3 {
		t.Fatalf("backfilled %d: %v", n, err)
	}
	if v, _ := st.GetMeta(MetaSilent); v == "" {
		t.Fatal("status needs the silent harness recorded")
	}
	if n, _ := d.CanaryOnce(now.Add(time.Minute)); n != 0 {
		t.Fatalf("second pass re-backfilled %d events", n)
	}
	st.Append(event.Event{Kind: event.Prompt, Harness: "claude-code", TS: now})
	d.CanaryOnce(now.Add(2 * time.Minute))
	if v, _ := st.GetMeta(MetaSilent); v != "" {
		t.Fatalf("hooks heard again, still flagged: %s", v)
	}
}
