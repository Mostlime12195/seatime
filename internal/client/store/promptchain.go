package store

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	"seatime/pkg/promptcheck"
)

// The device's prompt chain (pkg/promptcheck chain.go), what the server
// last said about the account's own setting, its requests for saved
// prompts, and the prompts it closed without a verdict.

// ChainHead is the device's last chain entry (seq 0 and
// promptcheck.ChainGenesis when it has none).
func (s *Store) ChainHead() (int64, string, error) {
	seq, link := int64(0), promptcheck.ChainGenesis
	err := s.db.QueryRow(`SELECT seq,link FROM prompt_chain ORDER BY seq DESC LIMIT 1`).Scan(&seq, &link)
	if err == sql.ErrNoRows {
		err = nil
	}
	return seq, link, err
}

// UnsentChain are up to n entries the server has not taken yet, in order.
func (s *Store) UnsentChain(n int) ([]promptcheck.ChainEntry, error) {
	rows, err := s.db.Query(`SELECT seq,ts,project,commit_hash,rep,link FROM prompt_chain WHERE sent=0 ORDER BY seq LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []promptcheck.ChainEntry
	for rows.Next() {
		var e promptcheck.ChainEntry
		var ts int64
		var rep int
		if err := rows.Scan(&e.Seq, &ts, &e.Project, &e.Commit, &rep, &e.Link); err != nil {
			return nil, err
		}
		e.TS, e.Repeat = time.UnixMilli(ts).UTC(), rep != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkChainSent marks the entries up to seq as taken by the server.
func (s *Store) MarkChainSent(seq int64) error {
	_, err := s.db.Exec(`UPDATE prompt_chain SET sent=1 WHERE seq<=? AND sent=0`, seq)
	return err
}

// RelinkUnsent puts the entries the server has not taken after the
// server's head (seq, link), renumbered and relinked (promptcheck.Relink):
// those never sent, and those past the server's head (it lost them). The
// server never saw them in that place, so nothing it holds changes. It is
// for a device whose store does not match the server's chain (lost, or
// restored from an older copy).
func (s *Store) RelinkUnsent(seq int64, link string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT seq,ts,project,commit_hash,rep,link FROM prompt_chain WHERE sent=0 OR seq>? ORDER BY seq`, seq)
	if err != nil {
		return err
	}
	var es []promptcheck.ChainEntry
	for rows.Next() {
		var e promptcheck.ChainEntry
		var ts int64
		var rep int
		if err := rows.Scan(&e.Seq, &ts, &e.Project, &e.Commit, &rep, &e.Link); err != nil {
			rows.Close()
			return err
		}
		e.TS, e.Repeat = time.UnixMilli(ts).UTC(), rep != 0
		es = append(es, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM prompt_chain WHERE sent=0 OR seq>?`, seq); err != nil {
		return err
	}
	for _, e := range promptcheck.Relink(seq, link, es) {
		if _, err := tx.Exec(`INSERT INTO prompt_chain(seq,ts,project,commit_hash,rep,link) VALUES(?,?,?,?,?,?)`,
			e.Seq, e.TS.UnixMilli(), e.Project, e.Commit, b2i(e.Repeat), e.Link); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ChainFrom is when the device's first chained prompt was sent (zero: none).
func (s *Store) ChainFrom() time.Time {
	var ms sql.NullInt64
	if err := s.db.QueryRow(`SELECT MIN(ts) FROM prompt_chain`).Scan(&ms); err != nil || !ms.Valid {
		return time.Time{}
	}
	return time.UnixMilli(ms.Int64).UTC()
}

// ChainResolve counts the chained prompts in [from, to) that are not
// repeats, and how many have a verdict (promptcheck.Resolver).
func (s *Store) ChainResolve(from, to time.Time) (n, settled int) {
	_ = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN v.check_hash IS NOT NULL THEN 1 ELSE 0 END),0)
  FROM prompt_chain c LEFT JOIN check_verdicts v ON v.check_hash=c.commit_hash
  WHERE c.rep=0 AND c.ts>=? AND c.ts<?`, from.UnixMilli(), to.UnixMilli()).Scan(&n, &settled)
	return n, settled
}

// CloseByServer stores a failing verdict for a prompt the server closed
// without one (its text did not match, or never came), and marks its
// turn done, so both count it the same way.
func (s *Store) CloseByServer(hash, status string) error {
	var have int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM check_verdicts WHERE check_hash=?`, hash).Scan(&have); err != nil {
		return err
	}
	if have > 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO check_verdicts(check_hash,pass,kind,labels,model,minutes,rules) VALUES(?,0,'','{}',?,0,'')`, hash, "closed by the server ("+status+")"); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE check_turns SET state=? WHERE check_hash=? AND state IN (?,?)`, TurnDone, hash, TurnQueued, TurnOpen); err != nil {
		return err
	}
	return tx.Commit()
}

// What the last sync answer said about the prompt chain (api.PromptChainResp).
const (
	MetaChain         = "prompt_chain"   // "1": the server takes prompt hashes
	MetaServerCheck   = "server_check"   // "1": the server's own switch is on
	MetaServerPeriods = "server_periods" // JSON []promptcheck.Period
	MetaUserPeriods   = "user_periods"   // JSON []promptcheck.Period
	MetaPulls         = "prompt_pulls"   // JSON []Pull
	MetaChecksPaused  = "checks_paused"  // unix ms: no check is sent before
)

// ChainOn reports whether the server takes prompt hashes.
func (s *Store) ChainOn() bool {
	v, _ := s.GetMeta(MetaChain)
	return v == "1"
}

// Pull is a range the server asked for (api.PromptPull, kept here to keep
// the store free of the wire package).
type Pull struct {
	Project string    `json:"project,omitempty"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
}

// Periods reads a JSON []promptcheck.Period meta value.
func (s *Store) Periods(key string) []promptcheck.Period {
	v, _ := s.GetMeta(key)
	var ps []promptcheck.Period
	if v == "" || json.Unmarshal([]byte(v), &ps) != nil {
		return nil
	}
	return ps
}

// PutJSON stores v as JSON under key.
func (s *Store) PutJSON(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.SetMeta(key, string(b))
}

// Pulls are the server's open requests for saved prompts.
func (s *Store) Pulls() []Pull {
	v, _ := s.GetMeta(MetaPulls)
	var ps []Pull
	if v == "" || json.Unmarshal([]byte(v), &ps) != nil {
		return nil
	}
	return ps
}

// PauseChecks holds every check until t (the server said to wait).
func (s *Store) PauseChecks(t time.Time) error {
	return s.SetMeta(MetaChecksPaused, strconv.FormatInt(t.UnixMilli(), 10))
}

// ChecksPaused reports whether checks are held at now.
func (s *Store) ChecksPaused(now time.Time) bool {
	v, _ := s.GetMeta(MetaChecksPaused)
	ms, err := strconv.ParseInt(v, 10, 64)
	return err == nil && now.UnixMilli() < ms
}

// GetJSON reads a JSON meta value into v (left as is when unset).
func (s *Store) GetJSON(key string, v any) error {
	b, err := s.GetMeta(key)
	if err != nil || b == "" {
		return err
	}
	return json.Unmarshal([]byte(b), v)
}
