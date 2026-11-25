package store

import (
	"time"
)

// LabelBlock is the width of one ground-truth label.
const LabelBlock = 5 * time.Minute

// Ground-truth labels: what the person says they were doing in each
// 5-minute block. Local only: they are not in the hash chain and never
// leave the machine.
const (
	LabelHandsOn  = "hands-on"
	LabelReading  = "reading"
	LabelWatching = "watching"
	LabelAway     = "away"
)

// LabelKeys are the four keys, in the order the day view offers them.
var LabelKeys = []string{LabelHandsOn, LabelReading, LabelWatching, LabelAway}

func validLabel(l string) bool {
	for _, k := range LabelKeys {
		if l == k {
			return true
		}
	}
	return false
}

// BlockOf is the block a moment falls in (unix seconds / 300).
func BlockOf(t time.Time) int64 { return t.Unix() / int64(LabelBlock/time.Second) }

// BlockStart is a block's first moment.
func BlockStart(b int64) time.Time { return time.Unix(b*int64(LabelBlock/time.Second), 0) }

// SetLabels stores labels by block; "" clears a block. Unknown labels are
// ignored.
func (s *Store) SetLabels(ls map[int64]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for b, l := range ls {
		switch {
		case l == "":
			_, err = tx.Exec("DELETE FROM labels WHERE block=?", b)
		case validLabel(l):
			_, err = tx.Exec("INSERT INTO labels(block,label) VALUES(?,?) ON CONFLICT(block) DO UPDATE SET label=excluded.label", b, l)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Labels returns the labels of blocks starting in [from, to).
func (s *Store) Labels(from, to time.Time) (map[int64]string, error) {
	rows, err := s.db.Query("SELECT block, label FROM labels WHERE block>=? AND block<?", BlockOf(from.Add(LabelBlock-time.Second)), BlockOf(to.Add(LabelBlock-time.Second)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var b int64
		var l string
		if err := rows.Scan(&b, &l); err != nil {
			return nil, err
		}
		out[b] = l
	}
	return out, rows.Err()
}
