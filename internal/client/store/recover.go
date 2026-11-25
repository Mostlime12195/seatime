package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"seatime/pkg/chain"
	"seatime/pkg/event"
)

// Anchor is the last checkpoint sent to the server, kept in a small file
// next to the database (written before each sync request). If the database
// is ever lost, a new log continues the chain from here, so the server sees
// the sequence keep rising and rejects nothing after the break; only the
// events since this checkpoint (the unsynced tail) are lost.
type Anchor struct {
	Seq  int64     `json:"seq"`
	Hash string    `json:"hash"`
	At   time.Time `json:"at"`
}

// AnchorPath is the anchor file for a database path.
func AnchorPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "chain-anchor.json")
}

// WriteAnchor records cp as sent (atomically; it only moves forward).
func WriteAnchor(dbPath string, cp chain.Checkpoint, at time.Time) error {
	if old, ok := ReadAnchor(dbPath); ok && (old.Seq > cp.Seq || old.Seq == cp.Seq && old.Hash == cp.Hash) {
		return nil // only ever moves forward
	}
	b, err := json.Marshal(Anchor{Seq: cp.Seq, Hash: cp.Hash, At: at.UTC()})
	if err != nil {
		return err
	}
	p := AnchorPath(dbPath)
	f, err := os.CreateTemp(filepath.Dir(p), ".anchor-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}

// ReadAnchor returns the recorded anchor, if a valid one exists.
func ReadAnchor(dbPath string) (Anchor, bool) {
	b, err := os.ReadFile(AnchorPath(dbPath))
	if err != nil {
		return Anchor{}, false
	}
	var a Anchor
	if json.Unmarshal(b, &a) != nil || a.Seq <= 0 || len(a.Hash) != 64 {
		return Anchor{}, false
	}
	return a, true
}

// IsCorrupt reports whether err means the database file itself is damaged
// (as opposed to busy, missing or unwritable).
func IsCorrupt(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	for _, k := range []string{"malformed", "not a database", "sqlite_corrupt", "sqlite_notadb", "database disk image", "file is encrypted"} {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}

// Check runs SQLite's quick integrity check.
func (s *Store) Check() error {
	rows, err := s.db.Query("PRAGMA quick_check(5)")
	if err != nil {
		return err
	}
	defer rows.Close()
	var bad []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return err
		}
		if r != "ok" {
			bad = append(bad, r)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("database disk image is malformed: %s", strings.Join(bad, "; "))
	}
	return nil
}

// Recovery describes what OpenChecked had to do.
type Recovery struct {
	Quarantined string // where the damaged file was moved ("" = nothing was wrong)
	Reason      string
	Base        int64 // seq the new chain continues from
}

// OpenChecked opens the database and checks it. A damaged file is moved
// aside (never deleted), a new log is started that continues the chain from
// the last checkpoint sent to the server, and its first record says so.
func OpenChecked(path string) (*Store, Recovery, error) {
	st, err := Open(path)
	if err == nil {
		if err = st.Check(); err == nil {
			_, _, err = st.Head()
		}
		if err == nil {
			// the links too: damage can hit a record's stored hashes without
			// SQLite noticing. A record whose content alone is unreadable is
			// tolerated (DamagedError): the chain still links through it.
			var dmg *DamagedError
			if _, verr := st.VerifyChain(); verr != nil && !errors.As(verr, &dmg) && !IsBusy(verr) {
				err = fmt.Errorf("database disk image is malformed: the hash chain does not link (%v)", verr)
			}
		}
		if err == nil {
			return st, Recovery{}, nil
		}
		st.Close()
	}
	if !IsCorrupt(err) {
		return nil, Recovery{}, err
	}
	return recoverFrom(path, err.Error())
}

func recoverFrom(path, why string) (*Store, Recovery, error) {
	rec := Recovery{Reason: why}
	stamp := time.Now().UTC().Format("20060102T150405")
	rec.Quarantined = path + ".corrupt-" + stamp
	for _, suf := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(path+suf, rec.Quarantined+suf); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, rec, fmt.Errorf("moving the damaged database aside: %w", err)
		}
	}
	st, err := Open(path)
	if err != nil {
		return nil, rec, err
	}
	a, ok := ReadAnchor(path)
	if ok {
		if err := st.SetBase(a.Seq, a.Hash); err != nil {
			st.Close()
			return nil, rec, err
		}
		rec.Base = a.Seq
		_ = st.SetSyncState(a.Seq, a.At) // the server has everything up to the anchor
	}
	note := "the local log was damaged (" + clipStr(why, 160) + ") and was moved to " + filepath.Base(rec.Quarantined) +
		"; this log continues the chain from checkpoint " + strconv.FormatInt(rec.Base, 10)
	if _, err := st.Append(event.Event{Kind: event.ChainRestart, Harness: "seatime", PrevSeq: rec.Base, Note: note}); err != nil {
		st.Close()
		return nil, rec, err
	}
	return st, rec, nil
}

// SetBase makes an empty log's chain start at (seq, hash).
func (s *Store) SetBase(seq int64, hash string) error {
	if n, err := s.Count(); err != nil {
		return err
	} else if n > 0 {
		return errors.New("store: the chain base can only be set on an empty log")
	}
	if err := s.SetMeta("chain_base_seq", strconv.FormatInt(seq, 10)); err != nil {
		return err
	}
	return s.SetMeta("chain_base_hash", hash)
}

func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// IsBusy reports a locked database (another process is writing).
func IsBusy(err error) bool { return err != nil && isBusy(err) }
