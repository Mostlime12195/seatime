package aider

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"seatime/internal/adapters/common"
)

func TestTailTurnLifecycle(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "code", "app")
	os.MkdirAll(proj, 0o755)
	hist := filepath.Join(proj, historyFile)
	os.WriteFile(hist, common.Fixture(t, "old_history.md"), 0o644)
	t0 := time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC)
	old := t0.Add(-24 * time.Hour)
	os.Chtimes(hist, old, old)

	var col common.Collector
	a := &Adapter{Roots: []string{root}}
	tr := a.NewTracker(t.TempDir())
	tr.Poll(t0, &col)
	if len(col.Events) != 0 {
		t.Fatalf("existing history must not be replayed: %v", col.Summary())
	}

	// the user submits a prompt and aider answers: both land in one append here
	f, _ := os.OpenFile(hist, os.O_APPEND|os.O_WRONLY, 0o644)
	f.Write(common.Fixture(t, "new_turn.md"))
	f.Close()
	tr.Poll(t0.Add(5*time.Second), &col)
	got := col.Summary()
	sid := col.Events[0].SessionID
	want := []string{
		"prompt harness=aider sid=" + sid + " cwd=" + proj + " text=add a --verbose flag to cli.py\nand log the args",
		"agent_start harness=aider sid=" + sid + " cwd=" + proj,
		"tool harness=aider sid=" + sid + " cwd=" + proj + " tool=edit files=" + filepath.Join(proj, "cli.py") + " write",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	if want := "aider:"; sid[:6] != want || sid == "aider:" {
		t.Fatalf("session id %q", sid)
	}
	// still growing: no stop yet; quiet for >= 4 s after the last growth: stop
	tr.Poll(t0.Add(7*time.Second), &col)
	if len(col.Events) != 3 {
		t.Fatalf("stop too early: %v", col.Summary())
	}
	tr.Poll(t0.Add(10*time.Second), &col)
	if len(col.Events) != 4 || col.Events[3].Kind != "agent_stop" || !col.Events[3].TS.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("agent_stop should be stamped at the last growth: %v", col.Summary())
	}
	tr.Poll(t0.Add(20*time.Second), &col)
	if len(col.Events) != 4 {
		t.Fatal("duplicate stop")
	}
}

func TestOffsetsSurviveRestartAndPartialLines(t *testing.T) {
	root := t.TempDir()
	hist := filepath.Join(root, historyFile)
	os.WriteFile(hist, []byte("# aider chat started at 2026-11-19 10:00:00\n\n"), 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(hist, old, old)
	state := t.TempDir()
	a := &Adapter{Roots: []string{root}}
	var col common.Collector
	tr := a.NewTracker(state)
	t0 := time.Now()
	tr.Poll(t0, &col)
	// aider is mid-write: half a line only
	f, _ := os.OpenFile(hist, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("#### fix the fla")
	tr.Poll(t0.Add(time.Second), &col)
	if len(col.Events) != 0 {
		t.Fatalf("partial line must wait: %v", col.Summary())
	}
	f.WriteString("ky test\n\n")
	f.Close()
	tr.Poll(t0.Add(2*time.Second), &col)
	if len(col.Events) != 2 || col.Events[0].Text != "fix the flaky test" {
		t.Fatalf("%v", col.Summary())
	}
	// daemon restart: a new tracker resumes at the stored offset instead of replaying
	tr2 := a.NewTracker(state)
	var col2 common.Collector
	tr2.Poll(t0.Add(3*time.Second), &col2)
	if len(col2.Events) != 0 {
		t.Fatalf("replayed after restart: %v", col2.Summary())
	}
}

func TestDiscoverBounds(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) {
		os.MkdirAll(filepath.Join(root, rel), 0o755)
		os.WriteFile(filepath.Join(root, rel, historyFile), nil, 0o644)
	}
	mk("a")
	mk("a/b/c")        // depth 3: found
	mk("a/b/c/d")      // depth 4: too deep
	mk("node_modules") // skipped
	mk(".hidden")      // skipped
	got := Discover([]string{root})
	if len(got) != 2 {
		t.Fatalf("%v", got)
	}
}

func TestWatchOnlyContract(t *testing.T) {
	a := New()
	if ok, err := a.Installed(); !ok || err != nil || a.Install("x") != nil || a.Uninstall() != nil {
		t.Fatal("watch-only adapters have nothing to install")
	}
	if out, err := a.HandleHook("x", nil, &common.Collector{}); out != nil || err != nil {
		t.Fatal("no hooks")
	}
}

// A history file whose mtime is ahead of the watcher's clock (clock jumped back, or a
// skewed network drive) is old history, not a live session: it must not be replayed.
func TestFutureMtimeIsNotReplayed(t *testing.T) {
	root := t.TempDir()
	hist := filepath.Join(root, historyFile)
	os.WriteFile(hist, []byte("# aider chat started at 2026-11-19 10:00:00\n\n#### old prompt\n\nanswer\n"), 0o644)
	t0 := time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC)
	future := t0.Add(6 * time.Hour)
	os.Chtimes(hist, future, future)
	var col common.Collector
	tr := (&Adapter{Roots: []string{root}}).NewTracker(t.TempDir())
	tr.Poll(t0, &col)
	tr.Poll(t0.Add(10*time.Second), &col)
	if len(col.Events) != 0 {
		t.Fatalf("future-dated history replayed: %v", col.Summary())
	}
}
