package store

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"seatime/pkg/chain"
	"seatime/pkg/event"
)

// KeyFile is the device's commitment key, next to the database. It is 32
// random bytes, made the first time a log is opened in that folder, and it
// never leaves the machine: no sync, submission, report or export reads it.
// Prompt hashes (TextHash) and the chain's commitments to text, commands
// and paths are HMAC-SHA-256 under it, so "yes" or "fix it" cannot be found
// in a submission by hashing a word list, while two prompts of the same
// person still compare equal (all rule 6 and the replay flag need).
const KeyFile = "commit.key"

const keyLen = 32

// loadKey reads dir/KeyFile, creating it if absent. Several hooks may open a
// fresh log at once: the key is written to a temporary file and hard-linked
// into place, so exactly one key wins and nobody reads a half-written one.
func loadKey(dir string) ([]byte, error) {
	path := filepath.Join(dir, KeyFile)
	if k, err := readKey(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return k, err
	}
	k := make([]byte, keyLen)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".commit-key-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(hex.EncodeToString(k) + "\n")
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return readKey(path) // ours, or the one another process linked first
}

func readKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(k) != keyLen {
		return nil, fmt.Errorf("%s is damaged: it must hold %d random bytes in hex", path, keyLen)
	}
	return k, nil
}

// TextHash is the prompt fingerprint: HMAC-SHA-256 under the device key of
// the lowercased, whitespace-collapsed text, in hex.
func (s *Store) TextHash(text string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("seatime text\x00"))
	m.Write([]byte(strings.Join(strings.Fields(strings.ToLower(text)), " ")))
	return hex.EncodeToString(m.Sum(nil))
}

// SentBefore records that a prompt, already normalised (ledger.Normalize),
// was sent in project, and reports whether it had been sent there before.
// The set holds only keyed hashes and never leaves the machine; it
// survives restarts, and `seatime forget` clears it.
func (s *Store) SentBefore(project, normalized string) (bool, error) {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("seatime sent\x00"))
	m.Write([]byte(normalized))
	res, err := s.db.Exec("INSERT OR IGNORE INTO prompts_sent(project, hash) VALUES(?, ?)", project, hex.EncodeToString(m.Sum(nil)))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 0, err
}

// Hide is chain.HideKey with this device's key: the record's private form.
func (s *Store) Hide(e event.Event) (event.Event, error) { return chain.HideKey(e, s.key) }
