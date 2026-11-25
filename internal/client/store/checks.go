package store

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"seatime/pkg/promptcheck"
)

// The prompt check (pkg/promptcheck). check_turns holds every prompt the
// device records, with the check on or off: the prompt, the AI's reply
// and the turn's change excerpt, so that a finished turn can be sent for
// its check with the turns before it as context, now or later (when the
// account turns the check back on, or the server asks for it). It never
// leaves the machine except inside a check request, and a prompt's text
// is never dropped. A turn is "open" until it ends, then "queued" (a new
// prompt, waiting to be sent) or "context" (a repeat: never sent on its
// own), then "done". check_verdicts holds the answers by prompt hash, and
// prompt_chain every prompt's hash in chain order (chain.go).
const checkSchema = `
CREATE TABLE IF NOT EXISTS check_turns(
  id         INTEGER PRIMARY KEY,
  session    TEXT NOT NULL,
  project    TEXT NOT NULL,
  prompt_ts  INTEGER NOT NULL,
  check_hash TEXT NOT NULL DEFAULT '',
  salt       TEXT NOT NULL DEFAULT '',
  prompt     TEXT NOT NULL,
  prev_reply TEXT NOT NULL DEFAULT '',
  reply      TEXT NOT NULL DEFAULT '',
  start      TEXT NOT NULL DEFAULT '',
  diff       TEXT NOT NULL DEFAULT '',
  git_ins    INTEGER NOT NULL DEFAULT 0,
  git_del    INTEGER NOT NULL DEFAULT 0,
  git_known  INTEGER NOT NULL DEFAULT 0,
  stop_ts    INTEGER NOT NULL DEFAULT 0,
  state      TEXT NOT NULL,
  tries      INTEGER NOT NULL DEFAULT 0,
  next_try   INTEGER NOT NULL DEFAULT 0,
  rep        INTEGER NOT NULL DEFAULT 0,
  info       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS check_turns_session ON check_turns(session, prompt_ts);
CREATE INDEX IF NOT EXISTS check_turns_state ON check_turns(state, next_try);
CREATE INDEX IF NOT EXISTS check_turns_project ON check_turns(project, prompt_ts);
CREATE TABLE IF NOT EXISTS check_verdicts(
  check_hash TEXT PRIMARY KEY,
  pass       INTEGER NOT NULL,
  kind       TEXT NOT NULL DEFAULT '',
  labels     TEXT NOT NULL,
  model      TEXT NOT NULL,
  minutes    REAL NOT NULL DEFAULT 0,
  rules      TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
-- the prompt chain (pkg/promptcheck chain.go): one entry per prompt, sent
-- to the server at once; sent=1 once the server took it
CREATE TABLE IF NOT EXISTS prompt_chain(
  seq         INTEGER PRIMARY KEY,
  ts          INTEGER NOT NULL,
  project     TEXT NOT NULL,
  commit_hash TEXT NOT NULL UNIQUE,
  rep         INTEGER NOT NULL DEFAULT 0,
  link        TEXT NOT NULL,
  sent        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS prompt_chain_ts ON prompt_chain(ts);
`

// Turn states.
const (
	TurnOpen    = "open"
	TurnQueued  = "queued"
	TurnContext = "context"
	TurnDone    = "done"
	// TurnExpired: a turn an older SeaTime gave up on after the server's
	// queue limit, dropping its text. Its prompt stays pending (never
	// decided, never counted as failed). No turn expires now.
	TurnExpired = "expired"
)

// CheckTurn is one prompt and what followed it.
type CheckTurn struct {
	ID       int64
	Session  string
	Project  string
	PromptTS time.Time
	// CheckHash is the prompt's hash (promptcheck.Hash with Salt); a
	// repeat has one too, for the chain, but is context only.
	CheckHash string
	Salt      string
	Repeat    bool
	Prompt    string
	PrevReply string
	Reply     string // set when the AI stopped; "" when interrupted
	Start     *promptcheck.Snapshot
	// Info is what the hook read in the project's folder when the prompt
	// was sent (its name, file names, README), for the check; nil when it
	// read nothing. Blanked with the reply and the change excerpt.
	Info     *promptcheck.Project
	Diff     string
	GitIns   int
	GitDel   int
	GitKnown bool
	StopTS   time.Time // zero: the AI did not stop on its own
	State    string
	Tries    int
	NextTry  time.Time
}

const turnCols = `id,session,project,prompt_ts,check_hash,salt,prompt,prev_reply,reply,start,diff,git_ins,git_del,git_known,stop_ts,state,tries,next_try,rep,info`

