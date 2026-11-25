package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"seatime/pkg/api"
	"seatime/pkg/chain"
	"seatime/pkg/ledger"

	_ "modernc.org/sqlite"
)

// DB is the server store on SQLite or Postgres (see dialect.go).
type DB struct {
	db *conn
	st wstats

	// wmu serializes writers in-process. SQLite allows one writer at a time
	// anyway; contending for its lock inside the busy handler costs polling
	// sleeps (1-100 ms) and unfair tails, a Go mutex hands the lock over at once.
	// On Postgres it keeps the same guarantee: write transactions that read
	// then write (a free username, the next checkpoint) never interleave.
	// That holds for one seatimed process per database, as with SQLite.
	wmu sync.Mutex

	pmu      sync.RWMutex
	prepared map[string]*sql.Stmt // statement cache: parsing/planning was 13% of CPU at 10x
}

// prep returns the cached prepared statement for q.
func (s *DB) prep(q string) (*sql.Stmt, error) {
	s.pmu.RLock()
	st := s.prepared[q]
	s.pmu.RUnlock()
	if st != nil {
		return st, nil
	}
	st, err := s.db.PrepareContext(context.Background(), q)
	if err != nil {
		return nil, err
	}
	s.pmu.Lock()
	if old := s.prepared[q]; old != nil {
		st.Close()
		st = old
	} else {
		s.prepared[q] = st
	}
	s.pmu.Unlock()
	return st, nil
}

func (s *DB) row(ctx context.Context, q string, args ...any) *sql.Row {
	if st, err := s.prep(q); err == nil {
		return st.QueryRowContext(ctx, args...)
	}
	return s.db.QueryRowContext(ctx, q, args...) // surfaces the error on Scan
}

func (s *DB) rows(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	st, err := s.prep(q)
	if err != nil {
		return nil, err
	}
	return st.QueryContext(ctx, args...)
}

// exec runs one autocommit write under the writer lock.
func (s *DB) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	st, err := s.prep(q)
	if err != nil {
		return nil, err
	}
	t0 := time.Now()
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.st.record(time.Since(t0), nil)
	return st.ExecContext(ctx, args...)
}

// wstats counts write transactions and how long they waited for SQLite's single
// write lock (BEGIN IMMEDIATE blocks in the busy handler while another writer
// is active). Exposed through Stats for capacity testing.
type wstats struct {
	txs, waitNS, maxWaitNS, waited10ms, errs atomic.Int64
}

func (w *wstats) record(wait time.Duration, err error) {
	w.txs.Add(1)
	w.waitNS.Add(int64(wait))
	for {
		m := w.maxWaitNS.Load()
		if int64(wait) <= m || w.maxWaitNS.CompareAndSwap(m, int64(wait)) {
			break
		}
	}
	if wait > 10*time.Millisecond {
		w.waited10ms.Add(1)
	}
	if err != nil {
		w.errs.Add(1)
	}
}

// Stats returns cumulative write-lock counters (monotonic except max_wait_ms).
func (s *DB) Stats() map[string]float64 {
	return map[string]float64{
		"write_txs":          float64(s.st.txs.Load()),
		"write_wait_ms":      float64(s.st.waitNS.Load()) / 1e6,
		"write_wait_max_ms":  float64(s.st.maxWaitNS.Load()) / 1e6,
		"write_waits_gt10ms": float64(s.st.waited10ms.Load()),
		"write_begin_errors": float64(s.st.errs.Load()),
		"open_conns":         float64(s.db.raw.Stats().OpenConnections),
		"in_use_conns":       float64(s.db.raw.Stats().InUse),
	}
}

var _ Store = (*DB)(nil)

// ErrSchemaAhead: the database was migrated by a newer seatimed than this
// one. Running on it could misread tables this binary does not know.
var ErrSchemaAhead = errors.New("store: the database schema is newer than this binary")

