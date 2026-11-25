package store

import (
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"seatime/pkg/chain"
	"seatime/pkg/event"
)

func fill(t *testing.T, path string, n int) (anchor chain.Checkpoint) {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		e, err := st.Append(event.Event{Kind: event.Prompt, Harness: "claude-code", SessionID: "s", Text: strings.Repeat("x", 200), TS: time.Unix(1795700000+int64(i), 0)})
		if err != nil {
			t.Fatal(err)
		}
		if e.Seq == int64(n/2) {
			h, _ := st.HashAt(e.Seq)
			anchor = chain.Checkpoint{Seq: e.Seq, Hash: h}
		}
	}
	if err := WriteAnchor(path, anchor, time.Now()); err != nil {
		t.Fatal(err)
	}
	st.Close()
	return anchor
}

func checkUsable(t *testing.T, st *Store) {
	t.Helper()
	if _, err := st.Append(event.Event{Kind: event.Edit, Harness: "editor:vscode", TS: time.Now()}); err != nil {
		t.Fatalf("append after recovery: %v", err)
	}
	var dmg *DamagedError
	if _, err := st.VerifyChain(); err != nil && !errors.As(err, &dmg) {
		t.Fatalf("chain after recovery: %v", err)
	}
	if _, err := st.Events(Query{}); err != nil {
		t.Fatalf("reading the log after damage: %v", err)
	}
}

func TestRecoverContinuesChainFromAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seatime.db")
	a := fill(t, path, 40)
	os.WriteFile(path, make([]byte, 8192), 0o600) // zeroed
	st, rec, err := OpenChecked(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if rec.Quarantined == "" || rec.Base != a.Seq {
		t.Fatalf("%+v", rec)
	}
	if _, err := os.Stat(rec.Quarantined); err != nil {
		t.Fatal("the damaged file must be kept, not deleted")
	}
	evs, _ := st.Events(Query{})
	if len(evs) != 1 || evs[0].Kind != event.ChainRestart || evs[0].Seq != a.Seq+1 || evs[0].PrevSeq != a.Seq || !strings.Contains(evs[0].Note, "damaged") {
		t.Fatalf("%+v", evs)
	}
	// the new chain links from the anchor hash, so the server's checkpoints still hold
	leaf, _ := chain.Leaf(evs[0])
	want, _ := chain.Link(a.Hash, leaf)
	if h, _ := st.HashAt(evs[0].Seq); h != want {
		t.Fatal("restart record does not link from the anchor")
	}
	if ss, _ := st.SyncState(); ss.Seq != a.Seq {
		t.Fatalf("sync state %+v", ss)
	}
	checkUsable(t, st)
	// a healthy file is left alone
	st.Close()
	st2, rec2, err := OpenChecked(path)
	if err != nil || rec2.Quarantined != "" {
		t.Fatal(rec2, err)
	}
	st2.Close()
}

// Truncate or scribble over the file at random offsets, 100 times: the
// client always comes back with a usable log, and the damaged copy is kept.
func TestRandomDamageAlwaysRecovers(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	dir := t.TempDir()
	recovered := 0
	for i := 0; i < 100; i++ {
		path := filepath.Join(dir, "g"+string(rune('a'+i%26))+string(rune('a'+i/26))+".db")
		fill(t, path, 60)
		b, _ := os.ReadFile(path)
		switch i % 3 {
		case 0:
			b = b[:rng.Intn(len(b))]
		case 1:
			off := rng.Intn(len(b))
			for j := 0; j < 512 && off+j < len(b); j++ {
				b[off+j] = byte(rng.Intn(256))
			}
		case 2:
			b = append(b[:rng.Intn(len(b))], make([]byte, rng.Intn(4096))...)
		}
		os.WriteFile(path, b, 0o600)
		st, rec, err := OpenChecked(path)
		if err != nil {
			t.Fatalf("damage %d: %v", i, err)
		}
		if rec.Quarantined != "" {
			recovered++
		}
		checkUsable(t, st)
		st.Close()
	}
	if recovered == 0 {
		t.Fatal("no damage was detected in 100 tries")
	}
	t.Logf("recovered %d of 100 damaged logs (the rest were still valid)", recovered)
}

func TestRecoverWithoutAnchorStartsAtGenesis(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seatime.db")
	os.WriteFile(path, []byte("this is not a database at all, just text"), 0o600)
	st, rec, err := OpenChecked(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if rec.Quarantined == "" || rec.Base != 0 {
		t.Fatalf("%+v", rec)
	}
	if seq, _, _ := st.Head(); seq != 1 {
		t.Fatalf("head %d", seq)
	}
	checkUsable(t, st)
}

func TestAnchorOnlyMovesForward(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seatime.db")
	WriteAnchor(path, chain.Checkpoint{Seq: 10, Hash: strings.Repeat("a", 64)}, time.Now())
	WriteAnchor(path, chain.Checkpoint{Seq: 4, Hash: strings.Repeat("b", 64)}, time.Now())
	if a, ok := ReadAnchor(path); !ok || a.Seq != 10 {
		t.Fatalf("%+v", a)
	}
	os.WriteFile(AnchorPath(path), []byte("{garbage"), 0o600)
	if _, ok := ReadAnchor(path); ok {
		t.Fatal("a garbled anchor must read as missing")
	}
}
