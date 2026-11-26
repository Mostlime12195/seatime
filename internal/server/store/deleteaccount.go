package store

import (
	"context"
	"strconv"
	"time"
)

// AccountDeletion is an owner deleting their own SeaTime account.
type AccountDeletion interface {
	// DeleteAccount deletes everything SeaTime holds for the account: its
	// projects, hours, records, commits, prompt checks, devices' data,
	// applications and approvals, settings, logins and sessions. The
	// devices in uninstall keep only their row and get an uninstall
	// request (with their data), so that they can still fetch it; the
	// account row stays, empty and renamed, until they have done it
	// (PurgeDeleted). With no such device the row goes at once.
	DeleteAccount(ctx context.Context, userID int64, uninstall []string, now time.Time) error
	// PurgeDeleted removes deleted accounts whose computers have all
	// removed SeaTime, and any deleted before olderThan; it returns how
	// many it removed.
	PurgeDeleted(ctx context.Context, olderThan time.Time) (int, error)
}

func (s *DB) DeleteAccount(ctx context.Context, uid int64, uninstall []string, now time.Time) error {
	keep := map[string]bool{}
	for _, d := range uninstall {
		keep[d] = true
	}
	return s.tx(ctx, func(tx *txn) error {
		var slack string
		if err := tx.QueryRowContext(ctx, `SELECT slack_id FROM users WHERE id=?`, uid).Scan(&slack); err != nil {
			return nf(err)
		}
		var devs []string
		rows, err := tx.QueryContext(ctx, `SELECT id FROM devices WHERE user_id=?`, uid)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			devs = append(devs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range devs {
			for _, q := range []string{
				`DELETE FROM checkpoints WHERE device_id=?`,
				`DELETE FROM records WHERE device_id=?`,
				`DELETE FROM record_streams WHERE device_id=?`,
				`DELETE FROM prompt_verdicts WHERE device_id=?`,
				`DELETE FROM prompt_chain WHERE device_id=?`,
				`DELETE FROM device_health WHERE device_id=?`,
			} {
				if _, err := tx.ExecContext(ctx, q, d); err != nil {
					return err
				}
			}
		}
		for _, q := range []string{
			`DELETE FROM commits WHERE project_id IN (SELECT id FROM projects WHERE user_id=?)`,
			`DELETE FROM segment_blobs WHERE id IN (SELECT id FROM segments WHERE user_id=?)`,
			`DELETE FROM segment_minutes WHERE user_id=?`,
			`DELETE FROM segment_models WHERE user_id=?`,
			`DELETE FROM segments WHERE user_id=?`,
			`DELETE FROM buckets WHERE user_id=?`,
			`DELETE FROM hourly WHERE user_id=?`,
			`DELETE FROM hourly_models WHERE user_id=?`,
			`DELETE FROM projects WHERE user_id=?`,
			`DELETE FROM project_keys WHERE user_id=?`,
			`DELETE FROM project_links WHERE user_id=?`,
			`DELETE FROM pending_time WHERE user_id=?`,
			`DELETE FROM check_spend WHERE user_id=?`,
			`DELETE FROM user_check_periods WHERE user_id=?`,
			`DELETE FROM prompt_pulls WHERE user_id=?`,
			`DELETE FROM account_settings WHERE user_id=?`,
			`DELETE FROM identities WHERE user_id=?`,
			`DELETE FROM device_codes WHERE user_id=?`,
			`DELETE FROM oauth_tokens WHERE user_id=? OR client_id IN (SELECT id FROM oauth_clients WHERE user_id=?)`,
			`DELETE FROM oauth_codes WHERE user_id=? OR client_id IN (SELECT id FROM oauth_clients WHERE user_id=?)`,
			`DELETE FROM oauth_grants WHERE user_id=? OR client_id IN (SELECT id FROM oauth_clients WHERE user_id=?)`,
			`DELETE FROM oauth_clients WHERE user_id=?`,
			`DELETE FROM device_requests WHERE user_id=?`,
		} {
			args := []any{uid}
			if n := countParams(q); n == 2 {
				args = append(args, uid)
			}
			if _, err := tx.ExecContext(ctx, q, args...); err != nil {
				return err
			}
		}
		if slack != "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM hackatime_trust WHERE slack_id=?`, slack); err != nil {
				return err
			}
		}
		left := 0
		at := now.UnixMilli()
		for _, d := range devs {
			if !keep[d] {
				if _, err := tx.ExecContext(ctx, `DELETE FROM devices WHERE id=?`, d); err != nil {
					return err
				}
				continue
			}
			left++
			if _, err := tx.ExecContext(ctx, `UPDATE devices SET name='', revoked=0 WHERE id=?`, d); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO device_requests(user_id,device_id,kind,project_key,purge,status,created,updated) VALUES(?,?,'uninstall','',1,?,?,?)`,
				uid, d, ReqQueued, at, at); err != nil {
				return err
			}
		}
		if left == 0 {
			_, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id=?`, uid)
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE users SET username=?, display_name='', avatar='', slack_id='', github='', public=0, role='user',
			tz_offset_min=0, last_bucket=0, session_epoch=session_epoch+1, deleted=? WHERE id=?`,
			"deleted-"+strconv.FormatInt(uid, 10), now.Unix(), uid)
		return err
	})
}

func countParams(q string) int {
	n := 0
	for _, c := range q {
		if c == '?' {
			n++
		}
	}
	return n
}

func (s *DB) PurgeDeleted(ctx context.Context, olderThan time.Time) (int, error) {
	n := 0
	err := s.tx(ctx, func(tx *txn) error {
		rows, err := tx.QueryContext(ctx, `SELECT u.id FROM users u WHERE u.deleted > 0 AND (u.deleted < ? OR NOT EXISTS (SELECT 1 FROM devices d WHERE d.user_id=u.id AND d.revoked=0))`, olderThan.Unix())
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			for _, q := range []string{
				`DELETE FROM device_requests WHERE user_id=?`,
				`DELETE FROM device_health WHERE device_id IN (SELECT id FROM devices WHERE user_id=?)`,
				`DELETE FROM devices WHERE user_id=?`,
				`DELETE FROM users WHERE id=? AND deleted > 0`,
			} {
				if _, err := tx.ExecContext(ctx, q, id); err != nil {
					return err
				}
			}
			n++
		}
		return nil
	})
	return n, err
}
