// Package chain links events into a tamper-evident SHA-256 hash chain.
//
//	leaf_n = SHA256(canonical(event_n))
//	h_0    = 32 zero bytes
//	h_n    = SHA256(h_{n-1} || leaf_n)
//
// Chaining leaf hashes (not raw events) lets a segment reveal only the
// submitted project's events in full and give bare leaf hashes for everything
// else in the range, while still being verifiable against the checkpoints the
// server received in real time.
//
// canonical(e) is encoding/json of Hide(e) with TS in UTC at ms precision;
// deterministic for this struct. Free-text fields (Text, Command) enter the
// leaf only as commitments ("sha256:<hex>"), and for records in the private
// form (event.CanonPrivate) so do the folder, file paths and extras, with
// the project as its server key. A bundle may therefore reveal a record in
// its private form (Hide) without changing any hash.
//
// For records in the keyed form (event.CanonKeyed) the commitment is
// HMAC-SHA-256 under the device's own random key (HideKey, LeafKey), so a
// revealed "yes" cannot be found by hashing a word list. The "sha256:"
// marker stays: it says "a commitment, 64 hex digits", not how it was made.
package chain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"seatime/pkg/event"
)

const Genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// CanonicalKey is canonical(e) from the package comment. key is the
// device's commitment key (see HideKey), needed while e holds raw text.
func CanonicalKey(e event.Event, key []byte) ([]byte, error) {
	e.TS = e.TS.UTC().Truncate(1e6)
	h, err := hideKey(e, key)
	if err != nil {
		return nil, err
	}
	return json.Marshal(h)
}

// ErrNeedsKey: a keyed record still holds raw text and no key was given.
var ErrNeedsKey = errors.New("chain: a keyed record holds raw text; its device key is needed to commit to it")

// CommitPrefix marks a free-text field that has been replaced by its commitment.
const CommitPrefix = "sha256:"

// Commit returns the commitment for a free-text field value. It is idempotent:
// an already-committed value (or "") is returned unchanged.
func Commit(s string) string {
	if s == "" || IsCommitted(s) {
		return s
	}
	h := sha256.Sum256([]byte(s))
	return CommitPrefix + hex.EncodeToString(h[:])
}

// CommitKey is Commit keyed with HMAC-SHA-256 (records in the keyed form).
func CommitKey(s string, key []byte) string {
	if s == "" || IsCommitted(s) {
		return s
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("seatime commit\x00"))
	m.Write([]byte(s))
	return CommitPrefix + hex.EncodeToString(m.Sum(nil))
}

// IsCommitted reports whether a field holds a commitment rather than raw text.
func IsCommitted(s string) bool {
	return len(s) == len(CommitPrefix)+64 && s[:len(CommitPrefix)] == CommitPrefix
}

// Hide returns the record's private form: Text and Command replaced by
// their commitments and, for a record in the private chain form
// (event.CanonPrivate), also the folder, the file paths and the extras, with
// the project named by its server key. Leaf(Hide(e)) == Leaf(e), so a
// bundle can reveal Hide(e) and still replay against the chain.
//
// Hide needs no key, so it is for records that are already private (what a
// server receives) and for older forms. A keyed record that still holds raw
// text comes back with that text dropped to "": its leaf will not match, so
// the mistake shows, and nothing guessable is produced. The client, which
// holds the key, uses HideKey.
func Hide(e event.Event) event.Event {
	h, err := hideKey(e, nil)
	if err != nil {
		h, _ = hideKey(dropRaw(e), nil)
	}
	return h
}

// HideKey is Hide for the client, with the device's commitment key.
func HideKey(e event.Event, key []byte) (event.Event, error) { return hideKey(e, key) }

func hideKey(e event.Event, key []byte) (event.Event, error) {
	commit := Commit
	if e.Canon >= event.CanonKeyed {
		if len(key) == 0 {
			if needsKey(e) {
				return e, ErrNeedsKey
			}
			commit = func(s string) string { return s } // nothing raw left to commit
		} else {
			commit = func(s string) string { return CommitKey(s, key) }
		}
	}
	e.Text = commit(e.Text)
	e.Command = commit(e.Command)
	if e.Canon >= event.CanonPrivate {
		e.CWD = commit(e.CWD)
		if len(e.Files) > 0 && !(len(e.Files) == 1 && IsCommitted(e.Files[0])) {
			e.Files = []string{commit(strings.Join(e.Files, "\n"))}
		}
		if len(e.Extra) > 0 && !committedExtra(e.Extra) {
			b, _ := json.Marshal(e.Extra)
			e.Extra = map[string]any{"commit": commit(string(b))}
		}
		e.Project = event.ServerKey(e.Project)
	}
	return e, nil
}

