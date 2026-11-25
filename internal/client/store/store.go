// Package store is seatime's local event log: SQLite (pure Go) in WAL mode, safe
// for many concurrent short-lived hook processes appending at once. Every
// append assigns the next per-device Seq and links the hash chain inside one
// IMMEDIATE transaction.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"seatime/pkg/chain"
	"seatime/pkg/event"
)

const schema = `
CREATE TABLE IF NOT EXISTS events(
  seq     INTEGER PRIMARY KEY,
  ts      INTEGER NOT NULL,
  kind    TEXT NOT NULL,
  project TEXT NOT NULL DEFAULT '',
  session TEXT NOT NULL DEFAULT '',
  leaf    TEXT NOT NULL,
  hash    TEXT NOT NULL,
  data    BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS events_ts ON events(ts);
CREATE INDEX IF NOT EXISTS events_project ON events(project, ts);
CREATE INDEX IF NOT EXISTS events_session ON events(session, seq);
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS projects(
  key TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', remote TEXT NOT NULL DEFAULT '',
  root TEXT NOT NULL DEFAULT '', first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL,
  label TEXT NOT NULL DEFAULT '');
-- every folder this device has seen and the project it belongs to, for
-- good (folders.go); label is what the folder goes by now
CREATE TABLE IF NOT EXISTS folders(root TEXT PRIMARY KEY, key TEXT NOT NULL, label TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS session_proj(
  session TEXT NOT NULL, project TEXT NOT NULL, writes INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(session, project));
CREATE TABLE IF NOT EXISTS commits_seen(sha TEXT NOT NULL, project TEXT NOT NULL, PRIMARY KEY(sha, project));
CREATE TABLE IF NOT EXISTS labels(block INTEGER PRIMARY KEY, label TEXT NOT NULL);
-- the prompts already sent, per project, as keyed hashes (definition 2
-- of the rule); never leaves the machine
CREATE TABLE IF NOT EXISTS prompts_sent(project TEXT NOT NULL, hash TEXT NOT NULL, PRIMARY KEY(project, hash)) WITHOUT ROWID;
`

// schemaVersion is the PRAGMA user_version of schema and checkSchema. A
// change to them raises it and adds a step to openOnce for older files.
const schemaVersion = 10

type Store struct {
	db   *sql.DB
	Path string
	key  []byte // the device's commitment key (see Key)
}

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	// Several hooks can open a fresh database at once; the WAL switch and the
	// schema setup can then report SQLITE_BUSY without waiting on the busy
	// handler. Both are idempotent, so retry briefly.
	var err error
	for i := 0; i < 25; i++ {
		var s *Store
		if s, err = openOnce(path); err == nil || !isBusy(err) {
			return s, err
		}
		time.Sleep(time.Duration(5*(i+1)) * time.Millisecond)
	}
	return nil, err
}

func isBusy(err error) bool {
	m := err.Error()
	return strings.Contains(m, "SQLITE_BUSY") || strings.Contains(m, "database is locked")
}

func openOnce(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-1000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	key, err := loadKey(filepath.Dir(path))
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, Path: path, key: key}
	var ver int
	if err := db.QueryRow("PRAGMA user_version").Scan(&ver); err != nil {
		db.Close()
		return nil, err
	}
	if ver < schemaVersion {
		// every statement is idempotent: another process may be creating
		// the same file at this moment
		if err := addColumn(db, "check_turns", "rep", "INTEGER NOT NULL DEFAULT 0"); err != nil { // 7: before the indexes
			db.Close()
			return nil, err
		}
		if err := addColumn(db, "check_turns", "info", "TEXT NOT NULL DEFAULT ''"); err != nil { // 8
			db.Close()
			return nil, err
		}
		if err := addColumn(db, "projects", "label", "TEXT NOT NULL DEFAULT ''"); err != nil { // 9
			db.Close()
			return nil, err
		}
		// 10: a device keeps only the decision and the minutes of a verdict
		const verdictsTrim = "UPDATE check_verdicts SET kind='', labels='{}' WHERE kind<>'' OR labels<>'{}';\n"
		if _, err := db.Exec(schema + checkSchema + foldersFill + verdictsTrim + "PRAGMA user_version=" + strconv.Itoa(schemaVersion)); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// addColumn adds a column to an existing table that lacks it (an older
// file). A table not made yet gets it from the schema; another process
// adding it at the same moment is not an error.
func addColumn(db *sql.DB, table, col, def string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	exists, has := false, false
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		exists = true
		has = has || n == col
	}
	rows.Close()
	if !exists || has {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + def); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return err
	}
	return nil
}

