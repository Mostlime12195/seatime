package sim

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	srvstore "seatime/internal/server/store"
)

// The demo database: every honest persona and every attack, each as its own
// user with a public profile and its submissions, in ONE seatimed SQLite file
// that a normal seatimed can serve (SEATIME_DB=<file>) for a public showcase.
//
// Each persona still runs in its own throwaway World, so the server's
// cross-account checks, such as replayed prompts, do not see the other
// personas. Afterwards its server database
// is copied into the demo file with ids remapped. Personas start one fake day
// apart so "earlier submission" is well defined and the same on every build.
// Times are the simulator's fake clock (tomorrow 09:00 UTC onwards).

// DemoOptions configure BuildDemo.
type DemoOptions struct {
	Out  string // seatimed SQLite file to create (replaced if present)
	Dir  string // scratch directory for the per-persona worlds
	Seed uint64 // RNG seed for human timing
	Run  string // only personas whose name contains one of these (comma-separated)
	Logf func(format string, args ...any)
}

// DemoUser is one account in the demo database.
type DemoUser struct {
	Username    string
	Persona     string
	Honest      bool
	Verdict     string // attacks: blocked | detected | discounted | succeeds; honest: pass | FAIL
	Submissions []string
	Credited    float64
	Note        string
}

// seatimed clamps usernames to 32 characters, so long persona names get a short
// account name.
var demoShort = map[string]string{
	"alarm_clock_attendance":    "alarm_clock",
	"forged_sync_buckets":       "forged_buckets",
	"forged_commit_dates":       "forged_commits",
	"resubmit_double_dip":       "resubmit_dip",
	"heartbeat_bot_jitter":      "heartbeat_jitter",
	"cherry_pick_other_project": "cherry_pick",
	"two_device_double_count":   "two_device_double",
	"backfill_synced_device":    "backfill_synced",
}

// DemoSlug is the account name of a persona.
func DemoSlug(p *Persona) string {
	name := p.Name
	if s, ok := demoShort[name]; ok {
		name = s
	}
	s := strings.ReplaceAll(name, "_", "-")
	if p.Honest {
		return "demo-" + s
	}
	return "demo-attack-" + s
}

// demoAlias names a persona's people: the first becomes demo-<persona>, any
// further person demo-<persona>-2, -3, ...
func demoAlias(slug string) func(string) string {
	seen := map[string]string{}
	return func(u string) string {
		if u == "" || strings.HasPrefix(u, "demo-") {
			return u
		}
		if a, ok := seen[u]; ok {
			return a
		}
		a := slug
		if len(seen) > 0 {
			a = fmt.Sprintf("%s-%d", slug, len(seen)+1)
		}
		seen[u] = a
		return a
	}
}

