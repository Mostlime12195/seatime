package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"seatime/pkg/promptcheck"
)

// The prompt chain (pkg/promptcheck chain.go), each account's own prompt
// check setting, and the server's requests for saved prompts. Hashes,
// times and project keys only: no prompt text is ever stored here.

// Chain statuses: open, the text sent did not match, never sent in time.
const (
	ChainOpen     = ""
	ChainMismatch = "mismatch"
	ChainMissing  = "missing"
)

// ChainRow is one stored entry of a device's prompt chain.
type ChainRow struct {
	promptcheck.ChainEntry
	Received time.Time
	Status   string
}

// PromptPull is a request for an account's saved prompts sent in
// [From, To), of Project (a server key) or of every project ("").
type PromptPull struct {
	ID       int64
	UserID   int64
	Project  string
	From, To time.Time
	Created  time.Time
	Deadline time.Time
	By       string
	Closed   bool
}

// PromptChain is the part of Store behind the prompt chain.
type PromptChain interface {
	// PromptChainHead is a device's last entry: seq 0 and
	// promptcheck.ChainGenesis when it has none.
	PromptChainHead(ctx context.Context, deviceID string) (seq int64, link string, err error)
	// AddPromptChain appends es to a device's chain after checking that they
	// continue it (promptcheck.VerifyChain). Entries the server already
	// holds with the same link are skipped. ErrConflict when they do not
	// continue it; nothing is stored then.
	AddPromptChain(ctx context.Context, deviceID string, es []promptcheck.ChainEntry, received time.Time) (taken int, err error)
	// PromptChainEntry is a device's entry for a prompt hash; ErrNotFound if none.
	PromptChainEntry(ctx context.Context, deviceID, commit string) (*ChainRow, error)
	// ChainFrom is when a device's first chained prompt was sent (zero: none).
	ChainFrom(ctx context.Context, deviceID string) (time.Time, error)
	// ChainResolve counts a device's chained prompts in [from, to) that are
	// not repeats, and how many of them are settled (a verdict, or a status).
	ChainResolve(ctx context.Context, deviceID string, from, to time.Time) (n, settled int, err error)
	// SetChainStatus closes an open entry without a verdict.
	SetChainStatus(ctx context.Context, deviceID, commit, status string) error
	// ClosedPrompts are a device's entries closed without a verdict, by
	// prompt hash (the newest limit).
	ClosedPrompts(ctx context.Context, deviceID string, limit int) (map[string]string, error)

	// UserCheckPeriods is an account's own setting's history, oldest first.
	UserCheckPeriods(ctx context.Context, userID int64) ([]promptcheck.Period, error)
	// SetUserCheck records the account's setting from at on, when it changes.
	SetUserCheck(ctx context.Context, userID int64, on bool, at time.Time) error

	// AddPromptPull stores a request for saved prompts.
	AddPromptPull(ctx context.Context, p PromptPull) (int64, error)
	// PromptPulls are an account's requests (open only, or all).
	PromptPulls(ctx context.Context, userID int64, openOnly bool) ([]PromptPull, error)
	ClosePromptPull(ctx context.Context, id int64) error
	// PullResolve counts the prompts (not repeats) of an account's devices
	// a pull covers, and how many of them are settled.
	PullResolve(ctx context.Context, p PromptPull) (n, settled int, err error)
	// MarkPullMissing marks every open prompt a pull covers that has no
	// verdict as missing, and returns how many.
	MarkPullMissing(ctx context.Context, p PromptPull) (int, error)
}

func (s *DB) PromptChainHead(ctx context.Context, deviceID string) (int64, string, error) {
	var seq int64
	var link string
	err := s.row(ctx, `SELECT seq,link FROM prompt_chain WHERE device_id=? ORDER BY seq DESC LIMIT 1`, deviceID).Scan(&seq, &link)
	if err == sql.ErrNoRows {
		return 0, promptcheck.ChainGenesis, nil
	}
	return seq, link, err
}

