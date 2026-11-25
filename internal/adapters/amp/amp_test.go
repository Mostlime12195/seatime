package amp

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"seatime/internal/adapters/common"
)

const t0 = 1790000000000

func at(ms int64) time.Time { return time.UnixMilli(t0 + ms) }

func TestThreadToEvents(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(`{"token":"do-not-read"}`), 0o600)
	p := filepath.Join(dir, "T-0001.json")
	os.WriteFile(p, common.Fixture(t, "thread_1.json"), 0o644)

	a := &Adapter{Dir: dir}
	if !a.Detect() {
		t.Fatal("threads dir present")
	}
	var col common.Collector
	state := t.TempDir()
	tr := a.NewTracker(state)
	tr.Poll(at(40_000), &col)

	b := "harness=amp sid=amp:T-0001 cwd=/work/api"
	want := []string{
		"prompt " + b + " text=add pagination to the users endpoint", "agent_start " + b,
		"tool " + b + " tool=Read",
		"tool " + b + " tool=edit_file files=/work/api/users.go write",
		"tool " + b + " tool=Bash cmd=go test ./...", "run " + b + " cmd=go test ./... run=test",
		"agent_stop " + b,
	}
	if got := col.Summary(); !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	if !col.Events[0].TS.Equal(at(0)) || !col.Events[len(col.Events)-1].TS.Equal(at(30_000)) {
		t.Fatalf("meta.sentAt must be the event time")
	}
	for _, e := range col.Events {
		if strings.Contains(e.Text, "do-not-read") {
			t.Fatal("secrets.json was read")
		}
	}
	// unchanged file: nothing; restart: nothing
	var again common.Collector
	tr.Poll(at(45_000), &again)
	a.NewTracker(state).Poll(at(45_000), &again) // restart with the same state dir
	if len(again.Events) != 0 {
		t.Fatalf("duplicate emission: %v", again.Summary())
	}
}

func TestNoBackfillOfOldThreads(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "T-0001.json"), common.Fixture(t, "thread_1.json"), 0o644)
	var col common.Collector
	state := t.TempDir()
	a := &Adapter{Dir: dir}
	a.NewTracker(state).Poll(at(3*3600*1000), &col)
	if len(col.Events) != 0 {
		t.Fatalf("history replayed: %v", col.Summary())
	}
}

func TestFileURIPath(t *testing.T) {
	if got := fileURIPath("file:///work/app"); filepath.ToSlash(got) != "/work/app" {
		t.Fatalf("%q", got)
	}
	if fileURIPath("https://x/y") != "" || fileURIPath("::") != "" {
		t.Fatal("only file URIs")
	}
}
