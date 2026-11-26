package records

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/emit"
	"seatime/internal/client/segment"
	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/event"
)

// server is a stand-in for the server's side of the records: it holds
// checkpoints, keeps what replays to them and answers as the real one.
type server struct {
	mu     sync.Mutex
	gone   bool             // an older server: no such address
	cps    map[int64]string // checkpoints by seq
	head   int64
	reqs   []api.RecordsReq
	stored []chain.Entry
}

func (f *server) next(base int64) int64 {
	var best int64
	for seq := range f.cps {
		if seq > base && (best == 0 || seq < best) {
			best = seq
		}
	}
	return best
}

func (f *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone || r.URL.Path != "/api/v1/records" {
		http.NotFound(w, r)
		return
	}
	var req api.RecordsReq
	json.NewDecoder(r.Body).Decode(&req)
	f.reqs = append(f.reqs, req)
	if n := len(req.Entries); n > 0 {
		last := req.Entries[n-1].Seq
		want, ok := f.cps[last]
		got, err := chain.Verify(req.StartHash, req.Entries, nil)
		if !ok || err != nil || got != want {
			w.WriteHeader(409)
		} else {
			for _, en := range req.Entries {
				if en.Seq > f.head {
					f.stored = append(f.stored, en)
				}
			}
			f.head = last
		}
	}
	json.NewEncoder(w).Encode(api.RecordsResp{Head: f.head, Next: f.next(max(f.head, req.From-1))})
}

type fixture struct {
	t    *testing.T
	st   *store.Store
	p    *emit.Pipeline
	srv  *server
	c    *syncer.Client
	auth config.Auth
	home string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := &server{cps: map[int64]string{}}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &fixture{t: t, st: st, p: emit.New(st, home), srv: srv, c: syncer.NewClient(ts.URL, "tok", "test"),
		auth: config.Auth{DeviceID: "dev1"}, home: home}
}

// synced is a sync: the server takes the checkpoint at the log's head.
func (f *fixture) synced() int64 {
	f.t.Helper()
	seq, hash, err := f.st.Head()
	if err != nil {
		f.t.Fatal(err)
	}
	f.srv.mu.Lock()
	f.srv.cps[seq] = hash
	f.srv.mu.Unlock()
	if err := f.st.SetSyncState(seq, time.Now()); err != nil {
		f.t.Fatal(err)
	}
	return seq
}

func (f *fixture) push() int {
	f.t.Helper()
	n, err := Push(context.Background(), f.st, f.auth, f.c)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) edits(dir string, at time.Time, n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		if _, err := f.p.EmitEvent(event.Event{Kind: event.Edit, Harness: "editor:vscode", CWD: dir, Files: []string{filepath.Join(dir, "main.go")},
			TS: at.Add(time.Duration(i) * time.Minute), Lines: 10}); err != nil {
			f.t.Fatal(err)
		}
	}
}