// Open opens and migrates the database dsn names: a postgres:// or
// postgresql:// URL is a Postgres database (which must exist), anything
// else the path of a SQLite file, created if needed and run in WAL mode.
// It refuses a database whose schema is ahead of this binary
// (ErrSchemaAhead).
func Open(dsn string) (*DB, error) {
	pg := IsPostgres(dsn)
	var db *sql.DB
	var err error
	if pg {
		db, err = sql.Open("pgx", dsn)
	} else {
		q := url.Values{}
		for _, p := range []string{"journal_mode(WAL)", "busy_timeout(5000)", "synchronous(NORMAL)", "foreign_keys(1)", "cache_size(-16000)"} {
			q.Add("_pragma", p)
		}
		q.Set("_txlock", "immediate")
		db, err = sql.Open("sqlite", "file:"+dsn+"?"+q.Encode())
	}
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if pg {
		db.SetConnMaxIdleTime(5 * time.Minute)
	}
	s := &DB{db: &conn{db, pg}, prepared: map[string]*sql.Stmt{}}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Postgres reports whether the store runs on Postgres.
func (s *DB) Postgres() bool { return s.db.pg }

// Close closes the database. On SQLite it first checkpoints the
// write-ahead log into the database file, so a stopped container leaves one
// self-contained file on its volume.
func (s *DB) Close() error {
	s.pmu.Lock()
	for _, st := range s.prepared {
		st.Close()
	}
	s.prepared = map[string]*sql.Stmt{}
	s.pmu.Unlock()
	var cerr error
	if !s.db.pg {
		_, cerr = s.db.raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	if err := s.db.raw.Close(); err != nil {
		return err
	}
	return cerr
}

// Ping is one trivial read: is the database there and answering.
func (s *DB) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations LIMIT 1`).Scan(&one)
}

func (s *DB) migrate() error {
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	for {
		done, err := s.migrateOne(ctx)
		if done || err != nil {
			return err
		}
	}
}

// migrateOne applies the next migration, if any, in a transaction that
// first reads the version: two processes starting at once take turns
// (SQLite's immediate transactions, a Postgres advisory lock).
func (s *DB) migrateOne(ctx context.Context) (done bool, err error) {
	tx, err := s.db.BeginTx(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if s.db.pg {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7413)`); err != nil {
			return false, err
		}
	}
	var cur int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&cur); err != nil {
		return false, err
	}
	if cur > len(migrations) {
		return false, fmt.Errorf("%w (database at migration %d, this binary knows %d)", ErrSchemaAhead, cur, len(migrations))
	}
	if cur >= len(migrations) {
		return true, tx.Commit()
	}
	m := migrations[cur]
	if s.db.pg {
		m = pgMigration(m)
	}
	if _, err := tx.tx.ExecContext(ctx, m); err != nil { // not rebound: a migration has no parameters
		return false, fmt.Errorf("migration %d: %w", cur+1, err)
	}
	if cur+1 == 8 {
		if err := foldProjects(ctx, tx); err != nil {
			return false, fmt.Errorf("migration %d: %w", cur+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES (?)`, cur+1); err != nil {
		return false, err
	}
	return false, tx.Commit()
}

func (s *DB) tx(ctx context.Context, f func(tx *txn) error) error {
	t0 := time.Now()
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx)
	s.st.record(time.Since(t0), err)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func nf(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func ts(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------- users ----------------

var reserved = map[string]bool{"api": true, "live": true, "static": true, "device": true, "settings": true, "login": true,
	"logout": true, "healthz": true, "docs": true, "leaderboard": true, "auth": true, "admin": true, "seatime": true}
var badName = regexp.MustCompile(`[^a-z0-9_-]+`)

// SanitizeUsername maps arbitrary input to [a-z0-9_-]{2,32}.
func SanitizeUsername(s string) string {
	s = strings.Trim(badName.ReplaceAllString(strings.ToLower(s), "-"), "-_")
	if len(s) > 32 {
		s = s[:32]
	}
	if len(s) < 2 {
		s = "user" + s
	}
	return s
}

const userCols = `id, username, display_name, avatar, slack_id, github, public, role, tz_offset_min, created, session_epoch, deleted`

type scanner interface{ Scan(...any) error }

func scanUser(r scanner) (*User, error) {
	var u User
	var pub int
	var created, deleted int64
	if err := r.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Avatar, &u.SlackID, &u.GitHub, &pub, &u.Role, &u.TZOffsetMin, &created, &u.SessionEpoch, &deleted); err != nil {
		return nil, nf(err)
	}
	u.Public = pub == 1
	u.Created = ts(created)
	if deleted > 0 {
		u.Deleted = ts(deleted)
	}
	return &u, nil
}

func (s *DB) LoginIdentity(ctx context.Context, id Identity, want, display, avatar, slackID, github string) (*User, error) {
	var out *User
	err := s.tx(ctx, func(tx *txn) error {
		var uid int64
		err := tx.QueryRowContext(ctx, `SELECT user_id FROM identities WHERE provider=? AND subject=?`, id.Provider, id.Subject).Scan(&uid)
		if err == nil {
			if slackID != "" {
				tx.ExecContext(ctx, `UPDATE users SET slack_id=? WHERE id=? AND slack_id=''`, slackID, uid)
			}
			if avatar != "" {
				tx.ExecContext(ctx, `UPDATE users SET avatar=? WHERE id=? AND avatar=''`, avatar, uid)
			}
			out, err = scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=?`, uid))
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		base := SanitizeUsername(want)
		if reserved[base] {
			base += "-user"
		}
		name := base
		for i := 2; ; i++ {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE username=?`, name).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				break
			}
			suffix := fmt.Sprintf("-%d", i)
			if len(base)+len(suffix) > 32 {
				base = base[:32-len(suffix)]
			}
			name = base + suffix
		}
		if display == "" {
			display = name
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO users(username,display_name,avatar,slack_id,github,created) VALUES (?,?,?,?,?,?) RETURNING id`,
			name, display, avatar, slackID, github, time.Now().Unix()).Scan(&uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO identities(provider,subject,user_id) VALUES (?,?,?)`, id.Provider, id.Subject, uid); err != nil {
			return err
		}
		out, err = scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=?`, uid))
		return err
	})
	return out, err
}

