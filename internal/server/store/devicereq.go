package store

import (
	"context"
	"database/sql"
	"time"
)

// What an account sets on the web for its computers, how each computer
// last reported itself, and what the owner asked a computer to do
// (migration 4; internal/server devices.go).

// AccountSetting is one setting an account chose on the web.
type AccountSetting struct {
	Value   bool
	Updated time.Time
}

// DeviceHealth is a device's last check-in, as the JSON it sent.
type DeviceHealth struct {
	Report string
	At     time.Time
}

// DeviceRequest is one thing the owner asked a device to do.
type DeviceRequest struct {
	ID       int64
	UserID   int64
	DeviceID string
	Kind     string
	Project  string
	Purge    bool
	Status   string // ReqQueued, ReqSent, ReqDone, ReqFailed
	Error    string
	Count    int
	Created  time.Time
	Updated  time.Time
}

const (
	ReqQueued = "queued"
	ReqSent   = "sent"
	ReqDone   = "done"
	ReqFailed = "failed"
)

// ReqResend is how long a request handed to a device waits for its answer
// before the next check-in hands it out again.
const ReqResend = 10 * time.Minute

type DeviceRequests interface {
	AccountSettings(ctx context.Context, userID int64) (map[string]AccountSetting, error)
	PutAccountSetting(ctx context.Context, userID int64, key string, value bool, at time.Time) error

	PutDeviceHealth(ctx context.Context, deviceID, report string, at time.Time) error
	// DeviceHealths are the last check-ins of a user's devices, by device id.
	DeviceHealths(ctx context.Context, userID int64) (map[string]DeviceHealth, error)

	// AddDeviceRequest queues a request unless the same kind is already
	// open (queued or sent) for the device; it returns the open one's id then.
	AddDeviceRequest(ctx context.Context, r DeviceRequest) (int64, error)
	// TakeDeviceRequests are the device's queued requests, and those sent
	// more than ReqResend ago with no answer; they are marked sent at now.
	TakeDeviceRequests(ctx context.Context, deviceID string, now time.Time) ([]DeviceRequest, error)
	DeviceRequest(ctx context.Context, id int64) (*DeviceRequest, error)
	// FinishDeviceRequest records a device's answer to an open request
	// (ErrNotFound when it is not open).
	FinishDeviceRequest(ctx context.Context, id int64, deviceID, status, errCode string, count int, at time.Time) error
	// UserDeviceRequests are a user's requests since a time, newest first.
	UserDeviceRequests(ctx context.Context, userID int64, since time.Time) ([]DeviceRequest, error)

	// PendingRelinks are the sign-ins waiting for approval that reconnect
	// one of the user's devices, by that device's id.
	PendingRelinks(ctx context.Context, userID int64) (map[string]DeviceCode, error)
	// RelinkDevice gives an earlier device of the user a new token and
	// accepts it again.
	RelinkDevice(ctx context.Context, userID int64, id, tokenHash string) error
	// SetProjectName sets the name the owner chose on the web; a name a
	// device sends no longer replaces it.
	SetProjectName(ctx context.Context, userID, projectID int64, name string) error
}

func (s *DB) AccountSettings(ctx context.Context, userID int64) (map[string]AccountSetting, error) {
	rows, err := s.rows(ctx, `SELECT key,value,updated FROM account_settings WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AccountSetting{}
	for rows.Next() {
		var k string
		var v int
		var ms int64
		if err := rows.Scan(&k, &v, &ms); err != nil {
			return nil, err
		}
		out[k] = AccountSetting{Value: v != 0, Updated: time.UnixMilli(ms).UTC()}
	}
	return out, rows.Err()
}

func (s *DB) PutAccountSetting(ctx context.Context, userID int64, key string, value bool, at time.Time) error {
	_, err := s.exec(ctx, `INSERT INTO account_settings(user_id,key,value,updated) VALUES(?,?,?,?)
  ON CONFLICT(user_id,key) DO UPDATE SET value=excluded.value, updated=excluded.updated
  WHERE account_settings.value<>excluded.value`, userID, key, b2i(value), at.UnixMilli())
	return err
}

func (s *DB) PutDeviceHealth(ctx context.Context, deviceID, report string, at time.Time) error {
	_, err := s.exec(ctx, `INSERT INTO device_health(device_id,report,at) VALUES(?,?,?)
  ON CONFLICT(device_id) DO UPDATE SET report=excluded.report, at=excluded.at`, deviceID, report, at.Unix())
	return err
}

func (s *DB) DeviceHealths(ctx context.Context, userID int64) (map[string]DeviceHealth, error) {
	rows, err := s.rows(ctx, `SELECT h.device_id,h.report,h.at FROM device_health h JOIN devices d ON d.id=h.device_id WHERE d.user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]DeviceHealth{}
	for rows.Next() {
		var id string
		var h DeviceHealth
		var at int64
		if err := rows.Scan(&id, &h.Report, &at); err != nil {
			return nil, err
		}
		h.At = ts(at)
		out[id] = h
	}
	return out, rows.Err()
}