// BuildDemo runs the personas and writes the demo database.
func BuildDemo(o DemoOptions) ([]DemoUser, error) {
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(o.Out), 0o755); err != nil {
		return nil, err
	}
	for _, suf := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(o.Out + suf)
	}
	st, err := srvstore.Open(o.Out) // creates the schema
	if err != nil {
		return nil, err
	}
	if err := st.Close(); err != nil {
		return nil, err
	}
	master, err := sql.Open("sqlite", "file:"+o.Out+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(0)")
	if err != nil {
		return nil, err
	}
	defer master.Close()
	master.SetMaxOpenConns(1) // ATTACH is per connection

	defer func() { UserAlias = nil }()
	var out []DemoUser
	i := 0
	for _, p := range append(HonestPersonas(), Attacks()...) {
		if o.Run != "" && !matches(p.Name, o.Run) {
			continue
		}
		slug := DemoSlug(p)
		if len(slug) > 30 { // room for a "-N" suffix
			return out, fmt.Errorf("account name %q is too long (seatimed clamps usernames to 32 characters); add it to demoShort", slug)
		}
		UserAlias = demoAlias(slug)
		wdir := filepath.Join(o.Dir, slug)
		// Demo personas live in the recent past (one fake day apart, the last
		// ending a few days ago) so a public showcase never shows future dates.
		demoStart := time.Now().UTC().Truncate(24 * time.Hour).Add(-60*24*time.Hour + 9*time.Hour)
		wo := WorldOpts{Start: demoStart.Add(time.Duration(i) * 24 * time.Hour)}
		i++
		var subs []string
		r := runPersona(p, wdir, o.Seed, wo, func(w *World, oc *Outcome) { subs = demoSubs(oc) }, true)
		if r.Err != nil || r.O == nil {
			logf("%-34s ERROR %v (skipped)", p.Name, r.Err)
			_ = os.RemoveAll(wdir)
			continue
		}
		users, err := importWorld(master, filepath.Join(wdir, "seatimed.db"), p)
		_ = os.RemoveAll(wdir)
		if err != nil {
			return out, fmt.Errorf("import %s: %w", p.Name, err)
		}
		verdict := r.Verdict
		if p.Honest {
			verdict = "pass"
			if !r.Pass {
				verdict = "FAIL"
			}
		}
		for _, u := range users {
			du := DemoUser{Username: u, Persona: p.Name, Honest: p.Honest, Verdict: verdict, Submissions: subs, Credited: r.O.Credited}
			if len(subs) == 0 {
				du.Note = "no submission was accepted (blocked or refused before any submission existed)"
			}
			out = append(out, du)
		}
		logf("%-34s %-26s %-10s %d submissions", p.Name, strings.Join(users, ","), verdict, len(subs))
	}
	// shared checkpoint so the file is a single, self-contained database
	_, _ = master.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	_, _ = master.Exec(`PRAGMA journal_mode=DELETE`)
	sort.SliceStable(out, func(a, b int) bool { return out[a].Username < out[b].Username })
	return out, nil
}

// demoSubs lists the submissions the persona got accepted.
func demoSubs(o *Outcome) []string {
	var ids []string
	seen := map[string]bool{}
	for _, s := range o.Subs {
		if s != nil && s.ID != "" && !seen[s.ID] {
			seen[s.ID] = true
			ids = append(ids, s.ID)
		}
	}
	return ids
}

func matches(name, pats string) bool {
	for _, p := range strings.Split(pats, ",") {
		if p = strings.TrimSpace(p); p != "" && strings.Contains(name, p) {
			return true
		}
	}
	return false
}