// What is sent carries no prompt text, command, file path, folder or local
// folder name: every record is in its private form or a bare fingerprint,
// every project's records go up, and the whole replays to the checkpoint.
func TestRecordsCarryNoPathsCommandsOrFolderNames(t *testing.T) {
	f := setup(t)
	proj := filepath.Join(f.home, "code", "FOLDER-CANARY")
	other := filepath.Join(f.home, "code", "OTHER-CANARY")
	for _, d := range []string{proj, other} {
		if err := os.MkdirAll(filepath.Join(d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(proj, "FILE-CANARY.go"), []byte("package x\n"), 0o644)
	at := time.Now().Add(-20 * time.Minute)
	for i, e := range []event.Event{
		{Kind: event.Prompt, Harness: "claude-code", SessionID: "s1", CWD: proj, Text: "PROMPT-CANARY make the pricing page responsive"},
		{Kind: event.AgentStart, Harness: "claude-code", SessionID: "s1", CWD: proj},
		{Kind: event.Tool, Harness: "claude-code", SessionID: "s1", CWD: proj, Tool: "Bash", Command: "cat COMMAND-CANARY.txt"},
		{Kind: event.Run, Harness: "claude-code", SessionID: "s1", CWD: proj, Command: "go test ./COMMAND-CANARY/...", RunKind: event.RunTest, ExitCode: new(int)},
		{Kind: event.Tool, Harness: "claude-code", SessionID: "s1", CWD: proj, Tool: "Edit", Write: true, Files: []string{filepath.Join(proj, "FILE-CANARY.go")}},
		{Kind: event.AgentStop, Harness: "claude-code", SessionID: "s1", CWD: proj},
		{Kind: event.Edit, Harness: "editor:vscode", CWD: proj, Files: []string{filepath.Join(proj, "FILE-CANARY.go")}, Extra: map[string]any{"editor_project": "EXTRA-CANARY"}, Lines: 90, LineNo: 12, CursorPos: 340},
		{Kind: event.Edit, Harness: "editor:vscode", CWD: other, Files: []string{filepath.Join(other, "main.go")}, Lines: 5},
		{Kind: event.Idle, Harness: "seatime", WindowMS: 600000, IdleSrc: "ioreg"},
	} {
		e.TS = at.Add(time.Duration(i) * 40 * time.Second)
		if _, err := f.p.EmitEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	// a record of no project, and one from before the private form
	if _, err := f.st.Append(event.Event{Kind: event.SessionStart, Harness: "claude-code", SessionID: "s9", CWD: "/NOPROJECT-CANARY", TS: at.Add(9 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Append(event.Event{Kind: event.Prompt, Harness: "claude-code", Project: "local:OLD-CANARY", Text: "sha256:" + strings.Repeat("ab", 32),
		CWD: "/OLDFORM-CANARY", Canon: 1, TS: at.Add(10 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	head := f.synced()
	if n := f.push(); n != int(head) {
		t.Fatalf("sent %d of %d records", n, head)
	}
	raw, _ := json.Marshal(f.srv.reqs)
	for _, leak := range []string{f.home, "pricing"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the records carry %q", leak)
		}
	}
	// the one name that goes: an editor heartbeat's file inside its repo
	if got := strings.Count(string(raw), "CANARY"); got != 1 || !strings.Contains(string(raw), `"path":"FILE-CANARY.go"`) {
		t.Errorf("CANARY appears %d times in what was sent; only the repo file's name may", got)
	}
	full, idle, projects := 0, 0, map[string]bool{}
	for _, en := range f.srv.stored {
		e := en.Event
		if e == nil {
			if en.LeafHash == "" {
				t.Errorf("record %d: neither a record nor a fingerprint", en.Seq)
			}
			continue
		}
		full++
		if !chain.Private(*e) {
			t.Errorf("record %d is not in its private form: %+v", en.Seq, e)
		}
		if e.Kind == event.Idle {
			idle++
		} else {
			projects[e.Project] = true
		}
	}
	if full != 9 || idle != 1 || len(projects) != 2 || len(f.srv.stored) != int(head) {
		t.Fatalf("%d records in full (idle %d) of %d sent, projects %v", full, idle, len(f.srv.stored), projects)
	}
	for k := range projects {
		if !strings.HasPrefix(k, "local:") || strings.Contains(k, "CANARY") {
			t.Errorf("a project goes by %q", k)
		}
	}
}

// After each sync the records since the last one go up, once. A server
// that holds more or less than this device thought says so, and the
// device goes on from there.
func TestPushGoesOnFromTheServersHead(t *testing.T) {
	f := setup(t)
	dir := filepath.Join(f.home, "code", "app")
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	t0 := time.Now().Add(-time.Hour)
	if n := f.push(); n != 0 || len(f.srv.reqs) != 0 {
		t.Fatalf("sent %d before any sync", n)
	}
	f.edits(dir, t0, 3)
	f.synced()
	if n := f.push(); n != 3 || !Sent(f.st) {
		t.Fatalf("first push sent %d", n)
	}
	if n := f.push(); n != 0 {
		t.Fatalf("sent again: %d", n)
	}
	asked := len(f.srv.reqs)
	f.edits(dir, t0.Add(10*time.Minute), 2)
	// not synced yet: nothing ends at a checkpoint, so nothing is sent
	if n := f.push(); n != 0 || len(f.srv.reqs) != asked {
		t.Fatalf("sent %d records the server has no checkpoint for", n)
	}
	f.synced()
	if n := f.push(); n != 2 || len(f.srv.reqs) != asked+1 || f.srv.reqs[asked].Entries[0].Seq != 4 {
		t.Fatalf("second push sent %d", n)
	}
	// this device forgets how far the server is (a store put back from a
	// copy): it asks, and sends only what is missing
	f.edits(dir, t0.Add(20*time.Minute), 2)
	f.synced()
	if err := f.st.SetMeta(MetaHead, ""); err != nil {
		t.Fatal(err)
	}
	if n := f.push(); n != 2 || f.srv.head != 7 || len(f.srv.stored) != 7 {
		t.Fatalf("after forgetting the server's head: sent %d, server holds %d", n, len(f.srv.stored))
	}
	// the device thinks the server holds less than it does: what it sends
	// again is kept once
	f.edits(dir, t0.Add(30*time.Minute), 1)
	f.synced()
	f.st.SetMeta(MetaHead, "3")
	if n := f.push(); n == 0 || len(f.srv.stored) != 8 {
		t.Fatalf("after a stale head: sent %d, server holds %d", n, len(f.srv.stored))
	}
	if v, _ := f.st.GetMeta(MetaHead); v != "8" {
		t.Fatalf("the server's head on record: %q", v)
	}
}

// The first upload of a device that sent segments before starts at the
// first record no segment covered, and at the last heartbeat before it.
func TestFirstPushStartsAfterUploadedSegments(t *testing.T) {
	f := setup(t)
	a, b := filepath.Join(f.home, "code", "a"), filepath.Join(f.home, "code", "b")
	for _, d := range []string{a, b} {
		os.MkdirAll(filepath.Join(d, ".git"), 0o755)
	}
	t0 := time.Now().Add(-10 * time.Hour).Truncate(time.Second)
	f.edits(a, t0, 4)                  // 1..4: a's stretch, uploaded as a segment
	f.edits(b, t0.Add(time.Hour), 3)   // 5..7: b's, uploaded too
	f.edits(a, t0.Add(8*time.Hour), 2) // 8..9: a's new work
	keys, _ := f.st.ProjectKeys()
	for k := range keys {
		until := t0.Add(2 * time.Hour)
		f.st.SetMeta(segment.MetaUntil(k), until.Format(time.RFC3339))
	}
	f.synced()
	if n := f.push(); n != 3 {
		t.Fatalf("sent %d records, want 3 (the last heartbeat before the new work, and the new work)", n)
	}
	if first := f.srv.stored[0].Seq; first != 7 {
		t.Fatalf("the upload starts at record %d, want 7", first)
	}
}

// An older server does not take records.
func TestPushToAnOlderServer(t *testing.T) {
	f := setup(t)
	dir := filepath.Join(f.home, "code", "app")
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	f.edits(dir, time.Now().Add(-time.Hour), 2)
	f.synced()
	f.srv.gone = true
	if _, err := Push(context.Background(), f.st, f.auth, f.c); err != ErrUnsupported || Sent(f.st) {
		t.Fatalf("older server: %v", err)
	}
	f.srv.gone = false
	if n := f.push(); n != 2 || !Sent(f.st) {
		t.Fatalf("once it takes records: sent %d", n)
	}
}

// The log was lost and started again from a checkpoint (store.SetBase),
// with many records never sent before that. The upload goes on from the
// first record the new log holds, wherever the server's checkpoints lie.
func TestPushGoesOnAfterTheLogWasLost(t *testing.T) {
	f := setup(t)
	dir := filepath.Join(f.home, "code", "app")
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	// the server holds records up to 3 and checkpoints the lost log made later
	f.srv.head = 3
	for _, seq := range []int64{100, 2000, 5900} {
		f.srv.cps[seq] = strings.Repeat("ab", 32)
	}
	if err := f.st.SetMeta(MetaHead, "3"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetBase(6000, strings.Repeat("cd", 32)); err != nil {
		t.Fatal(err)
	}
	f.edits(dir, time.Now().Add(-10*time.Minute), 3)
	if head := f.synced(); head != 6003 {
		t.Fatalf("head %d", head)
	}
	if n := f.push(); n != 3 || f.srv.head != 6003 || len(f.srv.stored) != 3 || f.srv.stored[0].Seq != 6001 {
		t.Fatalf("after the log was lost: sent %d, the server holds %d up to %d", n, len(f.srv.stored), f.srv.head)
	}
	if n := f.push(); n != 0 {
		t.Fatalf("sent again: %d", n)
	}
}
