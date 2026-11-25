package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"seatime/pkg/chain"
	"seatime/pkg/event"
)

// The commitment key is made once per folder, even when many hooks open a
// fresh log at once, and survives reopening.
func TestCommitKeyMadeOnce(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "seatime.db")
	keys := make([][]byte, 8)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k, err := loadKey(dir)
			if err != nil {
				t.Error(err)
			}
			keys[i] = k
		}(i)
	}
	wg.Wait()
	for _, k := range keys[1:] {
		if !bytes.Equal(k, keys[0]) || len(k) != keyLen {
			t.Fatal("concurrent opens made different keys")
		}
	}
	st, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(st.key, keys[0]) {
		t.Fatal("Open did not use the folder's key")
	}
	st.Close()
	if fi, err := os.Stat(filepath.Join(dir, KeyFile)); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".commit-key-*"))
	if len(matches) != 0 {
		t.Fatalf("temporary key files left behind: %v", matches)
	}
}

// Records are written in the keyed form: the chain verifies, Forget keeps it
// verifying, and neither the fingerprint nor the commitment of a prompt is
// its plain SHA-256.
func TestKeyedCommitments(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "seatime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e, err := st.Append(event.Event{Kind: event.Prompt, SessionID: "s", Project: "github.com/u/r", Text: "yes", TextHash: st.TextHash("Yes")})
	if err != nil {
		t.Fatal(err)
	}
	if e.Canon != event.CanonKeyed {
		t.Fatalf("canon %d", e.Canon)
	}
	plain := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	h, err := st.Hide(e)
	if err != nil {
		t.Fatal(err)
	}
	if e.TextHash == plain("yes") || h.Text == chain.Commit("yes") || !chain.IsCommitted(h.Text) {
		t.Fatalf("unkeyed: text_hash %s, commitment %s", e.TextHash, h.Text)
	}
	if st.TextHash("yes") != st.TextHash("  YES ") {
		t.Fatal("fingerprint does not normalise")
	}
	// without the key the raw record cannot be committed to
	if _, err := chain.Leaf(e); err == nil {
		t.Fatal("unkeyed leaf of a keyed record with raw text")
	}
	if strings.Contains(chain.Hide(e).Text, "yes") {
		t.Fatal("Hide without the key kept the text")
	}
	// the revealed form replays against the stored leaf without the key
	l1, _ := chain.LeafKey(e, st.key)
	l2, err := chain.Leaf(h)
	if err != nil || l1 != l2 {
		t.Fatalf("revealed leaf %s != stored %s (%v)", l2, l1, err)
	}
	if n, err := st.Forget(""); err != nil || n != 1 {
		t.Fatalf("forget: %d %v", n, err)
	}
	if _, err := st.VerifyChain(); err != nil {
		t.Fatal(err)
	}
}