// Append stamps and persists e, returning the stored event (Seq set). TS
// defaults to now; it is normalised to UTC at millisecond precision.
func (s *Store) Append(e event.Event) (event.Event, error) {
	if e.Kind == "" {
		return e, errors.New("store: event kind required")
	}
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	e.TS = e.TS.UTC().Truncate(time.Millisecond)
	tx, err := s.db.Begin()
	if err != nil {
		return e, err
	}
	defer tx.Rollback()
	var seq int64
	head := chain.Genesis
	err = tx.QueryRow("SELECT seq, hash FROM events ORDER BY seq DESC LIMIT 1").Scan(&seq, &head)
	if err == sql.ErrNoRows {
		seq, head, err = baseTx(tx)
	}
	if err != nil {
		return e, err
	}
	e.Seq = seq + 1
	e.TS = e.TS.UTC().Truncate(time.Millisecond)
	if e.Canon == 0 {
		e.Canon = event.CanonKeyed // the chain commits to paths, text and extras only as keyed hashes
	}
	data, err := json.Marshal(e) // raw event (the leaf commits to its private form)
	if err != nil {
		return e, err
	}
	leaf, err := chain.LeafKey(e, s.key)
	if err != nil {
		return e, err
	}
	h, err := chain.Link(head, leaf)
	if err != nil {
		return e, err
	}
	if _, err := tx.Exec("INSERT INTO events(seq,ts,kind,project,session,leaf,hash,data) VALUES(?,?,?,?,?,?,?,?)",
		e.Seq, e.TS.UnixMilli(), string(e.Kind), e.Project, e.SessionID, leaf, h, data); err != nil {
		return e, err
	}
	return e, tx.Commit()
}

// Head returns the newest seq and chain hash (the chain base, normally
// 0 and Genesis, when empty).
func (s *Store) Head() (int64, string, error) {
	var seq int64
	h := chain.Genesis
	err := s.db.QueryRow("SELECT seq, hash FROM events ORDER BY seq DESC LIMIT 1").Scan(&seq, &h)
	if err == sql.ErrNoRows {
		return s.Base()
	}
	return seq, h, err
}

// Base is where this log's chain starts: (0, Genesis), or the checkpoint a
// restarted log continues from (see Recover).
func (s *Store) Base() (int64, string, error) {
	return baseOf(s.db.QueryRow)
}

func baseTx(tx *sql.Tx) (int64, string, error) { return baseOf(tx.QueryRow) }