func (s *DB) LinkIdentity(ctx context.Context, userID int64, id Identity, github string) error {
	return s.tx(ctx, func(tx *txn) error {
		var uid int64
		err := tx.QueryRowContext(ctx, `SELECT user_id FROM identities WHERE provider=? AND subject=?`, id.Provider, id.Subject).Scan(&uid)
		if err == nil {
			if uid != userID {
				return ErrConflict
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO identities(provider,subject,user_id) VALUES (?,?,?)`, id.Provider, id.Subject, userID); err != nil {
				return err
			}
		} else {
			return err
		}
		if github != "" {
			_, err = tx.ExecContext(ctx, `UPDATE users SET github=? WHERE id=?`, github, userID)
			return err
		}
		return nil // (the outer err is sql.ErrNoRows after inserting a brand-new identity)
	})
}

func (s *DB) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.row(ctx, `SELECT `+userCols+` FROM users WHERE id=?`, id))
}
func (s *DB) UserByName(ctx context.Context, name string) (*User, error) {
	return scanUser(s.row(ctx, `SELECT `+userCols+` FROM users WHERE username=? AND deleted=0`, strings.ToLower(name)))
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (s *DB) SearchUsers(ctx context.Context, q string, limit int) ([]User, error) {
	pat := "%" + likeEscaper.Replace(strings.ToLower(q)) + "%"
	rows, err := s.rows(ctx, `SELECT `+userCols+` FROM users WHERE deleted=0 AND
		(lower(username) LIKE ? ESCAPE '\' OR lower(display_name) LIKE ? ESCAPE '\' OR lower(slack_id) LIKE ? ESCAPE '\' OR lower(github) LIKE ? ESCAPE '\')
		ORDER BY CASE WHEN lower(username) = ? THEN 0 ELSE 1 END, last_bucket DESC, id LIMIT ?`, pat, pat, pat, pat, strings.ToLower(q), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *DB) UserBySlackID(ctx context.Context, slackID string) (*User, error) {
	if slackID == "" {
		return nil, ErrNotFound
	}
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE slack_id=? ORDER BY id LIMIT 1`, slackID))
}
func (s *DB) UserByIdentity(ctx context.Context, provider, subject string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=(SELECT user_id FROM identities WHERE provider=? AND subject=?)`, provider, subject))
}
func (s *DB) SetSlackID(ctx context.Context, userID int64, slackID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET slack_id=? WHERE id=? AND slack_id=''`, slackID, userID)
	return err
}
func (s *DB) UpdateUser(ctx context.Context, u *User) error {
	_, err := s.exec(ctx, `UPDATE users SET display_name=?, public=?, tz_offset_min=? WHERE id=?`,
		u.DisplayName, b2i(u.Public), u.TZOffsetMin, u.ID)
	return err
}

// SetRole changes a user's role and ends their sessions (a new epoch).
func (s *DB) SetRole(ctx context.Context, userID int64, role string) error {
	_, err := s.exec(ctx, `UPDATE users SET role=?, session_epoch=session_epoch+1 WHERE id=? AND role<>?`, role, userID, role)
	return err
}

// EndSessions ends every session of a user (logout, a stolen cookie).
func (s *DB) EndSessions(ctx context.Context, userID int64) error {
	_, err := s.exec(ctx, `UPDATE users SET session_epoch=session_epoch+1 WHERE id=?`, userID)
	return err
}

func (s *DB) UserIdentities(ctx context.Context, userID int64) ([]Identity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, subject FROM identities WHERE user_id=? ORDER BY provider, subject`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var id Identity
		if err := rows.Scan(&id.Provider, &id.Subject); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *DB) StaffUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users WHERE role<>'user' ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// ---------------- devices ----------------

func (s *DB) CreateDeviceCode(ctx context.Context, hash string, dc DeviceCode) error {
	return s.tx(ctx, func(tx *txn) error {
		tx.ExecContext(ctx, `DELETE FROM device_codes WHERE expires < ?`, time.Now().Unix())
		_, err := tx.ExecContext(ctx, `INSERT INTO device_codes(code_hash,user_code,device_name,os,pubkey,expires,prev_device) VALUES (?,?,?,?,?,?,?)`,
			hash, dc.UserCode, dc.DeviceName, dc.OS, dc.PublicKey, dc.Expires.Unix(), dc.PrevDevice)
		return err
	})
}

func (s *DB) DeviceCodeByUserCode(ctx context.Context, uc string) (*DeviceCode, error) {
	var dc DeviceCode
	var exp int64
	err := s.row(ctx, `SELECT user_code,device_name,os,pubkey,expires,user_id,prev_device FROM device_codes WHERE user_code=?`, uc).
		Scan(&dc.UserCode, &dc.DeviceName, &dc.OS, &dc.PublicKey, &exp, &dc.UserID, &dc.PrevDevice)
	if err != nil {
		return nil, nf(err)
	}
	dc.Expires = ts(exp)
	if dc.Expires.Before(time.Now()) {
		return nil, ErrExpired
	}
	return &dc, nil
}

func (s *DB) ApproveDeviceCode(ctx context.Context, uc string, userID int64) error {
	res, err := s.exec(ctx, `UPDATE device_codes SET user_id=? WHERE user_code=? AND expires>=? AND user_id=0`, userID, uc, time.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *DB) ConsumeDeviceCode(ctx context.Context, hash string) (*DeviceCode, error) {
	var out *DeviceCode
	err := s.tx(ctx, func(tx *txn) error {
		var dc DeviceCode
		var exp int64
		err := tx.QueryRowContext(ctx, `SELECT user_code,device_name,os,pubkey,expires,user_id,prev_device FROM device_codes WHERE code_hash=?`, hash).
			Scan(&dc.UserCode, &dc.DeviceName, &dc.OS, &dc.PublicKey, &exp, &dc.UserID, &dc.PrevDevice)
		if err != nil {
			return nf(err)
		}
		dc.Expires = ts(exp)
		if dc.Expires.Before(time.Now()) {
			tx.ExecContext(ctx, `DELETE FROM device_codes WHERE code_hash=?`, hash)
			out = nil
			return ErrExpired
		}
		if dc.UserID != 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM device_codes WHERE code_hash=?`, hash); err != nil {
				return err
			}
		}
		out = &dc
		return nil
	})
	if errors.Is(err, ErrExpired) {
		// the delete above was rolled back; harmless, purged on next create.
		return nil, ErrExpired
	}
	return out, err
}

const devCols = `id,user_id,name,os,pubkey,last_seen,created,revoked,client_version`

func scanDevice(r scanner) (*Device, error) {
	var d Device
	var ls, cr int64
	var rev int
	if err := r.Scan(&d.ID, &d.UserID, &d.Name, &d.OS, &d.PublicKey, &ls, &cr, &rev, &d.ClientVersion); err != nil {
		return nil, nf(err)
	}
	d.LastSeen, d.Created, d.Revoked = ts(ls), ts(cr), rev == 1
	return &d, nil
}

