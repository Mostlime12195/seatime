package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// A device sends its chain as it writes it (internal/server records.go):
// every record in its private form, or as a bare leaf hash. The server
// keeps what replayed to a checkpoint, and cuts and counts each project's
// stretches of work from it.

// Record is one position of a device's chain as the server holds it.
type Record struct {
	Seq     int64
	TS      time.Time // zero for a bare leaf
	Project string    // the record's project key; "" for a bare leaf and for a record of no project
	Kind    string
	Leaf    string
	Prev    string // the chain head before the record
	Data    string // the record in its private form (JSON); "" for a bare leaf
	Path    string // an editor heartbeat's file, by its path in the project's repo
}

// RecordStream is how far a device's records go.
type RecordStream struct {
	DeviceID string
	UserID   int64
	Head     int64 // the last seq held
	RunLo    int64 // the first seq of the unbroken run that ends at Head
	// Dirty is the earliest time among the records that came since the
	// server last counted (zero: none came).
	Dirty    time.Time
	Received time.Time
}

// RecordTime is a record's place and time.
type RecordTime struct {
	Seq int64
	TS  time.Time
}

type Records interface {
	// RecordStream is a device's stream (ErrNotFound: it sent no records).
	RecordStream(ctx context.Context, deviceID string) (*RecordStream, error)
	// AddRecords stores records that follow the device's head, or start a
	// new run after a hole (first > head+1). Records at or below the head
	// are skipped. dirty is the earliest record time among them.
	AddRecords(ctx context.Context, deviceID string, userID int64, rs []Record, dirty, now time.Time) error
	// DirtyStreams are the streams that got records since they were counted.
	DirtyStreams(ctx context.Context) ([]RecordStream, error)
	// ClearDirty marks a stream counted, unless records came since it was
	// read (its Received then).
	ClearDirty(ctx context.Context, deviceID string, received time.Time) error
	// CountClaim takes the turn to count for ttl: true for one process at
	// a time. CountDone is the mark of the last cycle that finished;
	// SetCountDone records one.
	CountClaim(ctx context.Context, now time.Time, ttl time.Duration) (bool, error)
	CountDone(ctx context.Context) (time.Time, error)
	SetCountDone(ctx context.Context, mark time.Time) error
	// RecordRows are a device's records with lo <= seq <= hi, in order.
	RecordRows(ctx context.Context, deviceID string, lo, hi int64) ([]Record, error)
	// RecordKeys are the project keys among a device's revealed records
	// from seq lo on with a time at or after since.
	RecordKeys(ctx context.Context, deviceID string, lo int64, since time.Time) ([]string, error)
	// RecordTimes are the place and time of a project key's revealed
	// records with seq >= lo and a time in [since, until) (zero until: no
	// end), by time.
	RecordTimes(ctx context.Context, deviceID, key string, lo int64, since, until time.Time) ([]RecordTime, error)
	// RecordsBetween are a device's revealed records with a time in [from,
	// to], every project's, by time; before, when not zero, adds the last
	// edit or prompt before from.
	RecordsBetween(ctx context.Context, deviceID string, from, to time.Time, before bool) ([]Record, error)
	// LastRecordSeq is the seq of the last revealed record of project key
	// or of no project with seq >= lo and a time before until (0: none).
	LastRecordSeq(ctx context.Context, deviceID, key string, lo int64, until time.Time) (int64, error)
}

func (s *DB) RecordStream(ctx context.Context, dev string) (*RecordStream, error) {
	return scanStream(s.row(ctx, `SELECT device_id,user_id,head,run_lo,dirty,received FROM record_streams WHERE device_id=?`, dev))
}

func scanStream(r scanner) (*RecordStream, error) {
	var st RecordStream
	var dirty, rec int64
	if err := r.Scan(&st.DeviceID, &st.UserID, &st.Head, &st.RunLo, &dirty, &rec); err != nil {
		return nil, nf(err)
	}
	if dirty != 0 {
		st.Dirty = time.UnixMilli(dirty).UTC()
	}
	st.Received = time.UnixMilli(rec).UTC()
	return &st, nil
}

func (s *DB) AddRecords(ctx context.Context, dev string, uid int64, rs []Record, dirty, now time.Time) error {
	if len(rs) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *txn) error {
		var head, runLo, old int64
		err := tx.QueryRowContext(ctx, `SELECT head,run_lo,dirty FROM record_streams WHERE device_id=?`, dev).Scan(&head, &runLo, &old)
		fresh := errors.Is(err, sql.ErrNoRows)
		if err != nil && !fresh {
			return err
		}
		ins, err := tx.PrepareContext(ctx, `INSERT INTO records(device_id,seq,ts,project,kind,leaf,prev,data,path) VALUES (?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		added := false
		for _, r := range rs {
			if r.Seq <= head {
				continue
			}
			if !added && (fresh || r.Seq > head+1) {
				runLo = r.Seq // the first records, or a new run after a hole
			}
			if _, err := ins.ExecContext(ctx, dev, r.Seq, msOrZero(r.TS), r.Project, r.Kind, r.Leaf, r.Prev, r.Data, r.Path); err != nil {
				return err
			}
			head, added = r.Seq, true
		}
		if !added {
			return nil
		}
		d := msOrZero(dirty)
		if d == 0 {
			d = now.UnixMilli()
		}
		if old != 0 && old < d {
			d = old
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO record_streams(device_id,user_id,head,run_lo,dirty,received) VALUES (?,?,?,?,?,?)
ON CONFLICT(device_id) DO UPDATE SET user_id=excluded.user_id, head=excluded.head, run_lo=excluded.run_lo, dirty=excluded.dirty, received=excluded.received`,
			dev, uid, head, runLo, d, now.UnixMilli())
		return err
	})
}