func baseOf(q func(string, ...any) *sql.Row) (int64, string, error) {
	var v, h string
	if err := q("SELECT v FROM meta WHERE k='chain_base_seq'").Scan(&v); err == sql.ErrNoRows {
		return 0, chain.Genesis, nil
	} else if err != nil {
		return 0, "", err
	}
	if err := q("SELECT v FROM meta WHERE k='chain_base_hash'").Scan(&h); err != nil {
		return 0, "", err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, h, err
}

// HashAt returns the chain hash after event seq (the base hash at or below
// the base seq).
func (s *Store) HashAt(seq int64) (string, error) {
	bs, bh, err := s.Base()
	if err != nil {
		return "", err
	}
	if seq <= bs {
		if seq == bs {
			return bh, nil
		}
		if seq <= 0 && bs == 0 {
			return chain.Genesis, nil
		}
		return "", fmt.Errorf("seq %d is before this log's chain base %d (the log was restarted)", seq, bs)
	}
	var h string
	err = s.db.QueryRow("SELECT hash FROM events WHERE seq=?", seq).Scan(&h)
	return h, err
}

// Query selects events. Zero fields are ignored. Results are in seq order.
type Query struct {
	Since, Until time.Time // by TS, [Since, Until)
	Project      string
	Session      string
	Kinds        []event.Kind
	AfterSeq     int64 // seq > AfterSeq
	UpToSeq      int64 // seq <= UpToSeq (0 = no limit)
	Limit        int
	Desc         bool
}

func (q Query) where() (string, []any) {
	var cl []string
	var args []any
	if !q.Since.IsZero() {
		cl = append(cl, "ts >= ?")
		args = append(args, q.Since.UnixMilli())
	}
	if !q.Until.IsZero() {
		cl = append(cl, "ts < ?")
		args = append(args, q.Until.UnixMilli())
	}
	if q.Project != "" {
		cl = append(cl, "project = ?")
		args = append(args, q.Project)
	}
	if q.Session != "" {
		cl = append(cl, "session = ?")
		args = append(args, q.Session)
	}
	if len(q.Kinds) > 0 {
		ph := make([]string, len(q.Kinds))
		for i, k := range q.Kinds {
			ph[i] = "?"
			args = append(args, string(k))
		}
		cl = append(cl, "kind IN ("+strings.Join(ph, ",")+")")
	}
	if q.AfterSeq > 0 {
		cl = append(cl, "seq > ?")
		args = append(args, q.AfterSeq)
	}
	if q.UpToSeq > 0 {
		cl = append(cl, "seq <= ?")
		args = append(args, q.UpToSeq)
	}
	if len(cl) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(cl, " AND "), args
}

func (s *Store) Events(q Query) ([]event.Event, error) {
	w, args := q.where()
	sqlq := "SELECT data FROM events" + w
	if q.Desc {
		sqlq += " ORDER BY seq DESC"
	} else {
		sqlq += " ORDER BY seq"
	}
	if q.Limit > 0 {
		sqlq += " LIMIT " + strconv.Itoa(q.Limit)
	}
	rows, err := s.db.Query(sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []event.Event
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var e event.Event
		if err := json.Unmarshal(b, &e); err != nil {
			continue // a damaged record (see DamagedError): skip it, never fail the whole read
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Row is a raw stored entry (for building bundles without decoding everything).
type Row struct {
	Seq     int64
	Project string
	Leaf    string
	Hash    string
	Data    []byte
}

// Rows returns raw rows with from <= seq <= to, in order.
func (s *Store) Rows(from, to int64) ([]Row, error) {
	rows, err := s.db.Query("SELECT seq, project, leaf, hash, data FROM events WHERE seq >= ? AND seq <= ? ORDER BY seq", from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Seq, &r.Project, &r.Leaf, &r.Hash, &r.Data); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Count() (int64, error) {
	var n int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&n)
	return n, err
}

// SeqRange returns min/max seq and count of events matching q.
func (s *Store) SeqRange(q Query) (min, max int64, n int64, err error) {
	w, args := q.where()
	var mn, mx sql.NullInt64
	err = s.db.QueryRow("SELECT MIN(seq), MAX(seq), COUNT(*) FROM events"+w, args...).Scan(&mn, &mx, &n)
	return mn.Int64, mx.Int64, n, err
}

// MinTS returns the earliest TS among events with seq > after (zero if none).
func (s *Store) MinTS(after int64) (time.Time, error) {
	var ms sql.NullInt64
	if err := s.db.QueryRow("SELECT MIN(ts) FROM events WHERE seq > ?", after).Scan(&ms); err != nil {
		return time.Time{}, err
	}
	if !ms.Valid {
		return time.Time{}, nil
	}
	return time.UnixMilli(ms.Int64).UTC(), nil
}

// SessionPrompts returns the session's last n prompt texts, oldest first.
func (s *Store) SessionPrompts(session string, n int) ([]string, error) {
	if session == "" {
		return nil, nil
	}
	rows, err := s.db.Query("SELECT data FROM events WHERE session=? AND kind='prompt' ORDER BY seq DESC LIMIT ?", session, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var e struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(b, &e) == nil && e.Text != "" {
			out = append(out, e.Text)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// HasEvent reports whether an event with e's kind, harness, session, tool and
// millisecond timestamp is already stored. The spool uses it to avoid
// appending twice an event a timed-out hook did manage to commit.
func (s *Store) HasEvent(e event.Event) (bool, error) {
	ts := e.TS.UTC().Truncate(time.Millisecond).UnixMilli()
	rows, err := s.db.Query("SELECT data FROM events WHERE ts=? AND kind=?", ts, string(e.Kind))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return false, err
		}
		var got event.Event
		if json.Unmarshal(b, &got) != nil {
			continue
		}
		if got.Harness == e.Harness && got.SessionID == e.SessionID && got.Tool == e.Tool {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ---- meta / sync state ----

func (s *Store) GetMeta(k string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT v FROM meta WHERE k=?", k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) SetMeta(k, v string) error {
	_, err := s.db.Exec("INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", k, v)
	return err
}

type SyncState struct {
	Seq  int64     // last synced chain seq
	Time time.Time // when the last successful sync happened
}

func (s *Store) SyncState() (SyncState, error) {
	var st SyncState
	v, err := s.GetMeta("sync_seq")
	if err != nil {
		return st, err
	}
	st.Seq, _ = strconv.ParseInt(v, 10, 64)
	if v, _ = s.GetMeta("sync_ts"); v != "" {
		ms, _ := strconv.ParseInt(v, 10, 64)
		st.Time = time.UnixMilli(ms).UTC()
	}
	return st, nil
}

func (s *Store) SetSyncState(seq int64, t time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range map[string]string{"sync_seq": strconv.FormatInt(seq, 10), "sync_ts": strconv.FormatInt(t.UnixMilli(), 10)} {
		if _, err := tx.Exec("INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- projects ----

type Project struct {
	Key  string
	Name string
	// Label is what the project's folder goes by now (folders.go): the key
	// until a remote is added or changes. "" for an older key of a folder
	// that has since gone on under another.
	Label     string
	Remote    string // normalized, e.g. github.com/u/r
	Root      string
	FirstSeen time.Time
	LastSeen  time.Time
}

// TouchProject records that a project was seen now (cheap: updates at most
// once a minute per project).
func (s *Store) TouchProject(p Project, now time.Time) error {
	ms := now.UnixMilli()
	label := p.Label
	if label == "" {
		label = p.Key
	}
	if _, err := s.db.Exec("INSERT OR IGNORE INTO projects(key,name,remote,root,first_seen,last_seen,label) VALUES(?,?,?,?,?,?,?)",
		p.Key, p.Name, p.Remote, p.Root, ms, ms, label); err != nil {
		return err
	}
	_, err := s.db.Exec("UPDATE projects SET last_seen=?, root=CASE WHEN ?<>'' THEN ? ELSE root END, name=CASE WHEN ?<>'' THEN ? ELSE name END, remote=? WHERE key=? AND last_seen < ?",
		ms, p.Root, p.Root, p.Name, p.Name, p.Remote, p.Key, ms-60000)
	return err
}

func (s *Store) Projects(since time.Time) ([]Project, error) {
	rows, err := s.db.Query("SELECT key,name,remote,root,first_seen,last_seen,label FROM projects WHERE last_seen >= ? ORDER BY last_seen DESC", since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		var f, l int64
		if err := rows.Scan(&p.Key, &p.Name, &p.Remote, &p.Root, &f, &l, &p.Label); err != nil {
			return nil, err
		}
		p.FirstSeen, p.LastSeen = time.UnixMilli(f).UTC(), time.UnixMilli(l).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetProject(key string) (Project, bool, error) {
	ps, err := s.Projects(time.Time{})
	if err != nil {
		return Project{}, false, err
	}
	for _, p := range ps {
		if p.Key == key {
			return p, true, nil
		}
	}
	return Project{}, false, nil
}

// ProjectKeys lists distinct project keys present in events with counts.
func (s *Store) ProjectKeys() (map[string]int64, error) {
	rows, err := s.db.Query("SELECT project, COUNT(*) FROM events WHERE project<>'' GROUP BY project")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// ---- session project ----

func (s *Store) BumpSessionProject(session, project string, n int) error {
	_, err := s.db.Exec("INSERT INTO session_proj(session,project,writes) VALUES(?,?,?) ON CONFLICT(session,project) DO UPDATE SET writes=writes+excluded.writes", session, project, n)
	return err
}

// SessionProject returns the project with the most writes in the session.
func (s *Store) SessionProject(session string) (string, error) {
	var p string
	err := s.db.QueryRow("SELECT project FROM session_proj WHERE session=? ORDER BY writes DESC, project LIMIT 1", session).Scan(&p)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return p, err
}

// ---- commits ----

// MarkCommit records a commit as emitted; returns false if already seen.
func (s *Store) MarkCommit(sha, project string) (bool, error) {
	res, err := s.db.Exec("INSERT OR IGNORE INTO commits_seen(sha,project) VALUES(?,?)", sha, project)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// VerifyChain replays every stored event and checks leaf hashes, links and
// the JSON round trip. Returns the number of events checked.
func (s *Store) VerifyChain() (int64, error) {
	prev, h, err := s.Base() // before Query: the store has a single connection
	if err != nil {
		return 0, err
	}
	rows, err := s.db.Query("SELECT seq, leaf, hash, data FROM events ORDER BY seq")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int64
	var damaged []int64
	for rows.Next() {
		var seq int64
		var leaf, hash string
		var data []byte
		if err := rows.Scan(&seq, &leaf, &hash, &data); err != nil {
			return n, err
		}
		if seq != prev+1 {
			return n, fmt.Errorf("gap before seq %d", seq)
		}
		prev = seq
		var e event.Event
		if err := json.Unmarshal(data, &e); err != nil {
			// the record's content is damaged on disk; the stored leaf still
			// links the chain, so check the links and report the record
			damaged = append(damaged, seq)
			if h, err = chain.Link(h, leaf); err != nil || h != hash {
				return n, fmt.Errorf("seq %d: chain hash mismatch", seq)
			}
			n++
			continue
		}
		l, err := chain.LeafKey(e, s.key)
		if err != nil || l != leaf {
			return n, fmt.Errorf("seq %d: leaf mismatch", seq)
		}
		if h, err = chain.Link(h, leaf); err != nil || h != hash {
			return n, fmt.Errorf("seq %d: chain hash mismatch", seq)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	if len(damaged) > 0 {
		return n, &DamagedError{Seqs: damaged}
	}
	return n, nil
}

// DamagedError lists records whose content could not be read (disk damage).
// The chain still links through them: their content is lost, nothing else.
type DamagedError struct{ Seqs []int64 }

func (e *DamagedError) Error() string {
	return fmt.Sprintf("%d record(s) could not be read (first at seq %d); their content is lost, the chain still links", len(e.Seqs), e.Seqs[0])
}

// Forget replaces stored prompt text and shell commands with their SHA-256
// commitments (for one project, or all when project is ""). The hash chain
// commits to these fields only as hashes, so it still verifies. Returns how
// many records changed.
func (s *Store) Forget(project string) (int, error) {
	// The fingerprints of sent prompts (prompts_sent) stay: they hold no
	// text, and without them a prompt sent again would count as new.
	{ // the prompt check's turns hold text too (checks.go)
		q, args := "DELETE FROM check_turns", []any{}
		if project != "" {
			q, args = q+" WHERE project=?", []any{project}
		}
		if _, err := s.db.Exec(q, args...); err != nil {
			return 0, err
		}
	}
	q := "SELECT seq, data FROM events WHERE kind IN ('prompt','tool','run')"
	var args []any
	if project != "" {
		q += " AND project = ?"
		args = append(args, project)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return 0, err
	}
	type upd struct {
		seq  int64
		data []byte
	}
	var ups []upd
	for rows.Next() {
		var seq int64
		var b []byte
		if err := rows.Scan(&seq, &b); err != nil {
			rows.Close()
			return 0, err
		}
		var e event.Event
		if json.Unmarshal(b, &e) != nil {
			continue
		}
		h, err := s.Hide(e)
		if err != nil {
			rows.Close()
			return 0, err
		}
		if h.Text == e.Text && h.Command == e.Command {
			continue
		}
		nb, err := json.Marshal(h)
		if err != nil {
			rows.Close()
			return 0, err
		}
		ups = append(ups, upd{seq, nb})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, u := range ups {
		if _, err := tx.Exec("UPDATE events SET data=? WHERE seq=?", u.data, u.seq); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)") // don't leave old pages in the WAL
	return len(ups), nil
}
