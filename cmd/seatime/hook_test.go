package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"seatime/internal/client/config"
	"seatime/internal/client/daemon"
	"seatime/internal/client/emit"
	"seatime/internal/client/spool"
	"seatime/internal/client/store"
)

const promptHook = `{"session_id":"s1","cwd":"/work/app","hook_event_name":"UserPromptSubmit","prompt":"fix the failing login test"}`

func hookEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("SEATIME_HOME", home)
	return home
}

func stdinFile(t *testing.T, s string) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stdin.json")
	os.WriteFile(p, []byte(s), 0o600)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func countEvents(t *testing.T) int64 {
	t.Helper()
	st, err := store.Open(config.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n, _ := st.Count()
	return n
}

func TestHookStoresEventsNormally(t *testing.T) {
	hookEnv(t)
	var out bytes.Buffer
	runHookWith([]string{"claude-code", "UserPromptSubmit"}, stdinFile(t, promptHook), &out, time.Second, openPipeline, func() { t.Error("exit must not fire") })
	if n := countEvents(t); n != 2 { // prompt + agent_start
		t.Fatalf("stored %d events", n)
	}
	if n, _ := spool.Count(config.SpoolDir()); n != 0 {
		t.Fatal("nothing should be spooled")
	}
}

// A broken database must never fail the hook: events go to the spool.
func TestHookSpoolsWhenDatabaseUnavailable(t *testing.T) {
	hookEnv(t)
	broken := func() (*store.Store, *emit.Pipeline, error) { return nil, nil, errors.New("file is not a database") }
	runHookWith([]string{"claude-code", "UserPromptSubmit"}, stdinFile(t, promptHook), &bytes.Buffer{}, time.Second, broken, func() { t.Error("exit must not fire") })
	if n, _ := spool.Count(config.SpoolDir()); n != 1 {
		t.Fatalf("spool files: %d", n)
	}
	// the daemon stores them later, once
	st, p, err := openPipeline()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := &daemon.Daemon{Store: st, Pipeline: p}
	if n, err := d.DrainSpool(config.SpoolDir()); err != nil || n != 2 {
		t.Fatalf("drained %d: %v", n, err)
	}
	if c, _ := st.Count(); c != 2 {
		t.Fatalf("stored %d", c)
	}
}

// A database locked by another process (or a hung disk) must not hold the
// harness past the budget: the hook exits on time and spools what is left.
func TestHookBudgetWithLockedDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for a deadline")
	}
	hookEnv(t)
	st, err := store.Open(config.DBPath()) // create the schema first
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	locker, err := sql.Open("sqlite", config.DBPath()+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	tx, err := locker.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO meta(k,v) VALUES('lock','1')"); err != nil {
		t.Fatal(err)
	}
	var exited atomic.Bool
	exitedAt := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		// release the lock a little after the deadline so the stuck append finishes
		for !exited.Load() {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		tx.Rollback()
	}()
	runHookWith([]string{"claude-code", "UserPromptSubmit"}, stdinFile(t, promptHook), &bytes.Buffer{}, 300*time.Millisecond, openPipeline,
		func() { exitedAt <- time.Since(start); exited.Store(true) })
	select {
	case d := <-exitedAt:
		if d > time.Second {
			t.Fatalf("hook held the harness for %s", d)
		}
	default:
		t.Fatal("the watchdog never fired")
	}
	if n, _ := spool.Count(config.SpoolDir()); n != 1 {
		t.Fatalf("spool files: %d", n)
	}
	// whatever the stuck append managed to commit, draining never duplicates
	st, p, err := openPipeline()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := (&daemon.Daemon{Store: st, Pipeline: p}).DrainSpool(config.SpoolDir()); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.Count(); c != 2 {
		t.Fatalf("want exactly prompt + agent_start, got %d events", c)
	}
}

func TestHookUnknownAdapterAndGarbageNeverFail(t *testing.T) {
	hookEnv(t)
	var out bytes.Buffer
	runHookWith([]string{"nope"}, stdinFile(t, "x"), &out, time.Second, openPipeline, func() { t.Error("exit") })
	runHookWith(nil, stdinFile(t, ""), &out, time.Second, openPipeline, func() { t.Error("exit") })
	runHookWith([]string{"claude-code", "UserPromptSubmit"}, stdinFile(t, "{not json"), &out, time.Second, openPipeline, func() { t.Error("exit") })
	if out.Len() != 0 {
		t.Fatalf("printed %q", out.String())
	}
}

// The real hook reads the project's facts at a prompt (its name, file
// names, README) and keeps them with the turn, for the prompt check.
func TestHookKeepsProjectFacts(t *testing.T) {
	home := hookEnv(t)
	t.Setenv("SEATIME_FAKE_HOME", home)
	proj := filepath.Join(home, "code", "zorblax")
	os.MkdirAll(filepath.Join(proj, "src"), 0o755)
	os.WriteFile(filepath.Join(proj, "README.md"), []byte("# Zorblax\nCounts things.\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "src", "main.py"), []byte("print(1)\n"), 0o644)
	in, _ := json.Marshal(map[string]string{"session_id": "s1", "cwd": proj, "hook_event_name": "UserPromptSubmit", "prompt": "make the counter count down as well"})
	start := time.Now()
	runHookWith([]string{"claude-code", "UserPromptSubmit"}, stdinFile(t, string(in)), &bytes.Buffer{}, 2*time.Second, openHookPipeline, func() { t.Error("the hook ran out of time") })
	if d := time.Since(start); d > time.Second {
		t.Errorf("the hook took %v", d)
	}
	st, err := store.Open(config.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	turn, err := st.LastTurn("s1")
	if err != nil || turn == nil || turn.Info == nil {
		t.Fatalf("no turn with project facts: %+v %v", turn, err)
	}
	if p := turn.Info; p.Name != "zorblax" || p.Files != "README.md\nsrc/main.py" || p.Total != 2 || p.Readme != "# Zorblax\nCounts things." {
		t.Fatalf("project facts %+v", p)
	}
}
