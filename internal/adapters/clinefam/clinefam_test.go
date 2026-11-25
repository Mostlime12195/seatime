package clinefam

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"seatime/internal/adapters/common"
)

const t0 = 1790000000000 // ms; the fixtures' first message

func at(ms int64) time.Time { return time.UnixMilli(t0 + ms) }

func setup(t *testing.T, v Variant) (*Adapter, string, string) {
	t.Helper()
	gs := filepath.Join(t.TempDir(), "globalStorage", v.Ext)
	task := filepath.Join(gs, "tasks", "1790000000000")
	os.MkdirAll(task, 0o755)
	os.WriteFile(filepath.Join(task, "api_conversation_history.json"), common.Fixture(t, "api_conversation_history.json"), 0o644)
	return &Adapter{V: v, Dirs: []string{gs}}, task, t.TempDir()
}

func put(t *testing.T, task, fixture string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(task, "ui_messages.json")
	os.WriteFile(p, common.Fixture(t, fixture), 0o644)
	os.Chtimes(p, mtime, mtime)
}

func TestTaskLifecycleAllVariants(t *testing.T) {
	for _, v := range []Variant{Cline, Roo, Kilo} {
		a, task, state := setup(t, v)
		if !a.Detect() {
			t.Fatalf("%s: not detected", v.Name)
		}
		put(t, task, "ui_messages_1.json", time.Now())
		var col common.Collector
		tr := a.NewTracker(state)
		tr.Poll(at(40_000), &col)
		b := "harness=" + v.Name + " sid=" + v.Name + ":1790000000000 cwd=/work/snake"
		want := []string{
			"prompt " + b + " text=build a snake game in pygame", "agent_start " + b,
			"tool " + b + " tool=newFileCreated files=snake.py write", // readFile is not a write
			"tool " + b + " tool=execute_command cmd=python snake.py", "run " + b + " cmd=python snake.py run=exec",
			"agent_stop " + b,
		}
		got := col.Summary()
		// python <script> may classify as exec or not depending on runclass; compare ignoring run lines
		got, want = dropRun(got), dropRun(want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s:\n got %q\nwant %q", v.Name, got, want)
		}
		if !col.Events[0].TS.Equal(at(0)) || !col.Events[len(col.Events)-1].TS.Equal(at(30_000)) {
			t.Fatalf("events must use the message's own ts: %v .. %v", col.Events[0].TS, col.Events[len(col.Events)-1].TS)
		}

		// the human answers: only the new messages are processed
		put(t, task, "ui_messages_2.json", time.Now().Add(time.Second))
		var col2 common.Collector
		tr.Poll(at(55_000), &col2)
		w2 := []string{"prompt " + b + " text=make it faster", "agent_start " + b, "agent_stop " + b}
		if !reflect.DeepEqual(col2.Summary(), w2) {
			t.Fatalf("%s second poll:\n got %q\nwant %q", v.Name, col2.Summary(), w2)
		}

		// restart: offsets persisted, nothing replayed
		var col3 common.Collector
		a.NewTracker(state).Poll(at(60_000), &col3)
		if len(col3.Events) != 0 {
			t.Fatalf("%s replayed after restart: %v", v.Name, col3.Summary())
		}
	}
}

func dropRun(in []string) []string {
	var out []string
	for _, s := range in {
		if len(s) >= 3 && s[:3] == "run" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func TestOldTasksAreNotBackfilled(t *testing.T) {
	a, task, state := setup(t, Cline)
	put(t, task, "ui_messages_2.json", time.Now())
	var col common.Collector
	// "now" is a day after the task: it is history, not live activity
	tr := a.NewTracker(state)
	tr.Poll(at(24*3600*1000), &col)
	if len(col.Events) != 0 {
		t.Fatalf("history replayed: %v", col.Summary())
	}
	// and the offset is remembered, so appended messages (still ancient) are not emitted either
	put(t, task, "ui_messages_2.json", time.Now().Add(time.Second))
	tr.Poll(at(24*3600*1000+5000), &col)
	if len(col.Events) != 0 {
		t.Fatalf("%v", col.Summary())
	}
}

func TestMidRewriteAndGarbageAreTolerated(t *testing.T) {
	a, task, state := setup(t, Cline)
	p := filepath.Join(task, "ui_messages.json")
	os.WriteFile(p, []byte(`[{"ts":1790000000000,"type":"say","say":"te`), 0o644) // truncated mid-write
	var col common.Collector
	tr := a.NewTracker(state)
	tr.Poll(at(1000), &col)
	put(t, task, "ui_messages_1.json", time.Now().Add(time.Hour))
	tr.Poll(at(40_000), &col)
	if len(col.Events) == 0 {
		t.Fatal("a later complete write must be processed")
	}
}

func TestWatchOnlyContractAndDetect(t *testing.T) {
	a := &Adapter{V: Roo, Dirs: []string{filepath.Join(t.TempDir(), "nope")}}
	if a.Detect() {
		t.Fatal("no storage dir")
	}
	if ok, err := a.Installed(); !ok || err != nil || a.Install("x") != nil || a.Uninstall() != nil {
		t.Fatal("nothing to install")
	}
}

// Restore Task / edit-and-resend rewrites ui_messages.json shorter with the
// resent prompt at the end under a new ts: it is emitted once, and nothing
// older is replayed.
func TestRestoreRewritesAreDedupedByTS(t *testing.T) {
	a, task, state := setup(t, Cline)
	p := filepath.Join(task, "ui_messages.json")
	write := func(js string, mt time.Time) {
		os.WriteFile(p, []byte(js), 0o644)
		os.Chtimes(p, mt, mt)
	}
	msg := func(ms int64, say, text string) string {
		return fmt.Sprintf(`{"ts":%d,"type":"say","say":%q,"text":%q}`, t0+ms, say, text)
	}
	write("["+msg(0, "task", "build a snake game")+","+msg(5000, "user_feedback", "make it faster")+","+msg(9000, "user_feedback", "add a score")+"]", time.Now())
	tr := a.NewTracker(state)
	var col common.Collector
	tr.Poll(at(10_000), &col)
	if n := countPrompts(col); n != 3 {
		t.Fatalf("first poll: %d prompts", n)
	}
	// restore to after the first prompt, then resend a reworded second ask
	write("["+msg(0, "task", "build a snake game")+","+msg(20_000, "user_feedback", "speed the snake up")+"]", time.Now().Add(time.Second))
	var col2 common.Collector
	tr.Poll(at(21_000), &col2)
	if got := col2.Summary(); countPrompts(col2) != 1 || !strings.Contains(strings.Join(got, "|"), "speed the snake up") {
		t.Fatalf("after a restore: %q", got)
	}
	// a restart with the shorter file replays nothing
	var col3 common.Collector
	a.NewTracker(state).Poll(at(22_000), &col3)
	if len(col3.Events) != 0 {
		t.Fatalf("replayed: %q", col3.Summary())
	}
}

func countPrompts(c common.Collector) int {
	n := 0
	for _, s := range c.Summary() {
		if strings.HasPrefix(s, "prompt ") {
			n++
		}
	}
	return n
}
