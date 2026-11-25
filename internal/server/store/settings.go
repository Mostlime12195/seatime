package store

import (
	"context"
	"database/sql"
	"time"
)

// Setting is a policy value an admin set on /admin (internal/server
// settings.go). A key with no row takes its default from the environment.
type Setting struct {
	Key     string
	Value   string
	Updated time.Time
	By      string
}

// SettingChange is one line of the settings log: who changed what, from
// and to (nil: the default).
type SettingChange struct {
	Key      string
	Old, New *string
	By       string
	At       time.Time
}

// Settings are every setting an admin set.
func (s *DB) Settings(ctx context.Context) (map[string]Setting, error) {
	rows, err := s.rows(ctx, `SELECT key,value,updated,updated_by FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Setting{}
	for rows.Next() {
		var st Setting
		var at int64
		if err := rows.Scan(&st.Key, &st.Value, &at, &st.By); err != nil {
			return nil, err
		}
		st.Updated = ts(at)
		out[st.Key] = st
	}
	return out, rows.Err()
}

// PutSetting sets key to value (nil: back to its default) and logs the
// change, in one transaction.
func (s *DB) PutSetting(ctx context.Context, key string, value *string, by string, at time.Time) error {
	return s.tx(ctx, func(tx *txn) error {
		var old *string
		var v string
		switch err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v); err {
		case nil:
			old = &v
		case sql.ErrNoRows:
		default:
			return err
		}
		var err error
		if value == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, key)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated,updated_by) VALUES(?,?,?,?)
  ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated=excluded.updated, updated_by=excluded.updated_by`, key, *value, at.Unix(), by)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO settings_log(key,old,new,by,at) VALUES(?,?,?,?,?)`, key, old, value, by, at.Unix())
		return err
	})
}

// SettingsLog is the newest changes first.
func (s *DB) SettingsLog(ctx context.Context, limit int) ([]SettingChange, error) {
	rows, err := s.rows(ctx, `SELECT key,old,new,by,at FROM settings_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettingChange
	for rows.Next() {
		var c SettingChange
		var old, nw sql.NullString
		var at int64
		if err := rows.Scan(&c.Key, &old, &nw, &c.By, &at); err != nil {
			return nil, err
		}
		if old.Valid {
			c.Old = &old.String
		}
		if nw.Valid {
			c.New = &nw.String
		}
		c.At = ts(at)
		out = append(out, c)
	}
	return out, rows.Err()
}
