package store

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"seatime/pkg/chain"
	"seatime/pkg/event"
)

func TestMain(m *testing.M) {
	// Child mode for the multi-process test: append N events then exit.
	if p := os.Getenv("SEATIME_TEST_APPEND_DB"); p != "" {
		n, _ := strconv.Atoi(os.Getenv("SEATIME_TEST_APPEND_N"))
		id := os.Getenv("SEATIME_TEST_APPEND_ID")
		st, err := Open(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for i := 0; i < n; i++ {
			if _, err := st.Append(event.Event{Kind: event.Tool, Harness: "child" + id, Tool: "t" + strconv.Itoa(i)}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		st.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "seatime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAppendChainAndQueries(t *testing.T) {
	s := open(t)
	base := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	ex := 7
	for i := 0; i < 20; i++ {
		e := event.Event{Kind: event.Prompt, Harness: "x", SessionID: "s" + strconv.Itoa(i%2), Project: "p" + strconv.Itoa(i%3),
			TS: base.Add(time.Duration(i) * time.Minute), Text: "héllo", Extra: map[string]any{"n": 3, "f": 1.5}, ExitCode: &ex}
		got, err := s.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		if got.Seq != int64(i+1) {
			t.Fatalf("seq %d want %d", got.Seq, i+1)
		}
	}
	if n, err := s.VerifyChain(); err != nil || n != 20 {
		t.Fatalf("verify: %d %v", n, err)
	}
	seq, h, _ := s.Head()
	if seq != 20 {
		t.Fatal(seq)
	}
	// Revealed-everything bundle replays and matches the head.
	rows, _ := s.Rows(1, 20)
	var entries []chain.Entry
	for _, r := range rows {
		entries = append(entries, chain.Entry{Seq: r.Seq, LeafHash: r.Leaf})
	}
	if got, err := chain.Verify(chain.Genesis, entries, []chain.Checkpoint{{Seq: 20, Hash: h}}); err != nil || got != h {
		t.Fatalf("chain.Verify: %v", err)
	}
	// time/project/session queries, seq order
	evs, _ := s.Events(Query{Since: base.Add(5 * time.Minute), Until: base.Add(10 * time.Minute)})
	if len(evs) != 5 || evs[0].Seq != 6 {
		t.Fatalf("time query: %d", len(evs))
	}
	evs, _ = s.Events(Query{Project: "p1"})
	if len(evs) != 7 {
		t.Fatalf("project query: %d", len(evs))
	}
	evs, _ = s.Events(Query{Session: "s0", Kinds: []event.Kind{event.Prompt}, Desc: true, Limit: 3})
	if len(evs) != 3 || evs[0].Seq <= evs[1].Seq {
		t.Fatalf("session query: %v", evs)
	}
	if evs[0].TS.Nanosecond()%1e6 != 0 {
		t.Fatal("ts not ms precision")
	}
	prompts, _ := s.SessionPrompts("s1", 3)
	if len(prompts) != 3 {
		t.Fatal(prompts)
	}
}

func TestSyncStateAndMeta(t *testing.T) {
	s := open(t)
	if st, _ := s.SyncState(); st.Seq != 0 || !st.Time.IsZero() {
		t.Fatal("expected empty state")
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.SetSyncState(42, now); err != nil {
		t.Fatal(err)
	}
	st, _ := s.SyncState()
	if st.Seq != 42 || !st.Time.Equal(now) {
		t.Fatalf("%+v", st)
	}
}

func TestTamperDetected(t *testing.T) {
	s := open(t)
	for i := 0; i < 3; i++ {
		s.Append(event.Event{Kind: event.Prompt, Text: "a"})
	}
	if _, err := s.db.Exec(`UPDATE events SET data=replace(data,'"a"','"b"') WHERE seq=2`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyChain(); err == nil {
		t.Fatal("tampering not detected")
	}
}

func TestConcurrentHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seatime.db")
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			s, err := Open(path)
			if err != nil {
				t.Error(err)
				return
			}
			defer s.Close()
			for i := 0; i < 30; i++ {
				if _, err := s.Append(event.Event{Kind: event.Tool, Tool: strconv.Itoa(g)}); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	s, _ := Open(path)
	defer s.Close()
	if n, err := s.VerifyChain(); err != nil || n != 180 {
		t.Fatalf("verify %d %v", n, err)
	}
}

func TestConcurrentProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seatime.db")
	var cmds []*exec.Cmd
	for i := 0; i < 5; i++ {
		c := exec.Command(os.Args[0], "-test.run=NONE")
		c.Env = append(os.Environ(), "SEATIME_TEST_APPEND_DB="+path, "SEATIME_TEST_APPEND_N=25", "SEATIME_TEST_APPEND_ID="+strconv.Itoa(i))
		c.Stderr = os.Stderr
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, c)
	}
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := Open(path)
	defer s.Close()
	if n, err := s.VerifyChain(); err != nil || n != 125 {
		t.Fatalf("verify %d %v", n, err)
	}
}

func TestForgetKeepsChain(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Append(event.Event{Kind: event.Prompt, Project: "a", Text: "my secret plan", TS: time.Now()})
	st.Append(event.Event{Kind: event.Tool, Project: "a", Command: "cat notes.txt", TS: time.Now()})
	st.Append(event.Event{Kind: event.Prompt, Project: "b", Text: "keep me", TS: time.Now()})
	if n, err := st.Forget("a"); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if _, err := st.VerifyChain(); err != nil {
		t.Fatalf("forget broke the chain: %v", err)
	}
	evs, _ := st.Events(Query{})
	if !strings.HasPrefix(evs[0].Text, "sha256:") || !strings.HasPrefix(evs[1].Command, "sha256:") || evs[2].Text != "keep me" {
		t.Fatalf("%+v", evs)
	}
	if n, _ := st.Forget(""); n != 1 {
		t.Fatal("forget all")
	}
	if n, _ := st.Forget(""); n != 0 {
		t.Fatal("idempotent")
	}
}
