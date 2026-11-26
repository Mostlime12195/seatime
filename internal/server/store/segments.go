package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"seatime/pkg/ledger"
)

// A segment is one stretch of one project's records from one device. The
// server cuts it from the records the device sends as it writes them
// (records.go; Stream), or an older device uploads it once the stretch is
// over. The server checks it against the device's hash chain and counts
// it (internal/server segments.go). Its credited minutes are what
// applications read.

const (
	SegVerified = "verified"
	SegPending  = "pending"
	SegFailed   = "failed"
)

type Segment struct {
	ID         string
	UserID     int64
	ProjectID  int64
	ProjectKey string
	DeviceID   string
	Status     string
	Problems   []string
	Claimed    ledger.Bucket
	Scored     ledger.Bucket
	SeqLo      int64
	SeqHi      int64
	ScoreVer   string
	FirstTS    time.Time // the project's first and last record in it
	LastTS     time.Time
	Pending    int // prompts that wait for their check
	Created    time.Time
	Checked    time.Time
	// Stream: the server made it from the records the device sent as it
	// wrote them. It holds the project's records with a time in [Since,
	// Until). Open: the stretch of work is not over (no Until); the server
	// counts it again at each cycle.
	Stream bool
	Open   bool
	Since  time.Time
	Until  time.Time
	// RecKey is the project key a server-made segment's records carry
	// (ProjectKey is what the project goes by now).
	RecKey string
}

// SegmentMinute is credited time in one minute of a segment.
type SegmentMinute struct {
	StartMin    int64
	Human, Dark float64
	// Hand is the part of Human from hand edits; Models the part of the
	// rest, and of Prompts, whose records name a model (ledger.Bucket).
	Hand    float64
	Prompts int
	Models  []ledger.ModelUse
}

// MinuteClaim is credited seconds in one minute by an earlier segment.
type MinuteClaim struct {
	StartMin  int64
	SegmentID string
	Human     float64
}

type Segments interface {
	// PutSegment stores a new segment, its bundle and its minutes, or
	// replaces the status, numbers and minutes of a stored one (bundleGz nil).
	PutSegment(ctx context.Context, sg *Segment, bundleGz []byte, minutes []SegmentMinute) error
	// ReplaceSegment stores a segment the server made again, counted over
	// its records as they are now: everything but its id, owner and
	// creation time, with its bundle and its minutes. wasOpen is whether
	// the caller read it as open: when it is not that any more (it was
	// closed meanwhile), nothing is stored and the answer is ErrConflict.
	ReplaceSegment(ctx context.Context, sg *Segment, wasOpen bool, bundleGz []byte, minutes []SegmentMinute) error
	// StreamSegments are the segments the server made for a device, oldest
	// first: all of them, or only those that are open or pending.
	StreamSegments(ctx context.Context, deviceID string, unsettled bool) ([]Segment, error)
	// UnsettledStreams are the devices with a server-made segment that is
	// open or pending.
	UnsettledStreams(ctx context.Context) ([]string, error)
	// CloseStreamSegments closes a device's open segments at the last
	// record the server holds of each: its records go on after a hole.
	CloseStreamSegments(ctx context.Context, deviceID string) error
	Segment(ctx context.Context, id string) (*Segment, error)
	SegmentBundle(ctx context.Context, id string) ([]byte, error)
	// ListSegments are a user's segments of one project (or all, 0), oldest first.
	ListSegments(ctx context.Context, userID, projectID int64) ([]Segment, error)
	// AllSegments are every user's segments, oldest first.
	AllSegments(ctx context.Context) ([]Segment, error)
	// CheckedUpTo is, for each of a user's projects, the end of the last
	// segment that did not fail.
	CheckedUpTo(ctx context.Context, userID int64) (map[int64]time.Time, error)
	// PendingSegments are segments devices uploaded, of any user, waiting to
	// be checked again (one the server made is counted again at its cycle).
	PendingSegments(ctx context.Context, limit int) ([]Segment, error)
	// MarkSegmentsPending marks a project's segments pending: its prompts
	// were asked for, and they are counted again once the checks are in.
	MarkSegmentsPending(ctx context.Context, userID, projectID int64) error
	// MinuteClaims are minutes in [fromMin, toMin] that segments of the user
	// created before `before` (not failed, not excludeID) already credit.
	MinuteClaims(ctx context.Context, userID int64, fromMin, toMin int64, before time.Time, excludeID string) ([]MinuteClaim, error)
	// VerifiedMinutes are a project's credited minutes in [from, to) from
	// verified segments, merged by minute.
	VerifiedMinutes(ctx context.Context, projectID int64, from, to time.Time) ([]SegmentMinute, error)
	// SegmentMinutesOf are one segment's credited minutes, in order, with
	// the hand part of each and its seconds by model.
	SegmentMinutesOf(ctx context.Context, segmentID string) ([]SegmentMinute, error)
	// VerifiedModels are those minutes' seconds, and their prompts, by model.
	VerifiedModels(ctx context.Context, projectID int64, from, to time.Time) ([]ModelSum, error)
	// SetProjectPulled records that an application read the project and its
	// prompts were asked for.
	SetProjectPulled(ctx context.Context, projectID int64) error
	// ProjectPulled reports whether that happened.
	ProjectPulled(ctx context.Context, projectID int64) (bool, error)
	// ClosedPromptCount is how many prompts of a project, under any key its
	// devices sent for it, were closed without a verdict, by status
	// (ChainMismatch, ChainMissing).
	ClosedPromptCount(ctx context.Context, projectID int64) (map[string]int, error)
}