func (s *DB) CreateDevice(ctx context.Context, d Device, tokenHash string) error {
	_, err := s.exec(ctx, `INSERT INTO devices(id,user_id,name,os,pubkey,token_hash,created,last_seen) VALUES (?,?,?,?,?,?,?,?)`,
		d.ID, d.UserID, d.Name, d.OS, d.PublicKey, tokenHash, orNow(d.Created).Unix(), orNow(d.Created).Unix())
	return err
}
func (s *DB) DeviceByTokenHash(ctx context.Context, h string) (*Device, error) {
	return scanDevice(s.row(ctx, `SELECT `+devCols+` FROM devices WHERE token_hash=?`, h))
}
func (s *DB) DeviceByID(ctx context.Context, id string) (*Device, error) {
	return scanDevice(s.row(ctx, `SELECT `+devCols+` FROM devices WHERE id=?`, id))
}
func (s *DB) ListDevices(ctx context.Context, uid int64) ([]Device, error) {
	rows, err := s.rows(ctx, `SELECT `+devCols+` FROM devices WHERE user_id=? ORDER BY created DESC`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}
func (s *DB) RevokeDevice(ctx context.Context, uid int64, id string) error {
	res, err := s.exec(ctx, `UPDATE devices SET revoked=1 WHERE id=? AND user_id=?`, id, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetClientVersion records the SeaTime version a device syncs with.
func (s *DB) SetClientVersion(ctx context.Context, id, version string) error {
	_, err := s.exec(ctx, `UPDATE devices SET client_version=? WHERE id=?`, version, id)
	return err
}

// ClientVersions counts the devices not revoked that synced since a time,
// by the version they last synced with, most devices first.
func (s *DB) ClientVersions(ctx context.Context, since time.Time) ([]VersionCount, error) {
	rows, err := s.rows(ctx, `SELECT client_version, COUNT(*) FROM devices WHERE revoked=0 AND last_seen>=? GROUP BY client_version ORDER BY COUNT(*) DESC, client_version`, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VersionCount
	for rows.Next() {
		var v VersionCount
		if err := rows.Scan(&v.Version, &v.Devices); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *DB) TouchDevice(ctx context.Context, id string, t time.Time) error {
	_, err := s.exec(ctx, `UPDATE devices SET last_seen=? WHERE id=?`, t.Unix(), id)
	return err
}

// ---------------- projects ----------------

var ghRemote = regexp.MustCompile(`(?i)^(?:https?://|git@|ssh://git@)?(?:www\.)?github\.com[:/]+([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// RepoFromRemote extracts "owner/repo" from a GitHub remote URL or project key
// ("github.com/owner/repo"); "" if it is not a GitHub repo.
func RepoFromRemote(s string) string {
	m := ghRemote.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return ""
	}
	return m[1] + "/" + m[2]
}

var slugBad = regexp.MustCompile(`[^a-z0-9._-]+`)

func slugify(key, name string) string {
	src := key
	if i := strings.LastIndex(key, "/"); i >= 0 {
		src = key[i+1:] // github.com/owner/repo -> repo
	} else if strings.HasPrefix(key, "local:") && name != "" {
		src = name
	}
	s := strings.Trim(slugBad.ReplaceAllString(strings.ToLower(src), "-"), "-.")
	if s == "" {
		s = "project"
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

const projCols = `id,user_id,key,slug,name,remote,repo,hidden,decided,created,named`

func scanProject(r scanner) (*Project, error) {
	var p Project
	var h, d, n int
	var c int64
	if err := r.Scan(&p.ID, &p.UserID, &p.Key, &p.Slug, &p.Name, &p.Remote, &p.Repo, &h, &d, &c, &n); err != nil {
		return nil, nf(err)
	}
	p.Hidden, p.Decided, p.Created, p.Named = h == 1, d == 1, ts(c), n == 1
	return &p, nil
}

func upsertProject(ctx context.Context, tx *txn, uid int64, m api.ProjectMeta) (*Project, error) {
	if m.Key == "" || len(m.Key) > 300 {
		return nil, fmt.Errorf("bad project key")
	}
	p, err := scanProject(tx.QueryRowContext(ctx, `SELECT `+projCols+` FROM projects WHERE user_id=? AND key=?`, uid, m.Key))
	if errors.Is(err, ErrNotFound) {
		slug := slugify(m.Key, m.Name)
		base := slug
		for i := 2; ; i++ {
			var n int
			tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE user_id=? AND slug=?`, uid, slug).Scan(&n)
			if n == 0 {
				break
			}
			slug = fmt.Sprintf("%s-%d", base, i)
		}
		name := m.Name
		if name == "" {
			name = keyTail(m.Key)
		}
		repo := RepoFromRemote(m.Remote)
		if repo == "" {
			repo = RepoFromRemote(m.Key)
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO projects(user_id,key,slug,name,remote,repo,created) VALUES (?,?,?,?,?,?,?) RETURNING id`,
			uid, m.Key, slug, name, m.Remote, repo, time.Now().Unix()).Scan(&id); err != nil {
			return nil, err
		}
		return scanProject(tx.QueryRowContext(ctx, `SELECT `+projCols+` FROM projects WHERE id=?`, id))
	}
	if err != nil {
		return nil, err
	}
	changed := false
	if takesName(p, m) {
		p.Name, changed = m.Name, true
	}
	if m.Remote != "" && m.Remote != p.Remote {
		p.Remote, changed = m.Remote, true
		if p.Repo == "" {
			p.Repo = RepoFromRemote(m.Remote)
		}
	}
	if changed {
		_, err = tx.ExecContext(ctx, `UPDATE projects SET name=?, remote=?, repo=? WHERE id=?`, p.Name, p.Remote, p.Repo, p.ID)
	}
	return p, err
}

// takesName reports whether the name a device sent replaces the stored one.
// A name the owner set on the web (Named) is never replaced. A name the
// person chose on a device, or a public repo's, replaces any other. A
// folder's name (m.Folder) only fills in for a project nobody has named,
// one still shown by its key's own last part: so a project made before
// devices sent folder names picks its name up on the next sync, and a
// second computer's folder name never undoes a chosen one.
func takesName(p *Project, m api.ProjectMeta) bool {
	if m.Name == "" || m.Name == p.Name || p.Named {
		return false
	}
	return !m.Folder || p.Name == "" || p.Name == keyTail(p.Key)
}

// keyTail is the last part of a project key: what a project is called
// until it gets a name.
func keyTail(key string) string { return key[strings.LastIndexAny(key, "/:")+1:] }

func (s *DB) UpsertProject(ctx context.Context, uid int64, m api.ProjectMeta) (*Project, error) {
	// Every sync repeats its project metas; skip the write transaction when the
	// stored row already matches.
	if m.Key != "" && len(m.Key) <= 300 {
		if p, err := s.ProjectByKey(ctx, uid, m.Key); err == nil &&
			!takesName(p, m) && (m.Remote == "" || m.Remote == p.Remote) {
			return p, nil
		}
	}
	var p *Project
	err := s.tx(ctx, func(tx *txn) (err error) { p, err = upsertProject(ctx, tx, uid, m); return })
	return p, err
}
func (s *DB) ProjectByKey(ctx context.Context, uid int64, key string) (*Project, error) {
	return scanProject(s.row(ctx, `SELECT `+projCols+` FROM projects WHERE user_id=? AND key=?`, uid, key))
}
func (s *DB) ProjectBySlug(ctx context.Context, uid int64, v string) (*Project, error) {
	p, err := scanProject(s.row(ctx, `SELECT `+projCols+` FROM projects WHERE user_id=? AND (slug=? OR key=?)`, uid, v, v))
	if errors.Is(err, ErrNotFound) {
		// a key it went by before, or the slug of a project folded into it
		p, err = scanProject(s.row(ctx, `SELECT `+projCols+` FROM projects WHERE id=(SELECT project_id FROM project_links WHERE user_id=? AND name=?)`, uid, v))
	}
	if errors.Is(err, ErrNotFound) {
		// a key a device sent for it (the first project it was sent for)
		p, err = scanProject(s.row(ctx, `SELECT `+projCols+` FROM projects WHERE id=(SELECT project_id FROM project_keys WHERE user_id=? AND key=? ORDER BY created, project_id LIMIT 1)`, uid, v))
	}
	return p, err
}
func (s *DB) ProjectByID(ctx context.Context, id int64) (*Project, error) {
	return scanProject(s.row(ctx, `SELECT `+projCols+` FROM projects WHERE id=?`, id))
}
func (s *DB) ListProjects(ctx context.Context, uid int64) ([]Project, error) {
	rows, err := s.rows(ctx, `SELECT `+projCols+` FROM projects WHERE user_id=? ORDER BY created, id`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}
func (s *DB) EmptyProjects(ctx context.Context, uid int64) (map[int64]bool, error) {
	rows, err := s.rows(ctx, `SELECT p.id FROM projects p WHERE p.user_id=?
AND NOT EXISTS (SELECT 1 FROM hourly h WHERE h.user_id=p.user_id AND h.project_id=p.id AND h.struggle>0)
AND NOT EXISTS (SELECT 1 FROM segment_minutes m WHERE m.project_id=p.id AND m.human>0)
AND NOT EXISTS (SELECT 1 FROM commits c WHERE c.project_id=p.id)`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
func (s *DB) SetProjectHidden(ctx context.Context, uid, pid int64, hidden bool) error {
	_, err := s.exec(ctx, `UPDATE projects SET hidden=?, decided=1 WHERE id=? AND user_id=?`, b2i(hidden), pid, uid)
	return err
}
func (s *DB) SetProjectRepo(ctx context.Context, uid, pid int64, repo string) error {
	_, err := s.exec(ctx, `UPDATE projects SET repo=? WHERE id=? AND user_id=?`, repo, pid, uid)
	return err
}

// ---------------- buckets ----------------

// Under rule heartbeat/1 the bucket columns hold: human and struggle, the
// counted seconds (hand plus AI; one number, so the hourly rollup and every
// reader keep working); agent, the AI part of them; dark_flow, the agent's
// time not counted. Hand seconds are struggle minus agent. Rows written under
// ledger/2 keep their old meaning.

type hkey struct{ pid, hour int64 }
type hdelta struct {
	human, struggle, dark         float64
	prompts, edits, commits, runs int
}

// brow is one stored device row of a minute.
type brow struct {
	dev                           string
	human, struggle, dark, agent  float64
	prompts, edits, commits, runs int
	harn                          string
	models                        string // encodeModels
}

// mergeRows is the device-merged minute (see migrations: MAX durations, SUM counts).
func mergeRows(rs []brow) (m hdelta) {
	for _, r := range rs {
		m.human, m.struggle, m.dark = max(m.human, r.human), max(m.struggle, r.struggle), max(m.dark, r.dark)
		m.prompts += r.prompts
		m.edits += r.edits
		m.commits += r.commits
		m.runs += r.runs
	}
	return
}

// UpsertBuckets stores a device's buckets and maintains the hourly rollup by
// deltas. The existing minutes of each project in the batch's range are read
// with one range query (not two point reads per bucket), and rows whose values
// did not change (clients resend the trailing 30 min every sync) are not
// written at all.
func (s *DB) UpsertBuckets(ctx context.Context, uid int64, deviceID string, bs []ledger.Bucket) error {
	if len(bs) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *txn) error {
		type span struct{ lo, hi int64 }
		spans := map[string]*span{}
		pids := map[string]int64{}
		for _, b := range bs {
			sm := b.Start.Unix() / 60
			if sp := spans[b.Project]; sp == nil {
				spans[b.Project] = &span{sm, sm}
			} else {
				sp.lo, sp.hi = min(sp.lo, sm), max(sp.hi, sm)
			}
		}
		// existing[project id][minute] -> device rows
		existing := map[int64]map[int64][]brow{}
		for key, sp := range spans {
			p, err := upsertProject(ctx, tx, uid, api.ProjectMeta{Key: key})
			if err != nil {
				return err
			}
			pids[key] = p.ID
			m := map[int64][]brow{}
			existing[p.ID] = m
			rows, err := tx.QueryContext(ctx, `SELECT start_min,device_id,human,struggle,dark_flow,agent,prompts,edits,commits,runs,harnesses,models FROM buckets WHERE user_id=? AND project_id=? AND start_min>=? AND start_min<=?`, uid, p.ID, sp.lo, sp.hi)
			if err != nil {
				return err
			}
			for rows.Next() {
				var sm int64
				var r brow
				if err := rows.Scan(&sm, &r.dev, &r.human, &r.struggle, &r.dark, &r.agent, &r.prompts, &r.edits, &r.commits, &r.runs, &r.harn, &r.models); err != nil {
					rows.Close()
					return err
				}
				m[sm] = append(m[sm], r)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
		}
		up, err := tx.PrepareContext(ctx, `INSERT INTO buckets(user_id,project_id,start_min,device_id,human,struggle,dark_flow,agent,prompts,edits,commits,runs,harnesses,models)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(user_id,project_id,start_min,device_id) DO UPDATE SET human=excluded.human, struggle=excluded.struggle, dark_flow=excluded.dark_flow,
 agent=excluded.agent, prompts=excluded.prompts, edits=excluded.edits, commits=excluded.commits, runs=excluded.runs, harnesses=excluded.harnesses, models=excluded.models`)
		if err != nil {
			return err
		}
		defer up.Close()
		deltas := map[hkey]*hdelta{}
		mdeltas := map[mhkey]mval{}
		var dStruggle float64
		for _, b := range bs {
			pid := pids[b.Project]
			sm := b.Start.Unix() / 60
			nr := brow{dev: deviceID, human: b.Seconds(), struggle: b.Seconds(), dark: b.AINotCountedSeconds, agent: b.AISeconds,
				prompts: b.Prompts, edits: b.Edits, commits: b.Commits, runs: b.Runs, harn: strings.Join(b.Harnesses, ","), models: encodeModels(b.Models)}
			rs := existing[pid][sm]
			idx := -1
			for i := range rs {
				if rs[i].dev == deviceID {
					idx = i
				}
			}
			if idx >= 0 && rs[idx] == nr {
				continue // unchanged: no write, no delta
			}
			o := mergeRows(rs)
			nrs := append([]brow(nil), rs...)
			if idx >= 0 {
				nrs[idx] = nr
			} else {
				nrs = append(nrs, nr)
			}
			existing[pid][sm] = nrs
			n := mergeRows(nrs)
			modelDeltas(mdeltas, pid, sm, rs, nrs)
			if _, err := up.ExecContext(ctx, uid, pid, sm, deviceID, nr.human, nr.struggle, nr.dark, nr.agent,
				nr.prompts, nr.edits, nr.commits, nr.runs, nr.harn, nr.models); err != nil {
				return err
			}
			k := hkey{pid, sm / 60}
			d := deltas[k]
			if d == nil {
				d = &hdelta{}
				deltas[k] = d
			}
			d.human += n.human - o.human
			d.struggle += n.struggle - o.struggle
			d.dark += n.dark - o.dark
			d.prompts += n.prompts - o.prompts
			d.edits += n.edits - o.edits
			d.commits += n.commits - o.commits
			d.runs += n.runs - o.runs
			dStruggle += n.struggle - o.struggle
		}
		for k, d := range deltas {
			if *d == (hdelta{}) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO hourly(user_id,project_id,hour,human,struggle,dark_flow,prompts,edits,commits,runs) VALUES (?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(user_id,project_id,hour) DO UPDATE SET human=hourly.human+excluded.human, struggle=hourly.struggle+excluded.struggle, dark_flow=hourly.dark_flow+excluded.dark_flow,
 prompts=hourly.prompts+excluded.prompts, edits=hourly.edits+excluded.edits, commits=hourly.commits+excluded.commits, runs=hourly.runs+excluded.runs`,
				uid, k.pid, k.hour, d.human, d.struggle, d.dark, d.prompts, d.edits, d.commits, d.runs); err != nil {
				return err
			}
		}
		if err := applyModelDeltas(ctx, tx, uid, mdeltas); err != nil {
			return err
		}
		var newest int64
		for _, b := range bs {
			newest = max(newest, b.Start.Unix())
		}
		if _, err = tx.ExecContext(ctx, `UPDATE users SET last_bucket=? WHERE id=? AND last_bucket<?`, newest, uid, newest); err != nil {
			return err
		}
		if dStruggle != 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO kv(name,value) VALUES ('all_struggle',?) ON CONFLICT(name) DO UPDATE SET value=kv.value+excluded.value`, dStruggle)
		}
		return err
	})
}

// DeleteBuckets drops this device's rows for minutes in [from, to) except
// the (project, minute) keys in keep, and takes them out of the hourly
// rollup. A sync that describes its whole window calls it before storing the
// window, so minutes the rule later moved or removed do not linger.
func (s *DB) DeleteBuckets(ctx context.Context, uid int64, deviceID string, from, to time.Time, keep []ledger.Bucket) error {
	lo, hi := from.Unix()/60, to.Unix()/60
	if hi <= lo {
		return nil
	}
	type mk struct {
		pid, min int64
	}
	keepKey := map[string]map[int64]bool{}
	for _, b := range keep {
		if keepKey[b.Project] == nil {
			keepKey[b.Project] = map[int64]bool{}
		}
		keepKey[b.Project][b.Start.Unix()/60] = true
	}
	return s.tx(ctx, func(tx *txn) error {
		rows, err := tx.QueryContext(ctx, `SELECT b.project_id,p.key,b.start_min,b.device_id,b.human,b.struggle,b.dark_flow,b.agent,b.prompts,b.edits,b.commits,b.runs,b.harnesses,b.models
FROM buckets b JOIN projects p ON p.id=b.project_id WHERE b.user_id=? AND b.start_min>=? AND b.start_min<?`, uid, lo, hi)
		if err != nil {
			return err
		}
		all := map[mk][]brow{}
		var gone []mk
		for rows.Next() {
			var k mk
			var key string
			var r brow
			if err := rows.Scan(&k.pid, &key, &k.min, &r.dev, &r.human, &r.struggle, &r.dark, &r.agent, &r.prompts, &r.edits, &r.commits, &r.runs, &r.harn, &r.models); err != nil {
				rows.Close()
				return err
			}
			all[k] = append(all[k], r)
			if r.dev == deviceID && !keepKey[key][k.min] {
				gone = append(gone, k)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		deltas := map[hkey]*hdelta{}
		mdeltas := map[mhkey]mval{}
		var dStruggle float64
		for _, k := range gone {
			rs := all[k]
			o := mergeRows(rs)
			var left []brow
			for _, r := range rs {
				if r.dev != deviceID {
					left = append(left, r)
				}
			}
			n := mergeRows(left)
			modelDeltas(mdeltas, k.pid, k.min, rs, left)
			if _, err := tx.ExecContext(ctx, `DELETE FROM buckets WHERE user_id=? AND project_id=? AND start_min=? AND device_id=?`, uid, k.pid, k.min, deviceID); err != nil {
				return err
			}
			hk := hkey{k.pid, k.min / 60}
			d := deltas[hk]
			if d == nil {
				d = &hdelta{}
				deltas[hk] = d
			}
			d.human += n.human - o.human
			d.struggle += n.struggle - o.struggle
			d.dark += n.dark - o.dark
			d.prompts += n.prompts - o.prompts
			d.edits += n.edits - o.edits
			d.commits += n.commits - o.commits
			d.runs += n.runs - o.runs
			dStruggle += n.struggle - o.struggle
		}
		for k, d := range deltas {
			if _, err := tx.ExecContext(ctx, `UPDATE hourly SET human=human+?, struggle=struggle+?, dark_flow=dark_flow+?, prompts=prompts+?, edits=edits+?, commits=commits+?, runs=runs+?
WHERE user_id=? AND project_id=? AND hour=?`, d.human, d.struggle, d.dark, d.prompts, d.edits, d.commits, d.runs, uid, k.pid, k.hour); err != nil {
				return err
			}
		}
		if err := applyModelDeltas(ctx, tx, uid, mdeltas); err != nil {
			return err
		}
		if dStruggle != 0 {
			_, err = tx.ExecContext(ctx, `UPDATE kv SET value=value+? WHERE name='all_struggle'`, dStruggle)
		}
		return err
	})
}

// DeviceBuckets returns what one device sent for minutes in [from, to), with
// project keys.
func (s *DB) DeviceBuckets(ctx context.Context, uid int64, deviceID string, from, to time.Time) ([]ledger.Bucket, error) {
	rows, err := s.rows(ctx, `SELECT p.key,b.start_min,b.human,b.struggle,b.dark_flow,b.agent,b.prompts,b.edits,b.commits,b.runs,b.harnesses
FROM buckets b JOIN projects p ON p.id=b.project_id WHERE b.user_id=? AND b.device_id=? AND b.start_min>=? AND b.start_min<?`, uid, deviceID, from.Unix()/60, to.Unix()/60)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledger.Bucket
	for rows.Next() {
		var b ledger.Bucket
		var sm int64
		var h string
		var counted, ai float64
		if err := rows.Scan(&b.Project, &sm, &counted, &counted, &b.AINotCountedSeconds, &ai, &b.Prompts, &b.Edits, &b.Commits, &b.Runs, &h); err != nil {
			return nil, err
		}
		b.AISeconds, b.CodingSeconds = min(ai, counted), max(0, counted-ai)
		b.Start = time.Unix(sm*60, 0).UTC()
		if h != "" {
			b.Harnesses = strings.Split(h, ",")
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DaySums groups the hourly rollup into days of the given UTC offset
// (rounded to whole hours). Days are days-since-epoch in that offset; projectID 0 = all.
func (s *DB) DaySums(ctx context.Context, uid, pid, fromDay, toDay int64, offsetMin int) ([]DaySum, error) {
	off := int64(offsetMin / 60)
	lo, hi := fromDay*24-off, (toDay+1)*24-off
	q := `SELECT (hour+?)/24 AS d, SUM(struggle),SUM(dark_flow),SUM(human),SUM(prompts),SUM(edits),SUM(commits),SUM(runs) FROM hourly WHERE user_id=? AND hour>=? AND hour<?`
	args := []any{off, uid, lo, hi}
	if pid != 0 {
		q += ` AND project_id=?`
		args = append(args, pid)
	}
	q += ` GROUP BY d ORDER BY d`
	rows, err := s.rows(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DaySum
	for rows.Next() {
		var d DaySum
		if err := rows.Scan(&d.Day, &d.Struggle, &d.DarkFlow, &d.Human, &d.Prompts, &d.Edits, &d.Commits, &d.Runs); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *DB) Buckets(ctx context.Context, uid, pid int64, from, to time.Time) ([]ledger.Bucket, error) {
	return s.buckets(ctx, uid, pid, from, to, "")
}

// SegDevicePrefix starts the device id of the rows the server writes for a
// checked segment's minutes (the server's showChecked).
const SegDevicePrefix = "seg:"

func (s *DB) SyncedBuckets(ctx context.Context, uid, pid int64, from, to time.Time) ([]ledger.Bucket, error) {
	return s.buckets(ctx, uid, pid, from, to, ` AND device_id NOT LIKE '`+SegDevicePrefix+`%'`)
}

func (s *DB) buckets(ctx context.Context, uid, pid int64, from, to time.Time, where string) ([]ledger.Bucket, error) {
	rows, err := s.rows(ctx, `SELECT project_id,start_min,MAX(human),MAX(struggle),MAX(dark_flow),MAX(agent),SUM(prompts),SUM(edits),SUM(commits),SUM(runs),string_agg(harnesses,',')
FROM buckets WHERE user_id=? AND project_id=? AND start_min>=? AND start_min<?`+where+` GROUP BY project_id,start_min ORDER BY start_min`, uid, pid, from.Unix()/60, to.Unix()/60)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledger.Bucket
	for rows.Next() {
		var b ledger.Bucket
		var p, sm int64
		var h string
		var counted, ai float64
		if err := rows.Scan(&p, &sm, &counted, &counted, &b.AINotCountedSeconds, &ai, &b.Prompts, &b.Edits, &b.Commits, &b.Runs, &h); err != nil {
			return nil, err
		}
		b.AISeconds, b.CodingSeconds = min(ai, counted), max(0, counted-ai)
		b.Start = time.Unix(sm*60, 0).UTC()
		if h != "" {
			b.Harnesses = strings.Split(h, ",")
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *DB) ProjectSums(ctx context.Context, uid int64, from, to time.Time) ([]ProjectSum, error) {
	rows, err := s.rows(ctx, `SELECT h.project_id, SUM(h.struggle),SUM(h.dark_flow),SUM(h.human), MAX(h.hour)
FROM hourly h WHERE h.user_id=? AND h.hour>=? AND h.hour<? GROUP BY h.project_id ORDER BY MAX(h.hour) DESC`, uid, from.Unix()/3600, to.Unix()/3600+1)
	if err != nil {
		return nil, err
	}
	var out []ProjectSum
	var pids []int64
	for rows.Next() {
		var ps ProjectSum
		var pid, hr int64
		if err := rows.Scan(&pid, &ps.Struggle, &ps.DarkFlow, &ps.Human, &hr); err != nil {
			rows.Close()
			return nil, err
		}
		ps.LastStart = time.Unix(hr*3600, 0).UTC()
		out = append(out, ps)
		pids = append(pids, pid)
	}
	rows.Close()
	for i, pid := range pids {
		p, err := s.ProjectByID(ctx, pid)
		if err != nil {
			return nil, err
		}
		out[i].Project = p
	}
	return out, nil
}

func (s *DB) Leaderboard(ctx context.Context, from time.Time, limit int) ([]LeaderRow, error) {
	// aggregate over the covering hourly_hour index first, then join the few
	// users that matter (a join-then-group plan scanned the whole table by user).
	rows, err := s.rows(ctx, `SELECT u.username,u.display_name,u.avatar,t.s,t.d
FROM (SELECT user_id, SUM(struggle) AS s, SUM(dark_flow) AS d FROM hourly WHERE hour>=? GROUP BY user_id HAVING SUM(struggle)>0) t
JOIN users u ON u.id=t.user_id WHERE u.public=1 ORDER BY t.s DESC LIMIT ?`, from.Unix()/3600, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LeaderRow
	for rows.Next() {
		var r LeaderRow
		if err := rows.Scan(&r.Username, &r.DisplayName, &r.Avatar, &r.Struggle, &r.DarkFlow); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *DB) Global(ctx context.Context, now time.Time) (GlobalStats, error) {
	var g GlobalStats
	day := now.UTC().Truncate(24*time.Hour).Unix() / 3600
	cur := now.Unix() / 3600
	if err := s.row(ctx, `SELECT COALESCE(SUM(struggle),0) FROM hourly WHERE hour>=?`, day).Scan(&g.TodayStruggle); err != nil {
		return g, err
	}
	if err := s.row(ctx, `SELECT COALESCE(SUM(struggle),0) FROM hourly WHERE hour>=?`, cur-7*24).Scan(&g.WeekStruggle); err != nil {
		return g, err
	}
	s.row(ctx, `SELECT COALESCE(value,0) FROM kv WHERE name='all_struggle'`).Scan(&g.AllStruggle)
	s.row(ctx, `SELECT COUNT(*) FROM users WHERE deleted=0`).Scan(&g.Users)
	s.row(ctx, `SELECT COUNT(*) FROM users WHERE last_bucket>=?`, (now.Unix()/60-5)*60).Scan(&g.ActiveNow)
	return g, nil
}

// ---------------- checkpoints ----------------

func (s *DB) AddCheckpoint(ctx context.Context, dev string, c chain.Checkpoint, at time.Time) error {
	if c.Seq <= 0 {
		return fmt.Errorf("bad checkpoint seq")
	}
	return s.tx(ctx, func(tx *txn) error {
		var h string
		err := tx.QueryRowContext(ctx, `SELECT hash FROM checkpoints WHERE device_id=? AND seq=?`, dev, c.Seq).Scan(&h)
		if err == nil {
			if h == c.Hash {
				return nil
			}
			return ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var max int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM checkpoints WHERE device_id=?`, dev).Scan(&max); err != nil {
			return err
		}
		if c.Seq < max {
			return ErrRegression
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO checkpoints(device_id,seq,hash,server_ts) VALUES (?,?,?,?)`, dev, c.Seq, c.Hash, at.Unix())
		return err
	})
}

func (s *DB) Checkpoints(ctx context.Context, dev string, lo, hi int64) ([]Checkpoint, error) {
	rows, err := s.rows(ctx, `SELECT seq,hash,server_ts FROM checkpoints WHERE device_id=? AND seq>=? AND seq<=? ORDER BY seq`, dev, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Checkpoint
	for rows.Next() {
		var c Checkpoint
		var t int64
		if err := rows.Scan(&c.Seq, &c.Hash, &t); err != nil {
			return nil, err
		}
		c.ServerTS = ts(t)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *DB) CheckpointAt(ctx context.Context, dev string, seq int64) (*Checkpoint, error) {
	var c Checkpoint
	var t int64
	err := s.row(ctx, `SELECT seq,hash,server_ts FROM checkpoints WHERE device_id=? AND seq=?`, dev, seq).Scan(&c.Seq, &c.Hash, &t)
	c.ServerTS = ts(t)
	return &c, nf(err)
}

func (s *DB) FirstCheckpointFrom(ctx context.Context, dev string, seq int64) (*Checkpoint, error) {
	var c Checkpoint
	var t int64
	err := s.row(ctx, `SELECT seq,hash,server_ts FROM checkpoints WHERE device_id=? AND seq>=? ORDER BY seq LIMIT 1`, dev, seq).Scan(&c.Seq, &c.Hash, &t)
	c.ServerTS = ts(t)
	return &c, nf(err)
}

// ---------------- commits ----------------

func (s *DB) UpsertCommits(ctx context.Context, cs []Commit) error {
	if len(cs) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *txn) error {
		for _, c := range cs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO commits(project_id,sha,ts,ins,del) VALUES (?,?,?,?,?)
ON CONFLICT(project_id,sha) DO UPDATE SET ts=excluded.ts, ins=excluded.ins, del=excluded.del`,
				c.ProjectID, c.SHA, c.TS.Unix(), c.Ins, c.Del); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *DB) queryCommits(ctx context.Context, q string, args ...any) ([]Commit, error) {
	rows, err := s.rows(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Commit
	for rows.Next() {
		var c Commit
		var t int64
		if err := rows.Scan(&c.ProjectID, &c.SHA, &t, &c.Ins, &c.Del); err != nil {
			return nil, err
		}
		c.TS = ts(t)
		out = append(out, c)
	}
	return out, rows.Err()
}

// The verified, message and author columns are no longer read or written:
// the project page asks GitHub from the viewer's browser (web/static/commits.js).
const commitCols = `project_id,sha,ts,ins,del`

func (s *DB) Commits(ctx context.Context, pid int64, from, to time.Time) ([]Commit, error) {
	return s.queryCommits(ctx, `SELECT `+commitCols+` FROM commits WHERE project_id=? AND ts>=? AND ts<? ORDER BY ts`, pid, from.Unix(), to.Unix())
}
func (s *DB) CommitCount(ctx context.Context, pid int64, from, to time.Time) (int, error) {
	var n int
	err := s.row(ctx, `SELECT COUNT(*) FROM commits WHERE project_id=? AND ts>=? AND ts<?`, pid, from.Unix(), to.Unix()).Scan(&n)
	return n, err
}