func (s *DB) AddPromptChain(ctx context.Context, deviceID string, es []promptcheck.ChainEntry, received time.Time) (int, error) {
	taken := 0
	err := s.tx(ctx, func(tx *txn) error {
		seq, link := int64(0), promptcheck.ChainGenesis
		switch err := tx.QueryRowContext(ctx, `SELECT seq,link FROM prompt_chain WHERE device_id=? ORDER BY seq DESC LIMIT 1`, deviceID).Scan(&seq, &link); err {
		case nil, sql.ErrNoRows:
		default:
			return err
		}
		// a retry of entries already held: the same links, skipped
		for len(es) > 0 && es[0].Seq <= seq {
			var held string
			if err := tx.QueryRowContext(ctx, `SELECT link FROM prompt_chain WHERE device_id=? AND seq=?`, deviceID, es[0].Seq).Scan(&held); err != nil || held != es[0].Link {
				return ErrConflict
			}
			es = es[1:]
		}
		if err := promptcheck.VerifyChain(seq, link, es); err != nil {
			return errors.Join(ErrConflict, err) // the handler checked each entry's shape
		}
		for _, e := range es {
			if _, err := tx.ExecContext(ctx, `INSERT INTO prompt_chain(device_id,seq,ts,project_key,commit_hash,rep,link,received) VALUES(?,?,?,?,?,?,?,?)`,
				deviceID, e.Seq, e.TS.UnixMilli(), e.Project, e.Commit, b2i(e.Repeat), e.Link, received.UnixMilli()); err != nil {
				return err
			}
			taken++
		}
		return nil
	})
	return taken, err
}

func (s *DB) PromptChainEntry(ctx context.Context, deviceID, commit string) (*ChainRow, error) {
	var r ChainRow
	var ts, rec int64
	var rep int
	err := s.row(ctx, `SELECT seq,ts,project_key,commit_hash,rep,link,received,status FROM prompt_chain WHERE device_id=? AND commit_hash=? ORDER BY seq LIMIT 1`, deviceID, commit).
		Scan(&r.Seq, &ts, &r.Project, &r.Commit, &rep, &r.Link, &rec, &r.Status)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.TS, r.Received, r.Repeat = time.UnixMilli(ts).UTC(), time.UnixMilli(rec).UTC(), rep != 0
	return &r, nil
}

func (s *DB) ChainFrom(ctx context.Context, deviceID string) (time.Time, error) {
	var ms sql.NullInt64
	if err := s.row(ctx, `SELECT MIN(ts) FROM prompt_chain WHERE device_id=?`, deviceID).Scan(&ms); err != nil || !ms.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(ms.Int64).UTC(), nil
}

const settledSQL = `SELECT COUNT(*), COALESCE(SUM(CASE WHEN c.status<>'' OR v.check_hash IS NOT NULL THEN 1 ELSE 0 END),0)
  FROM prompt_chain c LEFT JOIN prompt_verdicts v ON v.device_id=c.device_id AND v.check_hash=c.commit_hash
  WHERE c.rep=0 AND c.ts>=? AND c.ts<?`

func (s *DB) ChainResolve(ctx context.Context, deviceID string, from, to time.Time) (n, settled int, err error) {
	err = s.row(ctx, settledSQL+` AND c.device_id=?`, from.UnixMilli(), to.UnixMilli(), deviceID).Scan(&n, &settled)
	return
}

func (s *DB) SetChainStatus(ctx context.Context, deviceID, commit, status string) error {
	_, err := s.exec(ctx, `UPDATE prompt_chain SET status=? WHERE device_id=? AND commit_hash=? AND status=''`, status, deviceID, commit)
	return err
}