func (s *DB) DirtyStreams(ctx context.Context) ([]RecordStream, error) {
	rows, err := s.rows(ctx, `SELECT device_id,user_id,head,run_lo,dirty,received FROM record_streams WHERE dirty<>0 ORDER BY device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecordStream
	for rows.Next() {
		st, err := scanStream(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}

func (s *DB) ClearDirty(ctx context.Context, dev string, received time.Time) error {
	_, err := s.exec(ctx, `UPDATE record_streams SET dirty=0 WHERE device_id=? AND received=?`, dev, received.UnixMilli())
	return err
}

func (s *DB) CountClaim(ctx context.Context, now time.Time, ttl time.Duration) (bool, error) {
	res, err := s.exec(ctx, `UPDATE kv SET value=? WHERE name='count_claim' AND value<?`, float64(now.Unix()), float64(now.Add(-ttl).Unix()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *DB) CountDone(ctx context.Context) (time.Time, error) {
	var v float64
	if err := s.row(ctx, `SELECT value FROM kv WHERE name='count_done'`).Scan(&v); err != nil {
		return time.Time{}, nf(err)
	}
	return time.Unix(int64(v), 0).UTC(), nil
}

// SetCountDone also gives the turn back.
func (s *DB) SetCountDone(ctx context.Context, mark time.Time) error {
	return s.tx(ctx, func(tx *txn) error {
		if _, err := tx.ExecContext(ctx, `UPDATE kv SET value=? WHERE name='count_done'`, float64(mark.Unix())); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE kv SET value=0 WHERE name='count_claim'`)
		return err
	})
}

func (s *DB) listRecords(ctx context.Context, q string, args ...any) ([]Record, error) {
	rows, err := s.rows(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var ms int64
		if err := rows.Scan(&r.Seq, &ms, &r.Project, &r.Kind, &r.Leaf, &r.Prev, &r.Data, &r.Path); err != nil {
			return nil, err
		}
		if ms != 0 {
			r.TS = time.UnixMilli(ms).UTC()
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const recCols = `seq,ts,project,kind,leaf,prev,data,path`

func (s *DB) RecordRows(ctx context.Context, dev string, lo, hi int64) ([]Record, error) {
	return s.listRecords(ctx, `SELECT `+recCols+` FROM records WHERE device_id=? AND seq>=? AND seq<=? ORDER BY seq`, dev, lo, hi)
}

func (s *DB) RecordKeys(ctx context.Context, dev string, lo int64, since time.Time) ([]string, error) {
	rows, err := s.rows(ctx, `SELECT DISTINCT project FROM records WHERE device_id=? AND ts>=? AND seq>=? AND data<>'' AND project<>'' ORDER BY project`, dev, msOrZero(since), lo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *DB) RecordTimes(ctx context.Context, dev, key string, lo int64, since, until time.Time) ([]RecordTime, error) {
	q, args := `SELECT seq,ts FROM records WHERE device_id=? AND project=? AND data<>'' AND seq>=? AND ts>=?`, []any{dev, key, lo, msOrZero(since)}
	if !until.IsZero() {
		q += ` AND ts<?`
		args = append(args, until.UnixMilli())
	}
	rows, err := s.rows(ctx, q+` ORDER BY ts, seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecordTime
	for rows.Next() {
		var r RecordTime
		var ms int64
		if err := rows.Scan(&r.Seq, &ms); err != nil {
			return nil, err
		}
		r.TS = time.UnixMilli(ms).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *DB) RecordsBetween(ctx context.Context, dev string, from, to time.Time, before bool) ([]Record, error) {
	var out []Record
	if before {
		prev, err := s.listRecords(ctx, `SELECT `+recCols+` FROM records WHERE device_id=? AND data<>'' AND ts<? AND ts>0 AND kind IN ('edit','prompt') ORDER BY ts DESC, seq DESC LIMIT 1`, dev, from.UnixMilli())
		if err != nil {
			return nil, err
		}
		out = prev
	}
	rs, err := s.listRecords(ctx, `SELECT `+recCols+` FROM records WHERE device_id=? AND data<>'' AND ts>=? AND ts<=? ORDER BY ts, seq`, dev, from.UnixMilli(), to.UnixMilli())
	return append(out, rs...), err
}

func (s *DB) LastRecordSeq(ctx context.Context, dev, key string, lo int64, until time.Time) (int64, error) {
	q, args := `SELECT COALESCE(MAX(seq),0) FROM records WHERE device_id=? AND data<>'' AND (project=? OR project='') AND seq>=?`, []any{dev, key, lo}
	if !until.IsZero() {
		q += ` AND ts<?`
		args = append(args, until.UnixMilli())
	}
	var n int64
	err := s.row(ctx, q, args...).Scan(&n)
	return n, err
}
