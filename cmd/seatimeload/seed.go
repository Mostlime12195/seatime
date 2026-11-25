package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	mrand "math/rand/v2"
	"net/url"
	"os"
	"sort"
	"time"

	"seatime/internal/server/store"

	_ "modernc.org/sqlite"
)

// UserRec is one pre-registered user+device the simulator can drive.
type UserRec struct {
	Idx       int       `json:"idx"`
	Name      string    `json:"name"`
	Device    string    `json:"device"`
	Token     string    `json:"token"`
	Priv      string    `json:"priv"` // base64 ed25519 private key
	Seq       int64     `json:"seq"`  // last checkpoint seq already in the database
	Submitter bool      `json:"submitter,omitempty"`
	Projects  []ProjRec `json:"projects"`
}

type ProjRec struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

func hashHex(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func cmdSeed(args []string) {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	dbPath := fs.String("db", "", "SQLite file to create (must not exist)")
	usersOut := fs.String("users", "", "write the users file here")
	n := fs.Int("n", MAU, "users to create")
	days := fs.Int("days", 30, "days of hourly-rollup history")
	bdays := fs.Int("bucket-days", 14, "days of per-minute buckets and checkpoints (the big tables)")
	subs := fs.Int("submitters", 200, "users reserved for submission traffic (no history)")
	fs.Parse(args)
	if *dbPath == "" || *usersOut == "" {
		fs.Usage()
		os.Exit(2)
	}
	if _, err := os.Stat(*dbPath); err == nil {
		fatalf("%s exists", *dbPath)
	}
	st, err := store.Open(*dbPath) // creates the schema exactly as seatimed would
	if err != nil {
		fatalf("open: %v", err)
	}
	st.Close()

	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "synchronous(OFF)", "foreign_keys(0)", "cache_size(-64000)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+*dbPath+"?"+q.Encode())
	if err != nil {
		fatalf("%v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	rng := mrand.New(mrand.NewPCG(42, 7))
	now := time.Now().UTC()
	nowHour := now.Unix() / 3600
	w := diurnal()
	var cum [24]float64
	acc := 0.0
	for i, v := range w {
		acc += v
		cum[i] = acc
	}
	pickHour := func() int { // UTC hour of day, diurnally weighted
		r := rng.Float64()
		for i, c := range cum {
			if r <= c {
				return i
			}
		}
		return 23
	}

	// Per-user monthly hours: lognormal, rescaled so the total is the real one.
	hrs := make([]float64, *n)
	var tot float64
	for i := range hrs {
		hrs[i] = math.Exp(1.15 * rng.NormFloat64())
		tot += hrs[i]
	}
	for i := range hrs {
		hrs[i] *= MonthlyHours / tot * float64(*days) / 30
	}

	tx, _ := db.Begin()
	prep := func(q string) *sql.Stmt {
		s, err := tx.Prepare(q)
		if err != nil {
			fatalf("%v: %s", err, q)
		}
		return s
	}
	insUser := prep(`INSERT INTO users(id,username,display_name,avatar,slack_id,github,public,role,tz_offset_min,created) VALUES (?,?,?,?,?,?,?,'user',?,?)`)
	insIdent := prep(`INSERT INTO identities(provider,subject,user_id) VALUES ('dev',?,?)`)
	insDev := prep(`INSERT INTO devices(id,user_id,name,os,pubkey,token_hash,last_seen,created,revoked) VALUES (?,?,?,?,?,?,?,?,0)`)
	insProj := prep(`INSERT INTO projects(id,user_id,key,slug,name,remote,repo,hidden,created) VALUES (?,?,?,?,?,?,?,?,?)`)
	insHour := prep(`INSERT INTO hourly(user_id,project_id,hour,human,struggle,dark_flow,prompts,edits,commits,runs) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`)
	insBucket := prep(`INSERT OR IGNORE INTO buckets(user_id,project_id,start_min,device_id,human,struggle,dark_flow,agent,prompts,edits,commits,runs,harnesses) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	insCP := prep(`INSERT OR IGNORE INTO checkpoints(device_id,seq,hash,server_ts) VALUES (?,?,?,?)`)
	insCommit := prep(`INSERT OR IGNORE INTO commits(project_id,sha,ts,ins,del) VALUES (?,?,?,?,?)`)

	recs := make([]UserRec, 0, *n)
	var projID int64
	var allStruggle float64
	var nBuckets, nHourly, nCP int64
	t0 := time.Now()
	for i := 0; i < *n; i++ {
		uid := int64(i + 1)
		name := fmt.Sprintf("lu%05d", i+1)
		tz := []int{-300, -300, -360, -420, -480, 0, 60, 120, 330, 480, -180}[rng.IntN(11)]
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		tokb := make([]byte, 32)
		rand.Read(tokb)
		token := "grt_" + base64.RawURLEncoding.EncodeToString(tokb)
		devb := make([]byte, 8)
		rand.Read(devb)
		dev := hex.EncodeToString(devb)
		public := 1
		if rng.Float64() < 0.03 {
			public = 0
		}
		created := now.Add(-time.Duration(rng.IntN(*days*24)) * time.Hour).Unix()
		mustExec(insUser, uid, name, name, "", "", "", public, tz, created)
		mustExec(insIdent, name, uid)
		mustExec(insDev, dev, uid, "laptop", "linux", base64.StdEncoding.EncodeToString(pub), hashHex(token), now.Unix(), created)

		rec := UserRec{Idx: i, Name: name, Device: dev, Token: token, Priv: base64.StdEncoding.EncodeToString(priv), Submitter: i >= *n-*subs}
		np := 1 + b2i(rng.Float64() < 0.35) + b2i(rng.Float64() < 0.1)
		pids := make([]int64, np)
		for p := 0; p < np; p++ {
			projID++
			pids[p] = projID
			var key, pname string
			remote, repo := "", ""
			if rng.Float64() < 0.6 {
				pname = fmt.Sprintf("repo%d", p+1)
				key = fmt.Sprintf("github.com/%s/%s", name, pname)
				remote, repo = "https://"+key, name+"/"+pname
			} else {
				h := hashHex(fmt.Sprintf("%s-%d", name, p))[:12]
				key, pname = "local:"+h, fmt.Sprintf("proj%d", p+1)
			}
			mustExec(insProj, projID, uid, key, pname, pname, remote, repo, 0, created)
			rec.Projects = append(rec.Projects, ProjRec{Key: key, Name: pname})
		}

		if !rec.Submitter {
			h := hrs[i]
			nd := int(math.Min(float64(*days), math.Max(1, math.Floor(h/1.6)+1)))
			dayPerm := rng.Perm(*days)[:nd]
			sort.Ints(dayPerm)
			wRatio := 0.25 + 0.45*rng.Float64()
			seq := int64(0)
			for _, d := range dayPerm {
				hd := h / float64(nd)
				startHour := (nowHour-int64(d*24))/24*24 + int64(pickHour())
				if startHour > nowHour {
					startHour -= 24
				}
				remain := hd
				for k := int64(0); remain > 0.02; k++ {
					hh := math.Min(1, remain)
					remain -= hh
					hour := startHour + k
					if hour > nowHour {
						break
					}
					p := pids[0]
					if np > 1 && rng.Float64() < 0.3 {
						p = pids[1+rng.IntN(np-1)]
					}
					human := 3600 * hh * (0.8 + 0.2*rng.Float64())
					strug := human * wRatio * (0.7 + 0.6*rng.Float64())
					strug = math.Min(strug, human)
					nb := int(math.Max(1, math.Round(human/50)))
					pr, ed, cm, rn := nb*2, nb*8, 0, nb/3
					if rng.Float64() < 0.08 {
						cm = 1
					}
					mustExec(insHour, uid, p, hour, human, strug, human-strug, pr, ed, cm, rn)
					nHourly++
					allStruggle += strug
					if cm > 0 {
						sha := hashHex(fmt.Sprintf("%s-%d", name, hour))[:40]
						mustExec(insCommit, p, sha, hour*3600+1800, 20, 5)
					}
					if d < *bdays {
						mins := rng.Perm(60)[:min(nb, 60)]
						for _, m := range mins {
							hm := math.Min(60, human/float64(nb)*(0.8+0.4*rng.Float64()))
							sm := math.Min(hm, hm*wRatio*(0.7+0.6*rng.Float64()))
							mustExec(insBucket, uid, p, hour*60+int64(m), dev, hm, sm, hm-sm, rng.Float64()*200, rng.IntN(4), rng.IntN(20), 0, rng.IntN(2), "claude-code")
							nBuckets++
						}
						for c := 0; c < 40; c++ {
							seq += int64(15 + rng.IntN(30))
							mustExec(insCP, dev, seq, hashHex(fmt.Sprintf("%s%d", dev, seq)), hour*3600+int64(c*90))
							nCP++
						}
					}
				}
			}
			rec.Seq = seq
		}
		recs = append(recs, rec)
		if i%1000 == 999 {
			if err := tx.Commit(); err != nil {
				fatalf("commit: %v", err)
			}
			tx, _ = db.Begin()
			insUser = prep(`INSERT INTO users(id,username,display_name,avatar,slack_id,github,public,role,tz_offset_min,created) VALUES (?,?,?,?,?,?,?,'user',?,?)`)
			insIdent = prep(`INSERT INTO identities(provider,subject,user_id) VALUES ('dev',?,?)`)
			insDev = prep(`INSERT INTO devices(id,user_id,name,os,pubkey,token_hash,last_seen,created,revoked) VALUES (?,?,?,?,?,?,?,?,0)`)
			insProj = prep(`INSERT INTO projects(id,user_id,key,slug,name,remote,repo,hidden,created) VALUES (?,?,?,?,?,?,?,?,?)`)
			insHour = prep(`INSERT INTO hourly(user_id,project_id,hour,human,struggle,dark_flow,prompts,edits,commits,runs) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`)
			insBucket = prep(`INSERT OR IGNORE INTO buckets(user_id,project_id,start_min,device_id,human,struggle,dark_flow,agent,prompts,edits,commits,runs,harnesses) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
			insCP = prep(`INSERT OR IGNORE INTO checkpoints(device_id,seq,hash,server_ts) VALUES (?,?,?,?)`)
			insCommit = prep(`INSERT OR IGNORE INTO commits(project_id,sha,ts,ins,del) VALUES (?,?,?,?,?)`)
			fmt.Fprintf(os.Stderr, "seeded %d users, %d hourly, %d buckets, %d checkpoints (%.0fs)\n", i+1, nHourly, nBuckets, nCP, time.Since(t0).Seconds())
		}
	}
	if _, err := tx.Exec(`INSERT INTO kv(name,value) VALUES ('all_struggle',?) ON CONFLICT(name) DO UPDATE SET value=excluded.value`, allStruggle); err != nil {
		fatalf("%v", err)
	}
	if err := tx.Commit(); err != nil {
		fatalf("commit: %v", err)
	}
	db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	b, _ := json.Marshal(recs)
	if err := os.WriteFile(*usersOut, b, 0o600); err != nil {
		fatalf("%v", err)
	}
	fi, _ := os.Stat(*dbPath)
	fmt.Printf("seed done: %d users, %d hourly rows (%.0f h), %d buckets, %d checkpoints; db %.0f MB\n",
		*n, nHourly, allStruggle/3600, nBuckets, nCP, float64(fi.Size())/1e6)
}

func mustExec(s *sql.Stmt, args ...any) {
	if _, err := s.Exec(args...); err != nil {
		fatalf("seed exec: %v", err)
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}

// ---- dbstat ----

func cmdDBStat(args []string) {
	fs := flag.NewFlagSet("dbstat", flag.ExitOnError)
	dbPath := fs.String("db", "", "SQLite file")
	fs.Parse(args)
	out, err := dbStat(*dbPath)
	if err != nil {
		fatalf("%v", err)
	}
	var tot int64
	for _, r := range out {
		fmt.Printf("%-28s %12d bytes %10d rows %8.1f B/row\n", r.Name, r.Bytes, r.Rows, float64(r.Bytes)/math.Max(1, float64(r.Rows)))
		tot += r.Bytes
	}
	fmt.Printf("%-28s %12d bytes\n", "TOTAL", tot)
}

type TableSize struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Rows  int64  `json:"rows"`
}

// dbStat returns bytes (via the dbstat vtable) and row counts per b-tree.
func dbStat(path string) ([]TableSize, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name, SUM(pgsize) FROM dbstat WHERE name NOT LIKE 'sqlite_%' GROUP BY name ORDER BY 2 DESC`)
	if err != nil {
		return nil, err
	}
	var out []TableSize
	for rows.Next() {
		var t TableSize
		if err := rows.Scan(&t.Name, &t.Bytes); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	for i, t := range out {
		var kind string
		db.QueryRow(`SELECT type FROM sqlite_master WHERE name=?`, t.Name).Scan(&kind)
		if kind == "table" {
			db.QueryRow(`SELECT COUNT(*) FROM "` + t.Name + `"`).Scan(&out[i].Rows)
		}
	}
	return out, nil
}