func (s *DB) PutSegment(ctx context.Context, sg *Segment, gz []byte, minutes []SegmentMinute) error {
	pr, _ := json.Marshal(sg.Problems)
	if sg.Problems == nil {
		pr = []byte("[]")
	}
	cl, _ := json.Marshal(sg.Claimed)
	sc, _ := json.Marshal(sg.Scored)
	return s.tx(ctx, func(tx *txn) error {
		if gz != nil {
			if _, err := tx.ExecContext(ctx, `INSERT INTO segments(id,user_id,project_id,device_id,status,problems,claimed,scored,seq_lo,seq_hi,score_ver,first_ts,last_ts,pending,created,checked,stream,is_open,since_ts,until_ts,rec_key)
  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				sg.ID, sg.UserID, sg.ProjectID, sg.DeviceID, sg.Status, string(pr), string(cl), string(sc), sg.SeqLo, sg.SeqHi, sg.ScoreVer,
				sg.FirstTS.UnixMilli(), sg.LastTS.UnixMilli(), sg.Pending, sg.Created.UnixMilli(), sg.Checked.UnixMilli(),
				b2i(sg.Stream), b2i(sg.Open), msOrZero(sg.Since), msOrZero(sg.Until), sg.RecKey); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO segment_blobs(id,data) VALUES (?,?)`, sg.ID, gz); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE segments SET status=?, problems=?, scored=?, pending=?, checked=? WHERE id=?`,
				sg.Status, string(pr), string(sc), sg.Pending, sg.Checked.UnixMilli(), sg.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM segment_minutes WHERE segment_id=?`, sg.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM segment_models WHERE segment_id=?`, sg.ID); err != nil {
				return err
			}
		}
		return putMinutes(ctx, tx, sg, minutes)
	})
}

// putMinutes writes a segment's credited minutes (none were there).
func putMinutes(ctx context.Context, tx *txn, sg *Segment, minutes []SegmentMinute) error {
	if sg.Status == SegFailed {
		return nil // a failed segment credits nothing
	}
	m, err := tx.PrepareContext(ctx, `INSERT INTO segment_minutes(segment_id,user_id,project_id,start_min,human,dark) VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer m.Close()
	for _, x := range minutes {
		if x.Human <= 0 && x.Dark <= 0 {
			continue
		}
		if _, err := m.ExecContext(ctx, sg.ID, sg.UserID, sg.ProjectID, x.StartMin, x.Human, x.Dark); err != nil {
			return err
		}
	}
	// the same minutes by model: one device's row, split as a merged minute is
	mm, err := tx.PrepareContext(ctx, `INSERT INTO segment_models(segment_id,user_id,project_id,start_min,hand,model,seconds,prompts) VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer mm.Close()
	for _, x := range minutes {
		row := brow{struggle: x.Human, agent: x.Human - x.Hand, prompts: x.Prompts, models: encodeModels(x.Models)}
		for k, v := range mergeModels([]brow{row}) {
			if _, err := mm.ExecContext(ctx, sg.ID, sg.UserID, sg.ProjectID, x.StartMin, b2i(k.hand), k.model, v.sec, v.prompts); err != nil {
				return err
			}
		}
	}
	return nil
}

func msOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func (s *DB) ReplaceSegment(ctx context.Context, sg *Segment, wasOpen bool, gz []byte, minutes []SegmentMinute) error {
	pr, _ := json.Marshal(sg.Problems)
	if sg.Problems == nil {
		pr = []byte("[]")
	}
	sc, _ := json.Marshal(sg.Scored)
	return s.tx(ctx, func(tx *txn) error {
		res, err := tx.ExecContext(ctx, `UPDATE segments SET status=?, problems=?, scored=?, seq_lo=?, seq_hi=?, score_ver=?, first_ts=?, last_ts=?, pending=?, checked=?, is_open=?, since_ts=?, until_ts=? WHERE id=? AND is_open=?`,
			sg.Status, string(pr), string(sc), sg.SeqLo, sg.SeqHi, sg.ScoreVer, sg.FirstTS.UnixMilli(), sg.LastTS.UnixMilli(), sg.Pending, sg.Checked.UnixMilli(),
			b2i(sg.Open), msOrZero(sg.Since), msOrZero(sg.Until), sg.ID, b2i(wasOpen))
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `UPDATE segment_blobs SET data=? WHERE id=?`, gz, sg.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM segment_minutes WHERE segment_id=?`, sg.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM segment_models WHERE segment_id=?`, sg.ID); err != nil {
			return err
		}
		return putMinutes(ctx, tx, sg, minutes)
	})
}

func (s *DB) StreamSegments(ctx context.Context, deviceID string, unsettled bool) ([]Segment, error) {
	q := `SELECT ` + segCols + segFrom + `WHERE s.device_id=? AND s.stream=1`
	if unsettled {
		q += ` AND (s.is_open=1 OR s.status='pending')`
	}
	return s.listSegs(ctx, q+` ORDER BY s.created, s.id`, deviceID)
}

func (s *DB) UnsettledStreams(ctx context.Context) ([]string, error) {
	rows, err := s.rows(ctx, `SELECT DISTINCT device_id FROM segments WHERE stream=1 AND (is_open=1 OR status='pending') ORDER BY device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

const segCols = `s.id,s.user_id,s.project_id,p.key,s.device_id,s.status,s.problems,s.claimed,s.scored,s.seq_lo,s.seq_hi,s.score_ver,s.first_ts,s.last_ts,s.pending,s.created,s.checked,s.stream,s.is_open,s.since_ts,s.until_ts,s.rec_key`
const segFrom = ` FROM segments s JOIN projects p ON p.id=s.project_id `

func scanSeg(r scanner) (*Segment, error) {
	var sg Segment
	var pr, cl, sc string
	var ft, lt, cr, ck, since, until int64
	var stream, open int
	if err := r.Scan(&sg.ID, &sg.UserID, &sg.ProjectID, &sg.ProjectKey, &sg.DeviceID, &sg.Status, &pr, &cl, &sc, &sg.SeqLo, &sg.SeqHi, &sg.ScoreVer,
		&ft, &lt, &sg.Pending, &cr, &ck, &stream, &open, &since, &until, &sg.RecKey); err != nil {
		return nil, nf(err)
	}
	sg.Stream, sg.Open = stream != 0, open != 0
	if since != 0 {
		sg.Since = time.UnixMilli(since).UTC()
	}
	if until != 0 {
		sg.Until = time.UnixMilli(until).UTC()
	}
	json.Unmarshal([]byte(pr), &sg.Problems)
	json.Unmarshal([]byte(cl), &sg.Claimed)
	json.Unmarshal([]byte(sc), &sg.Scored)
	sg.FirstTS, sg.LastTS = time.UnixMilli(ft).UTC(), time.UnixMilli(lt).UTC()
	sg.Created, sg.Checked = time.UnixMilli(cr).UTC(), time.UnixMilli(ck).UTC()
	return &sg, nil
}

func (s *DB) Segment(ctx context.Context, id string) (*Segment, error) {
	return scanSeg(s.row(ctx, `SELECT `+segCols+segFrom+`WHERE s.id=?`, id))
}

func (s *DB) SegmentBundle(ctx context.Context, id string) ([]byte, error) {
	var b []byte
	err := s.row(ctx, `SELECT data FROM segment_blobs WHERE id=?`, id).Scan(&b)
	return b, nf(err)
}

func (s *DB) listSegs(ctx context.Context, q string, args ...any) ([]Segment, error) {
	rows, err := s.rows(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Segment
	for rows.Next() {
		x, err := scanSeg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *x)
	}
	return out, rows.Err()
}

func (s *DB) ListSegments(ctx context.Context, uid, pid int64) ([]Segment, error) {
	if pid == 0 {
		return s.listSegs(ctx, `SELECT `+segCols+segFrom+`WHERE s.user_id=? ORDER BY s.created, s.id`, uid)
	}
	return s.listSegs(ctx, `SELECT `+segCols+segFrom+`WHERE s.user_id=? AND s.project_id=? ORDER BY s.created, s.id`, uid, pid)
}

func (s *DB) AllSegments(ctx context.Context) ([]Segment, error) {
	return s.listSegs(ctx, `SELECT `+segCols+segFrom+`ORDER BY s.created, s.id`)
}

func (s *DB) CheckedUpTo(ctx context.Context, uid int64) (map[int64]time.Time, error) {
	rows, err := s.rows(ctx, `SELECT project_id, MAX(last_ts) FROM segments WHERE user_id=? AND status<>'failed' GROUP BY project_id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var pid, ms int64
		if err := rows.Scan(&pid, &ms); err != nil {
			return nil, err
		}
		out[pid] = time.UnixMilli(ms).UTC()
	}
	return out, rows.Err()
}

func (s *DB) PendingSegments(ctx context.Context, limit int) ([]Segment, error) {
	return s.listSegs(ctx, `SELECT `+segCols+segFrom+`WHERE s.status='pending' AND s.stream=0 ORDER BY s.checked LIMIT ?`, limit)
}

func (s *DB) MarkSegmentsPending(ctx context.Context, uid, pid int64) error {
	_, err := s.exec(ctx, `UPDATE segments SET status='pending' WHERE user_id=? AND project_id=? AND status='verified'`, uid, pid)
	return err
}

func (s *DB) MinuteClaims(ctx context.Context, userID int64, fromMin, toMin int64, before time.Time, excludeID string) ([]MinuteClaim, error) {
	rows, err := s.rows(ctx, `SELECT m.start_min, m.segment_id, m.human FROM segment_minutes m JOIN segments s ON s.id = m.segment_id
WHERE m.user_id=? AND m.start_min>=? AND m.start_min<=? AND s.status<>'failed' AND (s.created<? OR s.created=? AND s.id<?) AND m.segment_id<>? ORDER BY m.start_min`,
		userID, fromMin, toMin, before.UnixMilli(), before.UnixMilli(), excludeID, excludeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MinuteClaim
	for rows.Next() {
		var c MinuteClaim
		if err := rows.Scan(&c.StartMin, &c.SegmentID, &c.Human); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *DB) VerifiedMinutes(ctx context.Context, pid int64, from, to time.Time) ([]SegmentMinute, error) {
	rows, err := s.rows(ctx, `SELECT m.start_min, SUM(m.human), SUM(m.dark) FROM segment_minutes m JOIN segments s ON s.id = m.segment_id
WHERE m.project_id=? AND s.status='verified' AND m.start_min>=? AND m.start_min<? GROUP BY m.start_min ORDER BY m.start_min`,
		pid, from.Unix()/60, (to.Unix()+59)/60)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SegmentMinute
	for rows.Next() {
		var x SegmentMinute
		if err := rows.Scan(&x.StartMin, &x.Human, &x.Dark); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *DB) SegmentMinutesOf(ctx context.Context, segmentID string) ([]SegmentMinute, error) {
	rows, err := s.rows(ctx, `SELECT start_min, human, dark FROM segment_minutes WHERE segment_id=? ORDER BY start_min`, segmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SegmentMinute
	for rows.Next() {
		var x SegmentMinute
		if err := rows.Scan(&x.StartMin, &x.Human, &x.Dark); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// the hand part of each minute and its seconds by model (segment_models)
	at := map[int64]*SegmentMinute{}
	for i := range out {
		at[out[i].StartMin] = &out[i]
	}
	mr, err := s.rows(ctx, `SELECT start_min, hand, model, seconds, prompts FROM segment_models WHERE segment_id=? ORDER BY start_min, hand, model`, segmentID)
	if err != nil {
		return nil, err
	}
	defer mr.Close()
	for mr.Next() {
		var sm int64
		var hand, prompts int
		var model string
		var sec float64
		if err := mr.Scan(&sm, &hand, &model, &sec, &prompts); err != nil {
			return nil, err
		}
		x := at[sm]
		switch {
		case x == nil:
		case hand != 0:
			x.Hand += sec
		default:
			x.Prompts += prompts
			if model != "" {
				x.Models = append(x.Models, ledger.ModelUse{Model: model, Seconds: sec, Prompts: prompts})
			}
		}
	}
	return out, mr.Err()
}

func (s *DB) SetProjectPulled(ctx context.Context, pid int64) error {
	_, err := s.exec(ctx, `UPDATE projects SET pulled=1 WHERE id=?`, pid)
	return err
}

func (s *DB) ProjectPulled(ctx context.Context, pid int64) (bool, error) {
	var n int
	err := s.row(ctx, `SELECT pulled FROM projects WHERE id=?`, pid).Scan(&n)
	return n == 1, nf(err)
}

func (s *DB) ClosedPromptCount(ctx context.Context, projectID int64) (map[string]int, error) {
	rows, err := s.rows(ctx, `SELECT c.status, COUNT(*) FROM prompt_chain c JOIN project_keys k ON k.device_id=c.device_id AND k.key=c.project_key
WHERE c.status<>'' AND k.project_id=? GROUP BY c.status`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[strings.TrimSpace(st)] = n
	}
	return out, rows.Err()
}

// A segment's until_ts is then just past the last record of its project
// that the server holds, which may be later than the last one counted:
// the next cycle counts the segment once more, with those records.
func (s *DB) CloseStreamSegments(ctx context.Context, deviceID string) error {
	_, err := s.exec(ctx, `UPDATE segments SET is_open=0, until_ts=1+COALESCE(
  (SELECT MAX(r.ts) FROM records r WHERE r.device_id=segments.device_id AND r.project=segments.rec_key AND r.data<>'' AND r.seq>=segments.seq_lo), segments.last_ts)
WHERE device_id=? AND stream=1 AND is_open=1`, deviceID)
	return err
}