func (s *DB) ClosedPrompts(ctx context.Context, deviceID string, limit int) (map[string]string, error) {
	rows, err := s.rows(ctx, `SELECT commit_hash,status FROM prompt_chain WHERE device_id=? AND status<>'' ORDER BY seq DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var h, st string
		if err := rows.Scan(&h, &st); err != nil {
			return nil, err
		}
		out[h] = st
	}
	return out, rows.Err()
}

func (s *DB) UserCheckPeriods(ctx context.Context, userID int64) ([]promptcheck.Period, error) {
	rows, err := s.rows(ctx, `SELECT from_ms,on_ FROM user_check_periods WHERE user_id=? ORDER BY from_ms`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []promptcheck.Period
	for rows.Next() {
		var ms int64
		var on int
		if err := rows.Scan(&ms, &on); err != nil {
			return nil, err
		}
		out = append(out, promptcheck.Period{From: time.UnixMilli(ms).UTC(), On: on != 0})
	}
	return out, rows.Err()
}

func (s *DB) SetUserCheck(ctx context.Context, userID int64, on bool, at time.Time) error {
	return s.tx(ctx, func(tx *txn) error {
		cur, last := true, int64(0) // no history: on
		var ms int64
		var o int
		switch err := tx.QueryRowContext(ctx, `SELECT from_ms,on_ FROM user_check_periods WHERE user_id=? ORDER BY from_ms DESC LIMIT 1`, userID).Scan(&ms, &o); err {
		case nil:
			cur, last = o != 0, ms
		case sql.ErrNoRows:
		default:
			return err
		}
		if cur == on {
			return nil
		}
		t := at.UnixMilli()
		if t <= last {
			t = last + 1
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO user_check_periods(user_id,from_ms,on_) VALUES(?,?,?)`, userID, t, b2i(on))
		return err
	})
}

func (s *DB) AddPromptPull(ctx context.Context, p PromptPull) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *txn) error {
		return tx.QueryRowContext(ctx, `INSERT INTO prompt_pulls(user_id,project_key,from_ms,to_ms,created,deadline,by) VALUES(?,?,?,?,?,?,?) RETURNING id`,
			p.UserID, p.Project, p.From.UnixMilli(), p.To.UnixMilli(), p.Created.UnixMilli(), p.Deadline.UnixMilli(), p.By).Scan(&id)
	})
	return id, err
}

func (s *DB) PromptPulls(ctx context.Context, userID int64, openOnly bool) ([]PromptPull, error) {
	q := `SELECT id,user_id,project_key,from_ms,to_ms,created,deadline,by,closed FROM prompt_pulls WHERE user_id=?`
	if openOnly {
		q += ` AND closed=0`
	}
	rows, err := s.rows(ctx, q+` ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PromptPull
	for rows.Next() {
		var p PromptPull
		var from, to, cr, dl int64
		var closed int
		if err := rows.Scan(&p.ID, &p.UserID, &p.Project, &from, &to, &cr, &dl, &p.By, &closed); err != nil {
			return nil, err
		}
		p.From, p.To, p.Created, p.Deadline = time.UnixMilli(from).UTC(), time.UnixMilli(to).UTC(), time.UnixMilli(cr).UTC(), time.UnixMilli(dl).UTC()
		p.Closed = closed != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *DB) ClosePromptPull(ctx context.Context, id int64) error {
	_, err := s.exec(ctx, `UPDATE prompt_pulls SET closed=1 WHERE id=?`, id)
	return err
}

// pullScope is the WHERE clause (after settledSQL's) for a pull's prompts.
func pullScope(p PromptPull) (string, []any) {
	q, args := ` AND c.device_id IN (SELECT id FROM devices WHERE user_id=?)`, []any{p.UserID}
	if p.Project != "" {
		q, args = q+` AND c.project_key=?`, append(args, p.Project)
	}
	return q, args
}

func (s *DB) PullResolve(ctx context.Context, p PromptPull) (n, settled int, err error) {
	q, args := pullScope(p)
	err = s.row(ctx, settledSQL+q, append([]any{p.From.UnixMilli(), p.To.UnixMilli()}, args...)...).Scan(&n, &settled)
	return
}

func (s *DB) MarkPullMissing(ctx context.Context, p PromptPull) (int, error) {
	q := `UPDATE prompt_chain SET status=? WHERE status='' AND rep=0 AND ts>=? AND ts<? AND device_id IN (SELECT id FROM devices WHERE user_id=?)
  AND NOT EXISTS (SELECT 1 FROM prompt_verdicts v WHERE v.device_id=prompt_chain.device_id AND v.check_hash=prompt_chain.commit_hash)`
	args := []any{ChainMissing, p.From.UnixMilli(), p.To.UnixMilli(), p.UserID}
	if p.Project != "" {
		q, args = q+` AND project_key=?`, append(args, p.Project)
	}
	res, err := s.exec(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