func scanTurn(sc interface{ Scan(...any) error }) (CheckTurn, error) {
	var t CheckTurn
	var pts, sts, nt int64
	var start, info string
	var known, rep int
	err := sc.Scan(&t.ID, &t.Session, &t.Project, &pts, &t.CheckHash, &t.Salt, &t.Prompt, &t.PrevReply, &t.Reply,
		&start, &t.Diff, &t.GitIns, &t.GitDel, &known, &sts, &t.State, &t.Tries, &nt, &rep, &info)
	if err != nil {
		return t, err
	}
	if info != "" {
		var p promptcheck.Project
		if json.Unmarshal([]byte(info), &p) == nil {
			t.Info = &p
		}
	}
	t.PromptTS = time.UnixMilli(pts).UTC()
	if sts != 0 {
		t.StopTS = time.UnixMilli(sts).UTC()
	}
	if nt != 0 {
		t.NextTry = time.UnixMilli(nt).UTC()
	}
	t.GitKnown, t.Repeat = known != 0, rep != 0
	if start != "" {
		var sn promptcheck.Snapshot
		if json.Unmarshal([]byte(start), &sn) == nil {
			t.Start = &sn
		}
	}
	return t, nil
}

// OpenTurn records a new turn (state open) and, when it has a prompt
// hash, the prompt's chain entry after the device's last (project is the
// server key the chain names), in one transaction.
func (s *Store) OpenTurn(t CheckTurn, project string) (int64, error) {
	start := ""
	if t.Start != nil {
		b, err := json.Marshal(t.Start)
		if err != nil {
			return 0, err
		}
		start = string(b)
	}
	info := ""
	if t.Info != nil {
		b, err := json.Marshal(t.Info)
		if err != nil {
			return 0, err
		}
		info = string(b)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO check_turns(session,project,prompt_ts,check_hash,salt,prompt,prev_reply,start,state,rep,info) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		t.Session, t.Project, t.PromptTS.UnixMilli(), t.CheckHash, t.Salt, t.Prompt, t.PrevReply, start, TurnOpen, b2i(t.Repeat), info)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if t.CheckHash != "" {
		seq, link := int64(0), promptcheck.ChainGenesis
		if err := tx.QueryRow(`SELECT seq,link FROM prompt_chain ORDER BY seq DESC LIMIT 1`).Scan(&seq, &link); err != nil && err != sql.ErrNoRows {
			return 0, err
		}
		e := promptcheck.ChainEntry{Seq: seq + 1, TS: t.PromptTS.UTC().Truncate(time.Millisecond), Project: project, Commit: t.CheckHash, Repeat: t.Repeat}
		e.Link = promptcheck.ChainLink(link, e)
		if _, err := tx.Exec(`INSERT INTO prompt_chain(seq,ts,project,commit_hash,rep,link) VALUES(?,?,?,?,?,?)`,
			e.Seq, e.TS.UnixMilli(), e.Project, e.Commit, b2i(e.Repeat), e.Link); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// LastTurn is the newest turn of a session, or nil.
func (s *Store) LastTurn(session string) (*CheckTurn, error) {
	t, err := scanTurn(s.db.QueryRow(`SELECT `+turnCols+` FROM check_turns WHERE session=? ORDER BY prompt_ts DESC, id DESC LIMIT 1`, session))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// CloseTurn ends a turn: the reply (empty when interrupted), the change
// excerpt, git's logged counts and when the AI stopped (zero when it did
// not). The start snapshot is dropped. A new prompt's turn is queued for
// its check; a repeat stays as context.
func (s *Store) CloseTurn(id int64, reply, diff string, gitIns, gitDel int, gitKnown bool, stop time.Time) error {
	var sts int64
	if !stop.IsZero() {
		sts = stop.UnixMilli()
	}
	_, err := s.db.Exec(`UPDATE check_turns SET reply=?, diff=?, git_ins=?, git_del=?, git_known=?, stop_ts=?, start='',
  state=CASE WHEN check_hash<>'' AND rep=0 THEN ? ELSE ? END WHERE id=? AND state=?`,
		reply, diff, gitIns, gitDel, b2i(gitKnown), sts, TurnQueued, TurnContext, id, TurnOpen)
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// DueChecks are queued turns whose next try is due, oldest first, at most
// limit of those want takes (nil: all).
func (s *Store) DueChecks(now time.Time, limit int, want func(CheckTurn) bool) ([]CheckTurn, error) {
	rows, err := s.db.Query(`SELECT `+turnCols+` FROM check_turns WHERE state=? AND next_try<=? ORDER BY prompt_ts, id`, TurnQueued, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckTurn
	for rows.Next() && len(out) < limit {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		if want == nil || want(t) {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

// TurnsBeforeProject are up to n turns of a project before t (any
// session), oldest first: the check's context is the project's turns, so a
// new chat does not hide the asks before it.
func (s *Store) TurnsBeforeProject(project string, t time.Time, id int64, n int) ([]CheckTurn, error) {
	rows, err := s.db.Query(`SELECT `+turnCols+` FROM check_turns WHERE project=? AND state<>? AND (prompt_ts<? OR prompt_ts=? AND id<?) ORDER BY prompt_ts DESC, id DESC LIMIT ?`,
		project, TurnExpired, t.UnixMilli(), t.UnixMilli(), id, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckTurn
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append([]CheckTurn{t}, out...)
	}
	return out, rows.Err()
}

// CheckedBefore are up to n turns of a project before t whose prompt
// was checked, not among skip (ids), oldest first.
func (s *Store) CheckedBefore(project string, t time.Time, skip []int64, n int) ([]CheckTurn, error) {
	q := `SELECT ` + turnCols + ` FROM check_turns WHERE project=? AND prompt_ts<? AND state=? AND check_hash IN (SELECT check_hash FROM check_verdicts WHERE model NOT LIKE 'closed by the server%' AND model NOT LIKE 'refused by the server%')`
	args := []any{project, t.UnixMilli(), TurnDone}
	for _, id := range skip {
		q += ` AND id<>?`
		args = append(args, id)
	}
	rows, err := s.db.Query(q+` ORDER BY prompt_ts DESC, id DESC LIMIT ?`, append(args, n)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckTurn
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append([]CheckTurn{t}, out...)
	}
	return out, rows.Err()
}

// TurnsBefore are up to n turns of a session before t, oldest first.
func (s *Store) TurnsBefore(session string, t time.Time, id int64, n int) ([]CheckTurn, error) {
	rows, err := s.db.Query(`SELECT `+turnCols+` FROM check_turns WHERE session=? AND (prompt_ts<? OR prompt_ts=? AND id<?) ORDER BY prompt_ts DESC, id DESC LIMIT ?`,
		session, t.UnixMilli(), t.UnixMilli(), id, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckTurn
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append([]CheckTurn{t}, out...)
	}
	return out, rows.Err()
}

// RetryCheck puts a queued turn's next try at next.
func (s *Store) RetryCheck(id int64, next time.Time) error {
	_, err := s.db.Exec(`UPDATE check_turns SET tries=tries+1, next_try=? WHERE id=?`, next.UnixMilli(), id)
	return err
}

// CheckDone stores a verdict and marks the turn done. A device keeps the
// decision and the minutes only: the kind and labels columns stay empty.
func (s *Store) CheckDone(id int64, hash string, v promptcheck.Verdict) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR REPLACE INTO check_verdicts(check_hash,pass,kind,labels,model,minutes,rules) VALUES(?,?,'','{}',?,?,?)`, hash, b2i(v.Pass), v.Model, v.Minutes, v.Rules); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE check_turns SET state=? WHERE id=?`, TurnDone, id); err != nil {
		return err
	}
	return tx.Commit()
}

// CheckPasses are the decisions of every verdict on this machine.
func (s *Store) CheckPasses() (map[string]bool, error) {
	p, _, err := s.CheckVerdicts()
	return p, err
}

// CheckVerdicts are the decision and minutes (0: not sent by the server)
// of every verdict on this machine.
func (s *Store) CheckVerdicts() (pass map[string]bool, minutes map[string]float64, err error) {
	rows, err := s.db.Query(`SELECT check_hash,pass,minutes FROM check_verdicts`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	pass, minutes = map[string]bool{}, map[string]float64{}
	for rows.Next() {
		var h string
		var p int
		var m float64
		if err := rows.Scan(&h, &p, &m); err != nil {
			return nil, nil, err
		}
		pass[h] = p != 0
		if m > 0 {
			minutes[h] = m
		}
	}
	return pass, minutes, rows.Err()
}

// PruneTurns keeps every prompt: no turn is deleted, and no prompt's text
// or salt is dropped, so the server can ask for any of them later. It
// only blanks the AI's replies, change excerpts and project facts of turns
// that are already done or context and older than keep, except the newest
// promptcheck.MaxContextTurns+1 of each project (the context of the next
// check). A turn still waiting for its check keeps everything.
func (s *Store) PruneTurns(now time.Time, keep time.Duration) error {
	_, err := s.db.Exec(`UPDATE check_turns SET prev_reply='', reply='', diff='', start='', info=''
  WHERE state IN (?,?) AND prompt_ts<? AND (reply<>'' OR diff<>'' OR prev_reply<>'' OR start<>'' OR info<>'')
  AND id NOT IN (SELECT id FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY project ORDER BY prompt_ts DESC, id DESC) AS rn FROM check_turns WHERE state<>?) WHERE rn<=?)`,
		TurnDone, TurnContext, now.Add(-keep).UnixMilli(), TurnExpired, promptcheck.MaxContextTurns+1)
	return err
}

// ExpiredChecks counts turns whose wait ended without a verdict and whose
// text was dropped (only an older SeaTime did that).
func (s *Store) ExpiredChecks() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM check_turns WHERE state=?`, TurnExpired).Scan(&n)
	return n, err
}

// QueuedChecks counts turns waiting to be sent.
func (s *Store) QueuedChecks() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM check_turns WHERE state=?`, TurnQueued).Scan(&n)
	return n, err
}

// MetaPromptCheck is the meta key holding "1" while the server's prompt
// check is on (from the last sync answer).
const MetaPromptCheck = "prompt_check"

// PromptCheckOn reports whether the last sync said the prompt check is on.
func (s *Store) PromptCheckOn() bool {
	v, _ := s.GetMeta(MetaPromptCheck)
	return v == "1"
}

// MetaQueueDays and MetaRecountDays hold the server's limits (from the
// last sync answer): how long a turn may wait for its check, and how far
// back a late verdict may recount synced time.
const (
	MetaQueueDays   = "check_queue_days"
	MetaRecountDays = "check_recount_days"
)

// MetaDays reads one of those limits, or def when the server sent none.
func (s *Store) MetaDays(key string, def time.Duration) time.Duration {
	v, _ := s.GetMeta(key)
	if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
		return time.Duration(n * float64(24*time.Hour))
	}
	return def
}

// MetaEarlier holds how many earlier prompts that passed a check request
// carries (the server's setting prompt_check_earlier).
const MetaEarlier = "check_earlier"

// MetaInt reads a whole number the server sent, or def when it sent none.
func (s *Store) MetaInt(key string, def int) int {
	v, _ := s.GetMeta(key)
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return n
	}
	return def
}

// MetaCheckPeriods holds the server's switch history (JSON of
// []promptcheck.Period, from the last sync answer).
const MetaCheckPeriods = "check_periods"

// CheckPeriods is the switch history the server last sent (nil: none, an
// older server: the switch applies to all time).
func (s *Store) CheckPeriods() []promptcheck.Period {
	v, _ := s.GetMeta(MetaCheckPeriods)
	if v == "" {
		return nil
	}
	var ps []promptcheck.Period
	if json.Unmarshal([]byte(v), &ps) != nil {
		return nil
	}
	return ps
}

// MetaFrontChain holds the process ids of the AI tool's own window chain
// (presence.Probe.Chain), as of the last prompt: the daemon's 5 s front
// check compares the front window with them. Process ids only; local.
const MetaFrontChain = "front_chain"

// JoinPIDs and SplitPIDs store a list of process ids.
func JoinPIDs(pids []int) string {
	parts := make([]string, len(pids))
	for i, p := range pids {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}

func SplitPIDs(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(f); err == nil && n > 1 {
			out = append(out, n)
		}
	}
	return out
}

// MetaResyncFrom holds the earliest time (unix ms) a verdict that came
// back since the last sync can change: the next sync recounts from there.
const MetaResyncFrom = "resync_from"

// NoteResync asks the next sync to recount from t (the earliest asked wins).
func (s *Store) NoteResync(t time.Time) error {
	if cur, ok := s.ResyncFrom(); ok && !t.Before(cur) {
		return nil
	}
	return s.SetMeta(MetaResyncFrom, strconv.FormatInt(t.UnixMilli(), 10))
}

// ResyncFrom is the time NoteResync asked for, if any.
func (s *Store) ResyncFrom() (time.Time, bool) {
	v, _ := s.GetMeta(MetaResyncFrom)
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(ms).UTC(), true
}

// ClearResync drops the request if it is still t (a newer one stays).
func (s *Store) ClearResync(t time.Time) error {
	if cur, ok := s.ResyncFrom(); ok && cur.Equal(t) {
		return s.SetMeta(MetaResyncFrom, "")
	}
	return nil
}
