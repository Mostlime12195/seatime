package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"seatime/pkg/api"
)

// Which project is this. A project is a folder on someone's computer. The
// computer names its projects by key and keeps a folder's key for good,
// whatever happens to the folder's git remote (internal/client/store
// folders.go). The remote is a label: what the folder goes by now, in the
// form of a key. Until a remote is added or changes, the label is the key.
//
// The server holds one rule, DeviceProject:
//
//   - A key a device has sent before belongs to the project it belonged to
//     then. That never changes, so a record's project is the same on every
//     replay.
//   - A key a device sends for the first time joins the project that goes
//     by the key's label now (projects.key): the same repo on a second
//     computer, or in a second folder. With no such project it starts one.
//   - When a device's label for a key changes, the project goes by the new
//     label from then on, unless another project already does. Then both
//     stay as they are: no project takes in another's time.
//
// project_keys is the map, one row per device and key. A key sent with no
// label (a segment, or a device from before labels, whose key changes with
// the remote) says nothing new about a key that is known, and is its own
// label when it is new.

// ProjectKey is one key a device has sent for a project.
type ProjectKey struct {
	DeviceID string
	Key      string
	Label    string
}

// DeviceProject returns the project a device's key belongs to, making the
// project or the key's row when the key is new, and follows the label.
func (s *DB) DeviceProject(ctx context.Context, uid int64, deviceID string, m api.ProjectMeta) (*Project, error) {
	if m.Key == "" || len(m.Key) > 300 || len(m.Label) > 300 {
		return nil, errors.New("bad project key")
	}
	// every sync repeats its project metas: no write when nothing changed
	var pid int64
	var label string
	if err := s.row(ctx, `SELECT project_id,label FROM project_keys WHERE user_id=? AND device_id=? AND key=?`, uid, deviceID, m.Key).Scan(&pid, &label); err == nil && (m.Label == "" || m.Label == label) {
		if p, err := s.ProjectByID(ctx, pid); err == nil && (p.Key != label || !takesName(p, m) && (m.Remote == "" || m.Remote == p.Remote)) {
			return p, nil
		}
	}
	var p *Project
	err := s.tx(ctx, func(tx *txn) (err error) { p, err = deviceProject(ctx, tx, uid, deviceID, m); return })
	return p, err
}