// needsKey reports whether a keyed record still holds a raw field.
func needsKey(e event.Event) bool {
	raw := func(s string) bool { return s != "" && !IsCommitted(s) }
	return raw(e.Text) || raw(e.Command) || raw(e.CWD) ||
		len(e.Files) > 0 && !(len(e.Files) == 1 && IsCommitted(e.Files[0])) ||
		len(e.Extra) > 0 && !committedExtra(e.Extra)
}

func dropRaw(e event.Event) event.Event {
	drop := func(s string) string {
		if IsCommitted(s) {
			return s
		}
		return ""
	}
	e.Text, e.Command, e.CWD = drop(e.Text), drop(e.Command), drop(e.CWD)
	if len(e.Files) > 0 && !(len(e.Files) == 1 && IsCommitted(e.Files[0])) {
		e.Files = nil
	}
	if len(e.Extra) > 0 && !committedExtra(e.Extra) {
		e.Extra = nil
	}
	return e
}

func committedExtra(x map[string]any) bool {
	if len(x) != 1 {
		return false
	}
	s, ok := x["commit"].(string)
	return ok && IsCommitted(s)
}

// Private reports whether e is already in its private form: nothing in it
// but what Hide leaves.
func Private(e event.Event) bool {
	if e.Canon < event.CanonPrivate {
		return false
	}
	h := Hide(e)
	return h.Text == e.Text && h.Command == e.Command && h.CWD == e.CWD && h.Project == e.Project &&
		(len(e.Files) == 0 || len(e.Files) == 1 && h.Files[0] == e.Files[0]) && (len(e.Extra) == 0 || committedExtra(e.Extra))
}

// Leaf returns hex SHA256(canonical(e)).
func Leaf(e event.Event) (string, error) { return LeafKey(e, nil) }

// LeafKey is Leaf for a record that may still hold raw text (the client's
// own log), with the device's commitment key.
func LeafKey(e event.Event, key []byte) (string, error) {
	b, err := CanonicalKey(e, key)
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

// Link returns h_n from h_{n-1} and leaf_n (both hex).
func Link(prevHex, leafHex string) (string, error) {
	prev, err := hex.DecodeString(prevHex)
	if err != nil || len(prev) != sha256.Size {
		return "", fmt.Errorf("chain: bad prev hash %q", prevHex)
	}
	leaf, err := hex.DecodeString(leafHex)
	if err != nil || len(leaf) != sha256.Size {
		return "", fmt.Errorf("chain: bad leaf hash %q", leafHex)
	}
	h := sha256.New()
	h.Write(prev)
	h.Write(leaf)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Next returns h_n given h_{n-1} (hex) and event n.
func Next(prevHex string, e event.Event) (string, error) {
	l, err := Leaf(e)
	if err != nil {
		return "", err
	}
	return Link(prevHex, l)
}

// Checkpoint is a commitment to the chain head at a sequence number.
type Checkpoint struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

// Entry is one position in a revealed chain segment: either a full Event
// (revealed) or just its LeafHash (hidden).
type Entry struct {
	Seq      int64        `json:"seq"`
	Event    *event.Event `json:"event,omitempty"`
	LeafHash string       `json:"leaf,omitempty"`
	// Path names the file of a revealed editor heartbeat by its path inside
	// the project's git repo ("src/app.go"), so a file reads by its name.
	// It is a label beside the record, not part of it: the leaf commits to
	// the file only through Files, so Verify ignores Path.
	Path string `json:"path,omitempty"`
}

// Verify replays a contiguous segment starting right after startHash and checks
// every checkpoint whose Seq falls in range. Returns the final head hash.
func Verify(startHash string, entries []Entry, checkpoints []Checkpoint) (string, error) {
	want := make(map[int64]string, len(checkpoints))
	for _, c := range checkpoints {
		want[c.Seq] = c.Hash
	}
	h := startHash
	for i, en := range entries {
		if i > 0 && en.Seq != entries[i-1].Seq+1 {
			return "", fmt.Errorf("chain: gap between seq %d and %d", entries[i-1].Seq, en.Seq)
		}
		leaf := en.LeafHash
		if en.Event != nil {
			if en.Event.Seq != en.Seq {
				return "", fmt.Errorf("chain: entry seq %d != event seq %d", en.Seq, en.Event.Seq)
			}
			var err error
			if leaf, err = Leaf(*en.Event); err != nil {
				return "", err
			}
		}
		var err error
		if h, err = Link(h, leaf); err != nil {
			return "", err
		}
		if w, ok := want[en.Seq]; ok && w != h {
			return "", fmt.Errorf("chain: checkpoint mismatch at seq %d", en.Seq)
		}
	}
	return h, nil
}