const reqCols = `id,user_id,device_id,kind,project_key,purge,status,error,count,created,updated`

func scanRequest(r scanner) (*DeviceRequest, error) {
	var q DeviceRequest
	var purge int
	var cr, up int64
	if err := r.Scan(&q.ID, &q.UserID, &q.DeviceID, &q.Kind, &q.Project, &purge, &q.Status, &q.Error, &q.Count, &cr, &up); err != nil {
		return nil, nf(err)
	}
	q.Purge, q.Created, q.Updated = purge != 0, time.UnixMilli(cr).UTC(), time.UnixMilli(up).UTC()
	return &q, nil
}

func (s *DB) AddDeviceRequest(ctx context.Context, r DeviceRequest) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *txn) error {
		switch err := tx.QueryRowContext(ctx, `SELECT id FROM device_requests WHERE device_id=? AND kind=? AND project_key=? AND status IN ('queued','sent') ORDER BY id LIMIT 1`,
			r.DeviceID, r.Kind, r.Project).Scan(&id); err {
		case nil:
			_, err := tx.ExecContext(ctx, `UPDATE device_requests SET purge=? WHERE id=?`, b2i(r.Purge), id)
			return err
		case sql.ErrNoRows:
		default:
			return err
		}
		at := orNow(r.Created).UnixMilli()
		return tx.QueryRowContext(ctx, `INSERT INTO device_requests(user_id,device_id,kind,project_key,purge,status,created,updated) VALUES(?,?,?,?,?,?,?,?) RETURNING id`,
			r.UserID, r.DeviceID, r.Kind, r.Project, b2i(r.Purge), ReqQueued, at, at).Scan(&id)
	})
	return id, err
}

func (s *DB) TakeDeviceRequests(ctx context.Context, deviceID string, now time.Time) ([]DeviceRequest, error) {
	var out []DeviceRequest
	err := s.tx(ctx, func(tx *txn) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `SELECT `+reqCols+` FROM device_requests WHERE device_id=? AND (status='queued' OR status='sent' AND updated<?) ORDER BY id`,
			deviceID, now.Add(-ReqResend).UnixMilli())
		if err != nil {
			return err
		}
		for rows.Next() {
			q, err := scanRequest(rows)
			if err != nil {
				rows.Close()
				return err
			}
			out = append(out, *q)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			if _, err := tx.ExecContext(ctx, `UPDATE device_requests SET status=?, updated=? WHERE id=?`, ReqSent, now.UnixMilli(), out[i].ID); err != nil {
				return err
			}
			out[i].Status, out[i].Updated = ReqSent, now
		}
		return nil
	})
	return out, err
}

func (s *DB) DeviceRequest(ctx context.Context, id int64) (*DeviceRequest, error) {
	return scanRequest(s.row(ctx, `SELECT `+reqCols+` FROM device_requests WHERE id=?`, id))
}

func (s *DB) FinishDeviceRequest(ctx context.Context, id int64, deviceID, status, errCode string, count int, at time.Time) error {
	res, err := s.exec(ctx, `UPDATE device_requests SET status=?, error=?, count=?, updated=? WHERE id=? AND device_id=? AND status IN ('queued','sent')`,
		status, errCode, count, at.UnixMilli(), id, deviceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *DB) UserDeviceRequests(ctx context.Context, userID int64, since time.Time) ([]DeviceRequest, error) {
	rows, err := s.rows(ctx, `SELECT `+reqCols+` FROM device_requests WHERE user_id=? AND created>=? ORDER BY created DESC, id DESC`, userID, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceRequest
	for rows.Next() {
		q, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *q)
	}
	return out, rows.Err()
}

func (s *DB) PendingRelinks(ctx context.Context, userID int64) (map[string]DeviceCode, error) {
	rows, err := s.rows(ctx, `SELECT c.user_code,c.device_name,c.os,c.pubkey,c.expires,c.user_id,c.prev_device FROM device_codes c JOIN devices d ON d.id=c.prev_device
  WHERE d.user_id=? AND c.user_id=0 AND c.expires>=? ORDER BY c.expires`, userID, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]DeviceCode{}
	for rows.Next() {
		var dc DeviceCode
		var exp int64
		if err := rows.Scan(&dc.UserCode, &dc.DeviceName, &dc.OS, &dc.PublicKey, &exp, &dc.UserID, &dc.PrevDevice); err != nil {
			return nil, err
		}
		dc.Expires = ts(exp)
		out[dc.PrevDevice] = dc // the newest wins
	}
	return out, rows.Err()
}

func (s *DB) RelinkDevice(ctx context.Context, userID int64, id, tokenHash string) error {
	res, err := s.exec(ctx, `UPDATE devices SET token_hash=?, revoked=0 WHERE id=? AND user_id=?`, tokenHash, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *DB) SetProjectName(ctx context.Context, userID, projectID int64, name string) error {
	_, err := s.exec(ctx, `UPDATE projects SET name=?, named=1 WHERE id=? AND user_id=?`, name, projectID, userID)
	return err
}