func deviceProject(ctx context.Context, tx *txn, uid int64, deviceID string, m api.ProjectMeta) (*Project, error) {
	var pid int64
	var label string
	switch err := tx.QueryRowContext(ctx, `SELECT project_id,label FROM project_keys WHERE user_id=? AND device_id=? AND key=?`, uid, deviceID, m.Key).Scan(&pid, &label); {
	case errors.Is(err, sql.ErrNoRows):
		// a key new to this device: the project that goes by its label now
		if m.Label == "" {
			m.Label = m.Key
		}
		p, err := upsertProject(ctx, tx, uid, api.ProjectMeta{Key: m.Label, Name: m.Name, Folder: m.Folder, Remote: m.Remote})
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO project_keys(user_id,device_id,key,project_id,label,created) VALUES (?,?,?,?,?,?)`,
			uid, deviceID, m.Key, p.ID, m.Label, time.Now().Unix())
		return p, err
	case err != nil:
		return nil, err
	}
	p, err := scanProject(tx.QueryRowContext(ctx, `SELECT `+projCols+` FROM projects WHERE id=?`, pid))
	if err != nil {
		return nil, err
	}
	if m.Label == "" {
		m.Label = label
	}
	if label != m.Label {
		// the folder's remote was added or changed
		if _, err := tx.ExecContext(ctx, `UPDATE project_keys SET label=? WHERE user_id=? AND device_id=? AND key=?`, m.Label, uid, deviceID, m.Key); err != nil {
			return nil, err
		}
		var taken int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE user_id=? AND key=?`, uid, m.Label).Scan(&taken); err != nil {
			return nil, err
		}
		if taken == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE projects SET key=? WHERE id=?`, m.Label, p.ID); err != nil {
				return nil, err
			}
			// the key it went by still leads to it
			if _, err := tx.ExecContext(ctx, `INSERT INTO project_links(user_id,name,project_id) VALUES (?,?,?)
ON CONFLICT(user_id,name) DO UPDATE SET project_id=excluded.project_id`, uid, p.Key, p.ID); err != nil {
				return nil, err
			}
			p.Key = m.Label
			// and its repo link is the one it goes by now
			if repo := RepoFromRemote(m.Remote); repo != "" && repo != p.Repo {
				if _, err := tx.ExecContext(ctx, `UPDATE projects SET repo=? WHERE id=?`, repo, p.ID); err != nil {
					return nil, err
				}
			}
		}
	}
	if p.Key != m.Label {
		// the folder goes by another label than the project (an older
		// remote, or one another project has): its name and remote are
		// not the project's
		return p, nil
	}
	return upsertProject(ctx, tx, uid, api.ProjectMeta{Key: p.Key, Name: m.Name, Folder: m.Folder, Remote: m.Remote})
}

// ProjectKeys are the keys devices have sent for a project.
func (s *DB) ProjectKeys(ctx context.Context, projectID int64) ([]ProjectKey, error) {
	rows, err := s.rows(ctx, `SELECT device_id,key,label FROM project_keys WHERE project_id=? ORDER BY created, device_id, key`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectKey
	for rows.Next() {
		var k ProjectKey
		if err := rows.Scan(&k.DeviceID, &k.Key, &k.Label); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// foldProjects runs once, with migration 8. Before it, a device made a new
// key, and so a new project, each time a folder's remote was added or
// changed, and its commit scan recorded the folder's commits under each of
// them. So projects of one account that hold the same commit were one
// folder: they fold into the newest, which has the key the folder goes by
// now, with their time, checked segments, commits and keys. The others'
// slugs lead to it.
func foldProjects(ctx context.Context, tx *txn) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT p1.user_id, c1.project_id, c2.project_id FROM commits c1
JOIN commits c2 ON c2.sha=c1.sha AND c2.project_id>c1.project_id
JOIN projects p1 ON p1.id=c1.project_id JOIN projects p2 ON p2.id=c2.project_id AND p2.user_id=p1.user_id`)
	if err != nil {
		return err
	}
	parent := map[int64]int64{}
	user := map[int64]int64{}
	var find func(int64) int64
	find = func(x int64) int64 {
		if p, ok := parent[x]; ok && p != x {
			parent[x] = find(p)
			return parent[x]
		}
		parent[x] = x
		return x
	}
	for rows.Next() {
		var uid, a, b int64
		if err := rows.Scan(&uid, &a, &b); err != nil {
			rows.Close()
			return err
		}
		user[a], user[b] = uid, uid
		parent[find(a)] = find(b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	groups := map[int64][]int64{}
	for id := range user {
		groups[find(id)] = append(groups[find(id)], id)
	}
	roots := make([]int64, 0, len(groups))
	for r := range groups {
		roots = append(roots, r)
	}
	slices.Sort(roots)
	for _, r := range roots {
		ids := groups[r]
		slices.SortFunc(ids, func(a, b int64) int { return cmp.Compare(b, a) }) // the newest first
		if err := foldInto(ctx, tx, user[ids[0]], ids[0], ids[1:]); err != nil {
			return err
		}
	}
	return nil
}

// foldInto moves everything the projects in from hold into project to and
// removes them.
func foldInto(ctx context.Context, tx *txn, uid, to int64, from []int64) error {
	p, err := scanProject(tx.QueryRowContext(ctx, `SELECT `+projCols+` FROM projects WHERE id=?`, to))
	if err != nil {
		return err
	}
	type bk struct {
		min int64
		dev string
	}
	read := func(pid int64) (map[bk]brow, error) {
		rows, err := tx.QueryContext(ctx, `SELECT start_min,device_id,human,struggle,dark_flow,agent,prompts,edits,commits,runs,harnesses,models FROM buckets WHERE user_id=? AND project_id=?`, uid, pid)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[bk]brow{}
		for rows.Next() {
			var k bk
			var r brow
			if err := rows.Scan(&k.min, &r.dev, &r.human, &r.struggle, &r.dark, &r.agent, &r.prompts, &r.edits, &r.commits, &r.runs, &r.harn, &r.models); err != nil {
				return nil, err
			}
			k.dev = r.dev
			out[k] = r
		}
		return out, rows.Err()
	}
	var total float64 // counted seconds in the rollup before, over all of them
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(struggle),0) FROM hourly WHERE user_id=? AND project_id=?`, uid, to).Scan(&total); err != nil {
		return err
	}
	have, err := read(to)
	if err != nil {
		return err
	}
	changed := map[bk]bool{}
	var slugs []string
	for _, id := range from {
		f, err := scanProject(tx.QueryRowContext(ctx, `SELECT `+projCols+` FROM projects WHERE id=?`, id))
		if err != nil {
			return err
		}
		slugs = append(slugs, f.Slug)
		var t float64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(struggle),0) FROM hourly WHERE user_id=? AND project_id=?`, uid, id).Scan(&t); err != nil {
			return err
		}
		total += t
		theirs, err := read(id)
		if err != nil {
			return err
		}
		for k, r := range theirs {
			if o, ok := have[k]; ok {
				r = addRows(o, r)
			}
			have[k], changed[k] = r, true
		}
		for _, q := range []string{
			`DELETE FROM buckets WHERE user_id=? AND project_id=?`,
			`DELETE FROM hourly WHERE user_id=? AND project_id=?`,
			`DELETE FROM hourly_models WHERE user_id=? AND project_id=?`,
		} {
			if _, err := tx.ExecContext(ctx, q, uid, id); err != nil {
				return err
			}
		}
		// checked segments stay as they were checked; a commit both held
		// is held once
		for _, q := range []string{
			`UPDATE segments SET project_id=? WHERE project_id=?`,
			`UPDATE segment_minutes SET project_id=? WHERE project_id=?`,
			`UPDATE segment_models SET project_id=? WHERE project_id=?`,
			`UPDATE project_keys SET project_id=? WHERE project_id=?`,
			`UPDATE project_links SET project_id=? WHERE project_id=?`,
			`INSERT INTO commits(project_id,sha,ts,ins,del) SELECT ?,sha,ts,ins,del FROM commits WHERE project_id=? ON CONFLICT(project_id,sha) DO NOTHING`,
		} {
			if _, err := tx.ExecContext(ctx, q, to, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM commits WHERE project_id=?`, id); err != nil {
			return err
		}
		// prompts asked for one are not yet asked for the whole
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET pulled=0 WHERE id=? AND (SELECT pulled FROM projects WHERE id=?)=0`, to, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_links(user_id,name,project_id) VALUES (?,?,?)`, uid, f.Slug, to); err != nil {
			return err
		}
	}
	for k := range changed {
		r := have[k]
		if _, err := tx.ExecContext(ctx, `INSERT INTO buckets(user_id,project_id,start_min,device_id,human,struggle,dark_flow,agent,prompts,edits,commits,runs,harnesses,models)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(user_id,project_id,start_min,device_id) DO UPDATE SET human=excluded.human, struggle=excluded.struggle, dark_flow=excluded.dark_flow,
 agent=excluded.agent, prompts=excluded.prompts, edits=excluded.edits, commits=excluded.commits, runs=excluded.runs, harnesses=excluded.harnesses, models=excluded.models`,
			uid, to, k.min, k.dev, r.human, r.struggle, r.dark, r.agent, r.prompts, r.edits, r.commits, r.runs, r.harn, r.models); err != nil {
			return err
		}
	}
	// the hourly rollups of the whole, from its minutes
	for _, q := range []string{`DELETE FROM hourly WHERE user_id=? AND project_id=?`, `DELETE FROM hourly_models WHERE user_id=? AND project_id=?`} {
		if _, err := tx.ExecContext(ctx, q, uid, to); err != nil {
			return err
		}
	}
	byMin := map[int64][]brow{}
	for k, r := range have {
		byMin[k.min] = append(byMin[k.min], r)
	}
	hours := map[int64]*hdelta{}
	models := map[mhkey]mval{}
	for m, rs := range byMin {
		sort.Slice(rs, func(i, j int) bool { return rs[i].dev < rs[j].dev })
		d := hours[m/60]
		if d == nil {
			d = &hdelta{}
			hours[m/60] = d
		}
		n := mergeRows(rs)
		d.human += n.human
		d.struggle += n.struggle
		d.dark += n.dark
		d.prompts += n.prompts
		d.edits += n.edits
		d.commits += n.commits
		d.runs += n.runs
		total -= n.struggle
		modelDeltas(models, to, m, nil, rs)
	}
	for h, d := range hours {
		if _, err := tx.ExecContext(ctx, `INSERT INTO hourly(user_id,project_id,hour,human,struggle,dark_flow,prompts,edits,commits,runs) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			uid, to, h, d.human, d.struggle, d.dark, d.prompts, d.edits, d.commits, d.runs); err != nil {
			return err
		}
	}
	if err := applyModelDeltas(ctx, tx, uid, models); err != nil {
		return err
	}
	if total != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE kv SET value=value-? WHERE name='all_struggle'`, total); err != nil {
			return err
		}
	}
	// it takes the plain slug of its key when one of the folded had it
	if want := slugify(p.Key, p.Name); want != p.Slug && slices.Contains(slugs, want) {
		for _, q := range []struct {
			q    string
			args []any
		}{
			{`DELETE FROM project_links WHERE user_id=? AND name=?`, []any{uid, want}},
			{`UPDATE projects SET slug=? WHERE id=?`, []any{want, to}},
			{`INSERT INTO project_links(user_id,name,project_id) VALUES (?,?,?)`, []any{uid, p.Slug, to}},
		} {
			if _, err := tx.ExecContext(ctx, q.q, q.args...); err != nil {
				return err
			}
		}
	}
	return nil
}

// addRows is one device's minute in two projects that were one folder, as
// the one minute it was: its seconds add up, to a minute at most. Each of
// the projects recorded the folder's commits, so those are counted once.
func addRows(a, b brow) brow {
	r := brow{dev: a.dev,
		human: min(60, a.human+b.human), struggle: min(60, a.struggle+b.struggle), dark: min(60, a.dark+b.dark),
		prompts: a.prompts + b.prompts, edits: a.edits + b.edits, commits: max(a.commits, b.commits), runs: a.runs + b.runs}
	r.agent = min(r.struggle, a.agent+b.agent)
	hs := strings.Split(a.harn, ",")
	for _, h := range strings.Split(b.harn, ",") {
		if !slices.Contains(hs, h) {
			hs = append(hs, h)
		}
	}
	hs = slices.DeleteFunc(hs, func(h string) bool { return h == "" })
	slices.Sort(hs)
	r.harn = strings.Join(hs, ",")
	ms := decodeModels(a.models)
next:
	for _, m := range decodeModels(b.models) {
		for i := range ms {
			if ms[i].Model == m.Model {
				ms[i].Seconds += m.Seconds
				ms[i].Prompts += m.Prompts
				continue next
			}
		}
		ms = append(ms, m)
	}
	r.models = encodeModels(ms)
	return r
}
