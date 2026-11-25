package store

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"seatime/pkg/ledger"
)

// Counted time and prompts by AI model. A device's bucket names the models
// behind its AI seconds and prompts (ledger.Bucket.Models, the buckets.models
// column); what is left of them has no model recorded, and hand time never
// has one. hourly_models is the rollup the pages read, kept by UpsertBuckets
// and DeleteBuckets next to hourly: per user, project and hour its rows add
// up to hourly.struggle. segment_models is the same split of each checked
// segment's credited minutes: what applications read.

// ModelSum is counted seconds and prompts with one model.
type ModelSum struct {
	// Hand: time from hand edits, which has no model. With Hand false and
	// Model "", AI time and prompts whose records name no model.
	Hand    bool
	Model   string // as the AI tool reported it
	Seconds float64
	Prompts int
}

type mkey struct {
	hand  bool
	model string
}

type mval struct {
	sec     float64
	prompts int
}

// encodeModels is the buckets.models column: JSON, sorted by model, "" for none.
func encodeModels(ms []ledger.ModelUse) string {
	var keep []ledger.ModelUse
	for _, m := range ms {
		if m.Model != "" && (m.Seconds > 0 || m.Prompts > 0) {
			keep = append(keep, m)
		}
	}
	if len(keep) == 0 {
		return ""
	}
	slices.SortStableFunc(keep, func(a, b ledger.ModelUse) int { return strings.Compare(a.Model, b.Model) })
	b, _ := json.Marshal(keep)
	return string(b)
}

func decodeModels(s string) []ledger.ModelUse {
	if s == "" {
		return nil
	}
	var ms []ledger.ModelUse
	json.Unmarshal([]byte(s), &ms)
	return ms
}

// mergeModels splits a device-merged minute (mergeRows) by model. Its
// counted seconds are those of the device that counted most, so its split
// is that device's: hand seconds, seconds per named model, and the rest of
// its AI seconds with no model. Prompts add up over devices. The seconds
// sum to mergeRows' struggle and the prompts to its prompts.
func mergeModels(rs []brow) map[mkey]mval {
	out := map[mkey]mval{}
	add := func(k mkey, sec float64, prompts int) {
		if sec <= 0 && prompts <= 0 {
			return
		}
		v := out[k]
		v.sec += sec
		v.prompts += prompts
		out[k] = v
	}
	top := -1
	for i, r := range rs {
		if top < 0 || r.struggle > rs[top].struggle || r.struggle == rs[top].struggle && r.dev < rs[top].dev {
			top = i
		}
		left := max(r.prompts, 0)
		for _, m := range decodeModels(r.models) {
			n := min(max(m.Prompts, 0), left)
			add(mkey{model: m.Model}, 0, n)
			left -= n
		}
		add(mkey{}, 0, left)
	}
	if top < 0 {
		return out
	}
	t := rs[top]
	ai := min(max(t.agent, 0), max(t.struggle, 0))
	add(mkey{hand: true}, t.struggle-ai, 0)
	ms := decodeModels(t.models)
	sum := 0.0
	for _, m := range ms {
		sum += max(m.Seconds, 0)
	}
	f := 1.0
	if sum > ai && sum > 0 {
		f = ai / sum
	}
	named := 0.0
	for _, m := range ms {
		sec := max(m.Seconds, 0) * f
		add(mkey{model: m.Model}, sec, 0)
		named += sec
	}
	add(mkey{}, ai-named, 0)
	return out
}

type mhkey struct {
	pid, hour int64
	mkey
}

// modelDeltas adds what a minute's change (old rows to new rows) does to
// its hour's rows of hourly_models.
func modelDeltas(d map[mhkey]mval, pid, min int64, old, new []brow) {
	o, n := mergeModels(old), mergeModels(new)
	for k, v := range n {
		hk := mhkey{pid, min / 60, k}
		x := d[hk]
		x.sec += v.sec - o[k].sec
		x.prompts += v.prompts - o[k].prompts
		d[hk] = x
	}
	for k, v := range o {
		if _, ok := n[k]; ok {
			continue
		}
		hk := mhkey{pid, min / 60, k}
		x := d[hk]
		x.sec -= v.sec
		x.prompts -= v.prompts
		d[hk] = x
	}
}

func applyModelDeltas(ctx context.Context, tx *txn, uid int64, d map[mhkey]mval) error {
	for k, v := range d {
		if v.sec == 0 && v.prompts == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO hourly_models(user_id,project_id,hour,hand,model,seconds,prompts) VALUES (?,?,?,?,?,?,?)
ON CONFLICT(user_id,project_id,hour,hand,model) DO UPDATE SET seconds=hourly_models.seconds+excluded.seconds, prompts=hourly_models.prompts+excluded.prompts`,
			uid, k.pid, k.hour, b2i(k.hand), k.model, v.sec, v.prompts); err != nil {
			return err
		}
	}
	return nil
}

func scanModelSums(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}) ([]ModelSum, error) {
	defer rows.Close()
	var out []ModelSum
	for rows.Next() {
		var m ModelSum
		var hand int
		if err := rows.Scan(&hand, &m.Model, &m.Seconds, &m.Prompts); err != nil {
			return nil, err
		}
		m.Hand = hand == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// ModelSums are a user's counted seconds and prompts by model in the hours
// of [from, to), for one project or for all (0): the same hours ProjectSums
// adds up, so the seconds sum to its struggle.
func (s *DB) ModelSums(ctx context.Context, uid, pid int64, from, to time.Time) ([]ModelSum, error) {
	q := `SELECT hand, model, SUM(seconds), SUM(prompts) FROM hourly_models WHERE user_id=? AND hour>=? AND hour<?`
	args := []any{uid, from.Unix() / 3600, to.Unix()/3600 + 1}
	if pid != 0 {
		q += ` AND project_id=?`
		args = append(args, pid)
	}
	rows, err := s.rows(ctx, q+` GROUP BY hand, model ORDER BY hand, model`, args...)
	if err != nil {
		return nil, err
	}
	return scanModelSums(rows)
}

// VerifiedModels splits a project's credited minutes in [from, to) from
// verified segments (VerifiedMinutes) by model. Segments checked before
// models were kept have no rows here.
func (s *DB) VerifiedModels(ctx context.Context, pid int64, from, to time.Time) ([]ModelSum, error) {
	rows, err := s.rows(ctx, `SELECT m.hand, m.model, SUM(m.seconds), SUM(m.prompts) FROM segment_models m JOIN segments s ON s.id = m.segment_id
WHERE m.project_id=? AND s.status='verified' AND m.start_min>=? AND m.start_min<? GROUP BY m.hand, m.model ORDER BY m.hand, m.model`,
		pid, from.Unix()/60, (to.Unix()+59)/60)
	if err != nil {
		return nil, err
	}
	return scanModelSums(rows)
}