// importWorld copies one persona world's seatimed database into master and
// labels its users and projects as simulated. It returns the persona's
// usernames.
func importWorld(master *sql.DB, src string, p *Persona) ([]string, error) {
	ctx := context.Background()
	if _, err := master.ExecContext(ctx, `ATTACH DATABASE ? AS src`, src); err != nil {
		return nil, err
	}
	defer master.ExecContext(ctx, `DETACH DATABASE src`)
	tx, err := master.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	cols := func(table string) ([]string, error) {
		rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?, 'main')`, table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var cs []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				return nil, err
			}
			cs = append(cs, c)
		}
		return cs, rows.Err()
	}
	except := func(cs []string, drop ...string) []string {
		var out []string
		for _, c := range cs {
			keep := true
			for _, d := range drop {
				if c == d {
					keep = false
				}
			}
			if keep {
				out = append(out, c)
			}
		}
		return out
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS umap(old INTEGER PRIMARY KEY, new INTEGER NOT NULL)`); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS pmap(old INTEGER PRIMARY KEY, new INTEGER NOT NULL)`); err != nil {
		return nil, err
	}
	for _, t := range []string{"umap", "pmap"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM temp.`+t); err != nil {
			return nil, err
		}
	}

	// users: reuse an existing account of the same name
	ucols, err := cols("users")
	if err != nil {
		return nil, err
	}
	ucols = except(ucols, "id")
	type urow struct {
		id   int64
		name string
	}
	var us []urow
	rows, err := tx.QueryContext(ctx, `SELECT id, username FROM src.users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u urow
		if err := rows.Scan(&u.id, &u.name); err != nil {
			rows.Close()
			return nil, err
		}
		us = append(us, u)
	}
	rows.Close()
	var names []string
	for _, u := range us {
		var nid int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM main.users WHERE username=?`, u.name).Scan(&nid)
		if err == sql.ErrNoRows {
			res, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO main.users(%s) SELECT %s FROM src.users WHERE id=?`,
				strings.Join(ucols, ","), strings.Join(ucols, ",")), u.id)
			if err != nil {
				return nil, err
			}
			if nid, err = res.LastInsertId(); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO temp.umap(old,new) VALUES (?,?)`, u.id, nid); err != nil {
			return nil, err
		}
		if strings.HasPrefix(u.name, "demo-") {
			names = append(names, u.name)
		}
	}

	// projects
	pcols, err := cols("projects")
	if err != nil {
		return nil, err
	}
	pcols = except(pcols, "id")
	sel := make([]string, len(pcols))
	for i, c := range pcols {
		sel[i] = "p." + c
		if c == "user_id" {
			sel[i] = "(SELECT new FROM temp.umap WHERE old=p.user_id)"
		}
	}
	prows, err := tx.QueryContext(ctx, `SELECT id FROM src.projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var pids []int64
	for prows.Next() {
		var id int64
		if err := prows.Scan(&id); err != nil {
			prows.Close()
			return nil, err
		}
		pids = append(pids, id)
	}
	prows.Close()
	for _, id := range pids {
		res, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO main.projects(%s) SELECT %s FROM src.projects p WHERE p.id=?`,
			strings.Join(pcols, ","), strings.Join(sel, ",")), id)
		if err != nil {
			return nil, err
		}
		nid, _ := res.LastInsertId()
		if _, err := tx.ExecContext(ctx, `INSERT INTO temp.pmap(old,new) VALUES (?,?)`, id, nid); err != nil {
			return nil, err
		}
	}

	// everything else, with user and project ids remapped
	for _, t := range []string{"identities", "devices", "buckets", "hourly", "hourly_models", "checkpoints", "commits", "segments", "segment_blobs",
		"segment_minutes", "segment_models"} {
		cs, err := cols(t)
		if err != nil {
			return nil, err
		}
		sel := make([]string, len(cs))
		for i, c := range cs {
			switch c {
			case "user_id":
				sel[i] = fmt.Sprintf("(SELECT new FROM temp.umap WHERE old=s.%s)", c)
			case "project_id":
				sel[i] = "(SELECT new FROM temp.pmap WHERE old=s.project_id)"
			default:
				sel[i] = "s." + c
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT OR IGNORE INTO main.%s(%s) SELECT %s FROM src.%s s`,
			t, strings.Join(cs, ","), strings.Join(sel, ","), t)); err != nil {
			return nil, fmt.Errorf("copy %s: %w", t, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO main.kv(name,value) SELECT name,value FROM src.kv WHERE name='all_struggle'
ON CONFLICT(name) DO UPDATE SET value=main.kv.value+excluded.value`); err != nil {
		return nil, err
	}

	// label: simulated people are public and say so
	kind := "attack"
	if p.Honest {
		kind = "honest persona"
	}
	label := fmt.Sprintf("Simulated persona (%s: %s). From seatime's red-team simulator, not a real person.", kind, p.Name)
	if _, err := tx.ExecContext(ctx, `UPDATE main.users SET display_name=?, public=1 WHERE username LIKE 'demo-%' AND (display_name='' OR display_name=username)`, label); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE main.users SET public=0, role='user' WHERE username NOT LIKE 'demo-%'`); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE main.projects SET name = name || ' (simulated by seatime''s red-team simulator, not a real repo)'
WHERE user_id IN (SELECT id FROM main.users WHERE username LIKE 'demo-%') AND name NOT LIKE '%(simulated%'`); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}
