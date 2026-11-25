package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"seatime/pkg/promptcheck"
)

// PromptVerdict is the stored verdict for one prompt hash of a device.
func (s *DB) PromptVerdict(ctx context.Context, deviceID, checkHash string) (*promptcheck.Verdict, error) {
	var labels, kind, model, rules, skipped string
	var pass int
	var minutes float64
	err := s.row(ctx, `SELECT labels,pass,kind,model,minutes,rules,skipped FROM prompt_verdicts WHERE device_id=? AND check_hash=?`, deviceID, checkHash).
		Scan(&labels, &pass, &kind, &model, &minutes, &rules, &skipped)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v := &promptcheck.Verdict{Pass: pass != 0, Kind: kind, Model: model, Minutes: minutes, Rules: rules}
	var raw map[string]*bool // a question not asked is null
	if err := json.Unmarshal([]byte(labels), &raw); err != nil {
		return nil, err
	}
	v.Labels = promptcheck.Labels{}
	for k, b := range raw {
		if b != nil {
			v.Labels[k] = *b
		}
	}
	if skipped != "" {
		v.Skipped = strings.Split(skipped, ",")
	}
	return v, nil
}

// PutPromptVerdict keeps the first verdict for a prompt hash: a check is
// judged once, and a retry gets the same answer.
func (s *DB) PutPromptVerdict(ctx context.Context, deviceID, checkHash string, v promptcheck.Verdict, promptTS, at time.Time, ev promptcheck.Evidence) error {
	evb, err := json.Marshal(ev) // numbers and prompt hashes: no text
	if err != nil {
		return err
	}
	raw := map[string]*bool{} // yes/no per question name, null when not asked: no text
	for k, b := range v.Labels {
		raw[k] = &b
	}
	for _, k := range v.Skipped {
		raw[k] = nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, `INSERT INTO prompt_verdicts(device_id,check_hash,labels,pass,kind,model,prompt_ts,created,minutes,rules,skipped,evidence) VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		deviceID, checkHash, string(b), b2i(v.Pass), v.Kind, v.Model, promptTS.UnixMilli(), at.Unix(), v.Minutes, v.Rules, strings.Join(v.Skipped, ","), string(evb))
	return err
}

// PromptPasses returns the decision, kind and minutes for each of hashes that has a verdict.
func (s *DB) PromptPasses(ctx context.Context, deviceID string, hashes []string) (map[string]PromptPass, error) {
	out := map[string]PromptPass{}
	for len(hashes) > 0 {
		n := min(len(hashes), 500)
		part := hashes[:n]
		hashes = hashes[n:]
		args := []any{deviceID}
		for _, h := range part {
			args = append(args, h)
		}
		rows, err := s.rows(ctx, `SELECT check_hash,pass,kind,minutes FROM prompt_verdicts WHERE device_id=? AND check_hash IN (?`+strings.Repeat(",?", n-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h string
			var p int
			var v PromptPass
			if err := rows.Scan(&h, &p, &v.Kind, &v.Minutes); err != nil {
				rows.Close()
				return nil, err
			}
			v.Pass = p != 0
			out[h] = v
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PassingPrompts are when this device's passing prompts in [from, to) were sent.
func (s *DB) PassingPrompts(ctx context.Context, deviceID string, from, to time.Time) ([]PromptTime, error) {
	rows, err := s.rows(ctx, `SELECT prompt_ts,kind,minutes FROM prompt_verdicts WHERE device_id=? AND pass=1 AND prompt_ts>=? AND prompt_ts<? ORDER BY prompt_ts`,
		deviceID, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PromptTime
	for rows.Next() {
		var ms int64
		var p PromptTime
		if err := rows.Scan(&ms, &p.Kind, &p.Minutes); err != nil {
			return nil, err
		}
		p.TS = time.UnixMilli(ms).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPending replaces a device's report of time waiting for the check.
func (s *DB) SetPending(ctx context.Context, userID int64, deviceID string, byProject map[string]float64, at time.Time) error {
	if _, err := s.exec(ctx, `DELETE FROM pending_time WHERE device_id=?`, deviceID); err != nil {
		return err
	}
	for k, sec := range byProject {
		if sec <= 0 {
			continue
		}
		if _, err := s.exec(ctx, `INSERT INTO pending_time(device_id,user_id,project_key,seconds,at) VALUES (?,?,?,?,?)
			ON CONFLICT(device_id,project_key) DO UPDATE SET user_id=excluded.user_id, seconds=excluded.seconds, at=excluded.at`,
			deviceID, userID, k, sec, at.Unix()); err != nil {
			return err
		}
	}
	return nil
}

// Pending sums a user's time waiting for the check reported since a time.
func (s *DB) Pending(ctx context.Context, userID, projectID int64, since time.Time) (float64, error) {
	q, args := `SELECT COALESCE(SUM(seconds),0) FROM pending_time WHERE user_id=? AND at>=?`, []any{userID, since.Unix()}
	if projectID != 0 {
		q = `SELECT COALESCE(SUM(t.seconds),0) FROM pending_time t JOIN project_keys k ON k.user_id=t.user_id AND k.device_id=t.device_id AND k.key=t.project_key
WHERE t.user_id=? AND t.at>=? AND k.project_id=?`
		args = append(args, projectID)
	}
	var sec float64
	err := s.row(ctx, q, args...).Scan(&sec)
	return sec, err
}

// PendingAll sums the time every device reported waiting for the check
// since a time, and how many accounts it belongs to (shown on /admin).
func (s *DB) PendingAll(ctx context.Context, since time.Time) (seconds float64, users int, err error) {
	err = s.row(ctx, `SELECT COALESCE(SUM(seconds),0), COUNT(DISTINCT user_id) FROM pending_time WHERE at>=?`, since.Unix()).Scan(&seconds, &users)
	return seconds, users, err
}

// AddCheckSpend counts one paid judge call of a user on day (YYYY-MM-DD,
// UTC) and what it cost.
func (s *DB) AddCheckSpend(ctx context.Context, day string, userID int64, cost float64, failed bool) error {
	_, err := s.exec(ctx, `INSERT INTO check_spend(day,user_id,calls,fails,cost) VALUES(?,?,1,?,?)
  ON CONFLICT(day,user_id) DO UPDATE SET calls=check_spend.calls+1, fails=check_spend.fails+excluded.fails, cost=check_spend.cost+excluded.cost`, day, userID, b2i(failed), cost)
	return err
}

// CheckSpend is a user's paid judge calls on day.
func (s *DB) CheckSpend(ctx context.Context, day string, userID int64) (int, error) {
	var n int
	err := s.row(ctx, `SELECT COALESCE(SUM(calls),0) FROM check_spend WHERE day=? AND user_id=?`, day, userID).Scan(&n)
	return n, err
}

// CheckSpendDay is every paid judge call on day: calls, failed calls and dollars.
func (s *DB) CheckSpendDay(ctx context.Context, day string) (calls, fails int, cost float64, err error) {
	err = s.row(ctx, `SELECT COALESCE(SUM(calls),0), COALESCE(SUM(fails),0), COALESCE(SUM(cost),0) FROM check_spend WHERE day=?`, day).Scan(&calls, &fails, &cost)
	return
}

// RulePeriods are the prompt check switch's history, oldest first.
func (s *DB) RulePeriods(ctx context.Context) ([]promptcheck.Period, error) {
	rows, err := s.rows(ctx, `SELECT from_ms,on_ FROM rule_periods ORDER BY from_ms`)
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

// AddRulePeriod records that the switch is on or off from a time on.
func (s *DB) AddRulePeriod(ctx context.Context, p promptcheck.Period, by string) error {
	_, err := s.exec(ctx, `INSERT INTO rule_periods(from_ms,on_,by) VALUES(?,?,?) ON CONFLICT(from_ms) DO UPDATE SET on_=excluded.on_, by=excluded.by`, p.From.UnixMilli(), b2i(p.On), by)
	return err
}
