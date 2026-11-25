package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"seatime/pkg/api"
	"seatime/pkg/audit"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

// ---- results collector ----

type collector struct {
	mu    sync.Mutex
	on    atomic.Bool
	lat   map[string][]float64 // ms
	codes map[string]map[string]int
	extra map[string]int64
}

func newCollector() *collector {
	return &collector{lat: map[string][]float64{}, codes: map[string]map[string]int{}, extra: map[string]int64{}}
}

func (c *collector) add(class string, d time.Duration, code int, err error) {
	if !c.on.Load() {
		return
	}
	k := strconv.Itoa(code)
	if err != nil {
		k = "err"
	}
	c.mu.Lock()
	c.lat[class] = append(c.lat[class], float64(d.Microseconds())/1000)
	if c.codes[class] == nil {
		c.codes[class] = map[string]int{}
	}
	c.codes[class][k]++
	c.mu.Unlock()
}

func (c *collector) count(name string, n int64) {
	if c.on.Load() {
		c.mu.Lock()
		c.extra[name] += n
		c.mu.Unlock()
	}
}

type ClassResult struct {
	N     int            `json:"n"`
	Rate  float64        `json:"per_s"`
	P50   float64        `json:"p50_ms"`
	P95   float64        `json:"p95_ms"`
	P99   float64        `json:"p99_ms"`
	Max   float64        `json:"max_ms"`
	Codes map[string]int `json:"codes"`
}

func pct(s []float64, p float64) float64 {
	if len(s) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(s)))) - 1
	return s[max(0, min(len(s)-1, i))]
}

func (c *collector) results(secs float64) map[string]ClassResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]ClassResult{}
	for k, v := range c.lat {
		s := append([]float64(nil), v...)
		sort.Float64s(s)
		out[k] = ClassResult{N: len(s), Rate: float64(len(s)) / secs, P50: pct(s, .5), P95: pct(s, .95), P99: pct(s, .99), Max: s[len(s)-1], Codes: c.codes[k]}
	}
	return out
}

// ---- http plumbing ----

type xffRT struct {
	ip   string
	base http.RoundTripper
}

func (x xffRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Forwarded-For", x.ip)
	return x.base.RoundTrip(r)
}

func ipFor(n uint32) string { return fmt.Sprintf("10.%d.%d.%d", n>>16&255, n>>8&255, n&255) }

type gen struct {
	base    string
	debug   string // the debug listener's base URL
	col     *collector
	rd, wr  *http.Client // readers (keepalive), writers (new connection per sync like the real daemon after 90 s idle)
	rng     *mrand.Rand
	rngMu   sync.Mutex
	users   []UserRec
	active  []int // indexes of simulated active users
	zipf    *mrand.Zipf
	ctx     context.Context
	scale   float64
	sseOpen atomic.Int64
}

func (g *gen) rnd() float64 { g.rngMu.Lock(); defer g.rngMu.Unlock(); return g.rng.Float64() }
func (g *gen) zipfIdx() int { g.rngMu.Lock(); defer g.rngMu.Unlock(); return int(g.zipf.Uint64()) }

func (g *gen) do(cl *http.Client, class, method, path string, hdr map[string]string, body []byte, ip string) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(g.ctx, method, g.base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Forwarded-For", ip)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	t0 := time.Now()
	resp, err := cl.Do(req)
	if err != nil {
		g.col.add(class, time.Since(t0), 0, err)
		return 0, nil, err
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	g.col.add(class, time.Since(t0), resp.StatusCode, err)
	return resp.StatusCode, b, err
}

// ---- simulated daemon ----

type simClient struct {
	g      *gen
	u      *UserRec
	priv   ed25519.PrivateKey
	seq    int64
	lastEn int64 // last minute included in a successful sync
	nSync  int
	ip     string
}

func newSimClient(g *gen, idx int) *simClient {
	u := &g.users[idx]
	pb, _ := base64.StdEncoding.DecodeString(u.Priv)
	return &simClient{g: g, u: u, priv: ed25519.PrivateKey(pb), seq: max(u.Seq, time.Now().Unix()*100), ip: ipFor(uint32(idx) + 1<<20)}
}

// minuteBuckets deterministically generates the buckets for one minute. ver
// changes only for the newest minutes (late events revise them); older minutes
// resend identical values, as the real client does.
func (c *simClient) minuteBuckets(m int64, ver int) []ledger.Bucket {
	r := mrand.New(mrand.NewPCG(uint64(c.u.Idx)<<40^uint64(m), uint64(ver)+1))
	if r.Float64() > 0.85 { // idle minute
		return nil
	}
	var out []ledger.Bucket
	mk := func(p ProjRec) ledger.Bucket {
		counted := 20 + 40*r.Float64()
		ai := r.Float64() * counted
		b := ledger.Bucket{Project: p.Key, Start: time.Unix(m*60, 0).UTC(), CodingSeconds: counted - ai, AISeconds: ai,
			AINotCountedSeconds: r.Float64() * (60 - counted), Prompts: r.IntN(4), Edits: r.IntN(25), Runs: r.IntN(2), Harnesses: []string{"claude-code"}}
		if r.Float64() < 0.4 {
			b.Harnesses = append(b.Harnesses, "editor:vscode")
		}
		return b
	}
	out = append(out, mk(c.u.Projects[0]))
	if len(c.u.Projects) > 1 && r.Float64() < 0.2 {
		out = append(out, mk(c.u.Projects[1+r.IntN(len(c.u.Projects)-1)]))
	}
	return out
}

func (c *simClient) sign(seq int64, hash string) string {
	msg := fmt.Sprintf("seatime-checkpoint\n%s\n%d\n%s", c.u.Device, seq, hash)
	return base64.StdEncoding.EncodeToString(ed25519.Sign(c.priv, []byte(msg)))
}

func (c *simClient) syncOnce() {
	nowMin := time.Now().Unix() / 60
	from := c.lastEn - 30
	var req api.SyncReq
	seen := map[string]bool{}
	for m := from; m <= nowMin; m++ {
		ver := 0
		if m >= nowMin-1 {
			ver = c.nSync
		}
		for _, b := range c.minuteBuckets(m, ver) {
			req.Buckets = append(req.Buckets, b)
			seen[b.Project] = true
		}
	}
	for _, p := range c.u.Projects {
		if seen[p.Key] {
			meta := api.ProjectMeta{Key: p.Key, Name: p.Name}
			if strings.HasPrefix(p.Key, "github.com/") {
				meta.Remote = "https://" + p.Key
			}
			req.Projects = append(req.Projects, meta)
		}
	}
	if c.g.rnd() < 0.03 && len(c.u.Projects) > 0 {
		sb := make([]byte, 20)
		rand.Read(sb)
		req.Commits = []api.CommitRef{{Project: c.u.Projects[0].Key, SHA: hex.EncodeToString(sb), TS: time.Now().UTC(), Ins: 12, Del: 3}}
	}
	elapsed := max(1, nowMin-c.lastEn)
	seq := c.seq + elapsed*25
	h := sha256.Sum256([]byte(c.u.Token + strconv.FormatInt(seq, 10)))
	req.DeviceID, req.ClientVersion, req.ScoreVersion = c.u.Device, "seatimeload", ledger.Version
	req.Checkpoint = chain.Checkpoint{Seq: seq, Hash: hex.EncodeToString(h[:])}
	req.CheckpointSig = c.sign(seq, req.Checkpoint.Hash)
	body, _ := json.Marshal(req)
	code, _, err := c.g.do(c.g.wr, "sync", "POST", "/api/v1/sync", map[string]string{"Authorization": "Bearer " + c.u.Token, "Content-Type": "application/json"}, body, c.ip)
	if err == nil && code == 200 {
		c.seq, c.lastEn = seq, nowMin
		c.nSync++
	}
}

func (c *simClient) run(startDelay time.Duration) {
	c.lastEn = time.Now().Unix()/60 - 2 // warm start: mid-session, trailing 30 min is filled
	select {
	case <-c.g.ctx.Done():
		return
	case <-time.After(startDelay):
	}
	for {
		c.syncOnce()
		d := 60*time.Second + time.Duration(c.g.rnd()*float64(60*time.Second))
		select {
		case <-c.g.ctx.Done():
			return
		case <-time.After(d):
		}
	}
}

// ---- readers ----

func (g *gen) poisson(rate float64, f func()) {
	if rate <= 0 {
		return
	}
	sem := make(chan struct{}, 512)
	go func() {
		for {
			g.rngMu.Lock()
			d := time.Duration(g.rng.ExpFloat64() / rate * float64(time.Second))
			g.rngMu.Unlock()
			select {
			case <-g.ctx.Done():
				return
			case <-time.After(d):
			}
			select {
			case sem <- struct{}{}:
				go func() { defer func() { <-sem }(); f() }()
			default:
				g.col.count("client_shed", 1)
			}
		}
	}()
}

func (g *gen) readerIP() string {
	g.rngMu.Lock()
	defer g.rngMu.Unlock()
	return ipFor(uint32(g.rng.IntN(1<<24)) | 1<<26)
}

func (g *gen) startReaders(base map[string]float64) {
	pick := func() *UserRec { return &g.users[g.zipfIdx()] }
	g.poisson(base["profile"]*g.scale, func() {
		g.do(g.rd, "profile", "GET", "/@"+pick().Name, nil, nil, g.readerIP())
	})
	g.poisson(base["project"]*g.scale, func() {
		u := pick()
		g.do(g.rd, "project", "GET", "/@"+u.Name+"/"+u.Projects[0].Name, nil, nil, g.readerIP())
	})
	g.poisson(base["leaderboard"]*g.scale, func() {
		p := "week"
		switch r := g.rnd(); {
		case r < 0.25:
			p = "today"
		case r > 0.85:
			p = "month"
		}
		g.do(g.rd, "leaderboard", "GET", "/leaderboard?p="+p, nil, nil, g.readerIP())
	})
	g.poisson(base["home"]*g.scale, func() { g.do(g.rd, "home", "GET", "/", nil, nil, g.readerIP()) })
	g.poisson(base["stats"]*g.scale, func() {
		u := pick()
		g.do(g.rd, "stats_api", "GET", "/api/v1/users/"+u.Name+"/projects/"+u.Projects[0].Name+"/stats", nil, nil, g.readerIP())
	})
}

// sseWatch holds a live connection like the browser's EventSource; on a
// "sync" event it refetches the profile fragment after a 1 s debounce.
func (g *gen) sseWatch(path, wantEvent string, frag string) {
	ip := g.readerIP()
	for g.ctx.Err() == nil {
		req, _ := http.NewRequestWithContext(g.ctx, "GET", g.base+path, nil)
		req.Header.Set("X-Forwarded-For", ip)
		t0 := time.Now()
		resp, err := g.rd.Do(req)
		if err != nil {
			g.col.add("sse_connect", time.Since(t0), 0, err)
			select {
			case <-g.ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		g.col.add("sse_connect", time.Since(t0), resp.StatusCode, nil)
		if resp.StatusCode != 200 {
			resp.Body.Close()
			if resp.StatusCode == 404 { // private profile: EventSource gives up too
				return
			}
			select {
			case <-g.ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		g.sseOpen.Add(1)
		sc := bufio.NewScanner(resp.Body)
		var ev string
		var timer *time.Timer
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				ev = line[7:]
			} else if line == "" && ev != "" {
				if ev == wantEvent {
					g.col.count("sse_events_"+wantEvent, 1)
					if frag != "" && timer == nil {
						timer = time.AfterFunc(time.Second, func() {
							g.do(g.rd, "frag", "GET", frag, nil, nil, ip)
							timer = nil
						})
					}
				}
				ev = ""
			}
		}
		g.sseOpen.Add(-1)
		resp.Body.Close()
	}
}

// ---- registration flow (device code + dev login), full browser dance ----

var csrfRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func (g *gen) register(n int) {
	ip := g.readerIP()
	t0 := time.Now()
	name := fmt.Sprintf("reg%d%04d", time.Now().UnixNano()%1e6, n)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	body, _ := json.Marshal(api.DeviceStartReq{DeviceName: "laptop", OS: "linux", PublicKey: base64.StdEncoding.EncodeToString(pub)})
	code, out, err := g.do(g.wr, "reg_device_start", "POST", "/api/v1/device/start", map[string]string{"Content-Type": "application/json"}, body, ip)
	if err != nil || code != 200 {
		g.col.add("register", time.Since(t0), code, errors.New("start"))
		return
	}
	var st api.DeviceStartResp
	json.Unmarshal(out, &st)
	jar, _ := cookiejar.New(nil)
	br := &http.Client{Jar: jar, Transport: xffRT{ip, g.rd.Transport}}
	get := func(class, path string) (int, string) {
		t := time.Now()
		resp, err := br.Get(g.base + path)
		if err != nil {
			g.col.add(class, time.Since(t), 0, err)
			return 0, ""
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		g.col.add(class, time.Since(t), resp.StatusCode, nil)
		return resp.StatusCode, string(b)
	}
	post := func(class, path string, f url.Values) (int, string) {
		t := time.Now()
		resp, err := br.PostForm(g.base+path, f)
		if err != nil {
			g.col.add(class, time.Since(t), 0, err)
			return 0, ""
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		g.col.add(class, time.Since(t), resp.StatusCode, nil)
		return resp.StatusCode, string(b)
	}
	fail := func(why string) { g.col.add("register", time.Since(t0), 0, errors.New(why)) }
	_, page := get("reg_login_page", "/login")
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		fail("csrf")
		return
	}
	if c, _ := post("reg_dev_login", "/auth/dev", url.Values{"csrf": {m[1]}, "username": {name}}); c != 200 {
		fail("login")
		return
	}
	_, page = get("reg_device_page", "/device?code="+st.UserCode)
	m = csrfRe.FindStringSubmatch(page)
	if m == nil {
		fail("csrf2")
		return
	}
	if c, _ := post("reg_approve", "/device/approve", url.Values{"csrf": {m[1]}, "code": {st.UserCode}}); c != 200 {
		fail("approve")
		return
	}
	pb, _ := json.Marshal(api.DevicePollReq{DeviceCode: st.DeviceCode})
	code, out, err = g.do(g.wr, "reg_poll", "POST", "/api/v1/device/poll", map[string]string{"Content-Type": "application/json"}, pb, ip)
	if err != nil || code != 200 {
		fail("poll")
		return
	}
	var pr api.DevicePollResp
	json.Unmarshal(out, &pr)
	// first sync from the new device
	h := hex.EncodeToString(make([]byte, 32))
	cp := chain.Checkpoint{Seq: 1, Hash: h}
	msg := fmt.Sprintf("seatime-checkpoint\n%s\n%d\n%s", pr.DeviceID, cp.Seq, cp.Hash)
	sb, _ := json.Marshal(api.SyncReq{DeviceID: pr.DeviceID, ClientVersion: "seatimeload", ScoreVersion: ledger.Version, Checkpoint: cp,
		CheckpointSig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(msg)))})
	code, _, _ = g.do(g.wr, "reg_first_sync", "POST", "/api/v1/sync", map[string]string{"Authorization": "Bearer " + pr.Token, "Content-Type": "application/json"}, sb, ip)
	g.col.add("register", time.Since(t0), code, nil)
}

// ---- segments: event bundles ----

type submitter struct {
	u    *UserRec
	priv ed25519.PrivateKey
	seq  int64
	head string
	busy atomic.Bool
}

func (g *gen) submit(s *submitter) {
	g.rngMu.Lock()
	n := int(math.Min(20000, math.Max(100, 1500*math.Exp(1.0*g.rng.NormFloat64()))))
	g.rngMu.Unlock()
	proj := s.u.Projects[0].Key
	evs := make([]event.Event, 0, n)
	heads := make([]string, 0, n)
	prev := s.head
	now := time.Now().UTC()
	ts := now.Add(-time.Duration(n)*12*time.Second - 10*time.Minute).Truncate(time.Millisecond)
	sess := "s" + strconv.Itoa(s.u.Idx)
	for len(evs) < n {
		seq := s.seq + int64(len(evs)) + 1
		mk := func(k event.Kind) event.Event {
			ts = ts.Add(time.Duration(3000+g.rndInt(20000)) * time.Millisecond)
			return event.Event{Seq: seq, TS: ts, Kind: k, Harness: "claude-code", SessionID: sess, Project: proj, Model: "claude-opus-5"}
		}
		turn := []event.Event{}
		e := mk(event.Prompt)
		e.Text = fmt.Sprintf("the migration fails with 'no such column: owner_%d' after the rename; the test in store_test.go passes locally but CI errors in the same step, what should I read first?", len(evs))
		e.Words = 30
		turn = append(turn, e)
		turn = append(turn, event.Event{Kind: event.AgentStart})
		for t := 0; t < 1+g.rndInt(4); t++ {
			turn = append(turn, event.Event{Kind: event.Tool, Tool: "Edit", Files: []string{"internal/store/migrations.go"}, Write: true, Command: "go test ./... -run TestMigrate"})
		}
		turn = append(turn, event.Event{Kind: event.AgentStop})
		if g.rndInt(4) == 0 {
			turn = append(turn, event.Event{Kind: event.Edit, Tool: "vscode", Files: []string{"internal/store/migrations.go"}, Write: true, Lines: 3})
		}
		if g.rndInt(6) == 0 {
			ec := 0
			turn = append(turn, event.Event{Kind: event.Run, RunKind: event.RunTest, Command: "go test ./...", ExitCode: &ec})
		}
		if g.rndInt(12) == 0 {
			turn = append(turn, event.Event{Kind: event.Commit, SHA: strings.Repeat("c", 40), Insertions: 20, Deletions: 4, FilesChanged: 2})
		}
		for i, te := range turn {
			if len(evs) >= n {
				break
			}
			if i > 0 {
				x := mk(te.Kind)
				te.Seq, te.TS, te.Harness, te.SessionID, te.Project, te.Model = x.Seq, x.TS, x.Harness, x.SessionID, x.Project, x.Model
				e = te
			} else {
				e = te
			}
			e.Seq = s.seq + int64(len(evs)) + 1
			var err error
			if prev, err = chain.Next(prev, e); err != nil {
				return
			}
			evs = append(evs, e)
			heads = append(heads, prev)
		}
	}
	hi := evs[len(evs)-1].Seq
	// checkpoint at the end of the range, as the daemon would have synced it in real time
	cp := chain.Checkpoint{Seq: hi, Hash: heads[len(heads)-1]}
	msg := fmt.Sprintf("seatime-checkpoint\n%s\n%d\n%s", s.u.Device, cp.Seq, cp.Hash)
	sb, _ := json.Marshal(api.SyncReq{DeviceID: s.u.Device, ClientVersion: "seatimeload", ScoreVersion: ledger.Version, Checkpoint: cp,
		CheckpointSig: base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, []byte(msg)))})
	hdr := map[string]string{"Authorization": "Bearer " + s.u.Token, "Content-Type": "application/json"}
	ip := ipFor(uint32(s.u.Idx) + 2<<20)
	if code, _, err := g.do(g.wr, "submit_checkpoint", "POST", "/api/v1/sync", hdr, sb, ip); err != nil || code != 200 {
		return
	}
	var entries []chain.Entry
	var revealed []event.Event
	for i := range evs {
		ev := evs[i]
		if g.rndInt(100) < 12 { // other-project events: leaf hash only
			l, _ := chain.Leaf(ev)
			entries = append(entries, chain.Entry{Seq: ev.Seq, LeafHash: l})
			continue
		}
		entries = append(entries, chain.Entry{Seq: ev.Seq, Event: &ev})
		revealed = append(revealed, ev)
	}
	b := api.Bundle{DeviceID: s.u.Device, Project: proj, StartHash: s.head, Entries: entries, ScoreVersion: ledger.Version, Claimed: audit.Claim(revealed, proj)}
	raw, _ := json.Marshal(b)
	d := sha256.Sum256(raw)
	b.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, d[:]))
	body, _ := json.Marshal(b)
	code, out, err := g.do(g.wr, "submit", "POST", "/api/v1/segments", hdr, body, ip)
	g.col.count("submit_events", int64(n))
	g.col.count("submit_bytes", int64(len(body)))
	if err == nil && code == 200 {
		s.seq, s.head = hi, cp.Hash
		var r api.SegmentResp
		json.Unmarshal(out, &r)
		if r.Status != api.StatusVerified {
			g.col.count("submit_unverified", 1)
		}
	} else if code == 200 || code == 429 || code == 503 {
		// not stored; chain state unchanged except the checkpoint we synced, so advance anyway
		if code == 200 {
			s.seq, s.head = hi, cp.Hash
		}
	}
}

func (g *gen) rndInt(n int) int { g.rngMu.Lock(); defer g.rngMu.Unlock(); return g.rng.IntN(n) }

// ---- process sampling ----

type procSample struct {
	cpuTicks float64
	rssKB    int64
	hwmKB    int64
}

func readProc(pid int) (procSample, bool) {
	var s procSample
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return s, false
	}
	str := string(b)
	str = str[strings.LastIndexByte(str, ')')+2:]
	f := strings.Fields(str)
	if len(f) < 13 {
		return s, false
	}
	ut, _ := strconv.ParseFloat(f[11], 64)
	st, _ := strconv.ParseFloat(f[12], 64)
	s.cpuTicks = ut + st
	sb, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	for _, l := range strings.Split(string(sb), "\n") {
		if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			s.rssKB, _ = strconv.ParseInt(strings.Fields(v)[0], 10, 64)
		}
		if v, ok := strings.CutPrefix(l, "VmHWM:"); ok {
			s.hwmKB, _ = strconv.ParseInt(strings.Fields(v)[0], 10, 64)
		}
	}
	return s, true
}

func dbBytes(path string) int64 {
	var t int64
	for _, suf := range []string{"", "-wal"} {
		if fi, err := os.Stat(path + suf); err == nil {
			t += fi.Size()
		}
	}
	return t
}

func fetchStats(base string) map[string]float64 {
	resp, err := http.Get(base + "/debug/stats")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	m := map[string]float64{}
	json.NewDecoder(resp.Body).Decode(&m)
	return m
}

// ---- run ----

type Report struct {
	Scale        float64                `json:"scale"`
	Active       int                    `json:"active_clients"`
	Seconds      float64                `json:"measure_seconds"`
	Classes      map[string]ClassResult `json:"classes"`
	Counters     map[string]int64       `json:"counters"`
	ServerCPU    float64                `json:"seatimed_cpu_cores"`
	ProxyCPU     float64                `json:"proxy_cpu_cores,omitempty"`
	LoadCPU      float64                `json:"loadgen_cpu_cores"`
	RSSMaxMB     float64                `json:"seatimed_rss_peak_mb"`
	RSSEndMB     float64                `json:"seatimed_rss_end_mb"`
	DBStart      int64                  `json:"db_bytes_start"`
	DBEnd        int64                  `json:"db_bytes_end"`
	UserHours    float64                `json:"sim_user_hours"`
	TableDelta   map[string]int64       `json:"table_bytes_delta,omitempty"`
	RowDelta     map[string]int64       `json:"table_rows_delta,omitempty"`
	ServerStats  map[string]float64     `json:"server_stats_delta,omitempty"`
	SSEOpen      int64                  `json:"sse_open_end"`
	MinFreeGB    float64                `json:"min_free_gb"`
	LoadRSSMaxMB float64                `json:"loadgen_rss_peak_mb"`
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	base := fs.String("url", "http://127.0.0.1:8796", "seatimed base URL")
	debugURL := fs.String("debug-url", "http://127.0.0.1:6060", "seatimed's debug listener (SEATIME_DEBUG_ADDR) for /debug/stats")
	usersF := fs.String("users", "", "users file from `seed`")
	scale := fs.Float64("scale", 1, "multiple of the modelled peak load")
	warm := fs.Duration("warmup", 45*time.Second, "unmeasured ramp")
	dur := fs.Duration("duration", 3*time.Minute, "measured window")
	pid := fs.Int("pid", 0, "seatimed pid (cpu/rss sampling)")
	pid2 := fs.Int("pid2", 0, "reverse proxy pid (optional)")
	dbPath := fs.String("db", "", "database path (size growth, disk guard)")
	minFree := fs.Float64("min-free-gb", 1.2, "abort if free disk drops below this")
	maxDB := fs.Float64("max-db-gb", 1.5, "abort if db+wal exceeds this")
	rssKill := fs.Float64("rss-kill-mb", 650, "SIGTERM seatimed if its RSS exceeds this")
	register := fs.Int("register", 60, "device registrations during the window")
	subRate := fs.Float64("submit-rate", 0.05, "submissions/s at 1x (inflated vs. reality so short runs get samples)")
	sseUserFrac := fs.Float64("sse-user", 0.12, "live profile viewers as a fraction of active clients")
	sseGlobalFrac := fs.Float64("sse-global", 0.05, "live home-page viewers as a fraction of active clients")
	out := fs.String("json", "", "write the report here")
	fs.Parse(args)

	raw, err := os.ReadFile(*usersF)
	if err != nil {
		fatalf("%v", err)
	}
	var users []UserRec
	json.Unmarshal(raw, &users)
	m := buildModel()
	nActive := int(math.Round(m.PeakConcurrent * *scale))
	var subs []*submitter
	var pool []int
	for i := range users {
		if users[i].Submitter {
			pb, _ := base64.StdEncoding.DecodeString(users[i].Priv)
			subs = append(subs, &submitter{u: &users[i], priv: ed25519.PrivateKey(pb), head: chain.Genesis})
		} else {
			pool = append(pool, i)
		}
	}
	if nActive > len(pool) {
		fatalf("need %d active users but only %d non-submitter users", nActive, len(pool))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	col := newCollector()
	mkTr := func(keep bool) *http.Transport {
		return &http.Transport{MaxIdleConns: 4096, MaxIdleConnsPerHost: 4096, IdleConnTimeout: 90 * time.Second, DisableKeepAlives: !keep}
	}
	g := &gen{base: strings.TrimRight(*base, "/"), debug: strings.TrimRight(*debugURL, "/"), col: col, users: users, ctx: ctx, scale: *scale,
		rd:  &http.Client{Transport: mkTr(true), Timeout: 0, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		wr:  &http.Client{Transport: mkTr(false), Timeout: 60 * time.Second},
		rng: mrand.New(mrand.NewPCG(uint64(time.Now().UnixNano()), 1))}
	g.zipf = mrand.NewZipf(g.rng, 1.15, 4, uint64(len(pool)-1))
	// Zipf index -> any user; shuffle so popular profiles are not the sync set
	// (they overlap in reality, which only makes caches look better).

	// disk / memory guard
	var minFreeSeen int64 = 1 << 60
	var rssPeak, loadRSS int64
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if *dbPath != "" {
				f := freeBytes(*dbPath)
				if f < minFreeSeen {
					minFreeSeen = f
				}
				if float64(f) < *minFree*1e9 || float64(dbBytes(*dbPath)) > *maxDB*1e9 {
					fmt.Fprintf(os.Stderr, "GUARD: free=%.2fGB db=%.2fGB, aborting\n", float64(f)/1e9, float64(dbBytes(*dbPath))/1e9)
					if *pid > 0 {
						killPID(*pid)
					}
					os.Exit(3)
				}
			}
			if *pid > 0 {
				if s, ok := readProc(*pid); ok {
					if s.rssKB > rssPeak {
						rssPeak = s.rssKB
					}
					if float64(s.rssKB)/1024 > *rssKill {
						fmt.Fprintf(os.Stderr, "GUARD: seatimed RSS %dMB, aborting\n", s.rssKB/1024)
						killPID(*pid)
						os.Exit(4)
					}
				}
			}
			if s, ok := readProc(os.Getpid()); ok && s.rssKB > loadRSS {
				loadRSS = s.rssKB
			}
		}
	}()

	fmt.Fprintf(os.Stderr, "scale %.1fx: %d active clients (%.1f syncs/s), %d submitters; warmup %s + measure %s\n",
		*scale, nActive, float64(nActive)/90, len(subs), *warm, *dur)
	perm := g.rng.Perm(len(pool))
	for i := 0; i < nActive; i++ {
		c := newSimClient(g, pool[perm[i]])
		g.active = append(g.active, pool[perm[i]])
		go c.run(time.Duration(g.rnd() * float64(90*time.Second)))
	}
	g.startReaders(map[string]float64{"profile": 0.8, "project": 0.3, "leaderboard": 0.4, "home": 0.4, "stats": 1.0})
	// live viewers: mostly your own profile while you work
	nU, nG := int(math.Round(*sseUserFrac*float64(nActive))), int(math.Round(*sseGlobalFrac*float64(nActive)))
	for i := 0; i < nU; i++ {
		var u *UserRec
		if g.rnd() < 0.7 {
			u = &users[g.active[g.rndInt(len(g.active))]]
		} else {
			u = &users[pool[g.rndInt(len(pool))]]
		}
		go g.sseWatch("/live/@"+u.Name, "sync", "/@"+u.Name+"?frag=1")
		time.Sleep(2 * time.Millisecond)
	}
	for i := 0; i < nG; i++ {
		go g.sseWatch("/live/global", "stats", "")
		time.Sleep(2 * time.Millisecond)
	}
	g.poisson(*subRate**scale, func() {
		for i := 0; i < len(subs); i++ {
			s := subs[g.rndInt(len(subs))]
			if s.busy.CompareAndSwap(false, true) {
				defer s.busy.Store(false)
				g.submit(s)
				return
			}
		}
	})

	time.Sleep(*warm)
	// ---- measured window ----
	var db0 []TableSize
	var db0bytes int64
	if *dbPath != "" {
		db0bytes = dbBytes(*dbPath)
		db0, _ = dbStat(*dbPath) // scans the file; done before the CPU baseline
	}
	var t0cpu, t0cpu2 procSample
	if *pid > 0 {
		t0cpu, _ = readProc(*pid)
	}
	if *pid2 > 0 {
		t0cpu2, _ = readProc(*pid2)
	}
	loadCPU0 := selfCPU()
	st0 := fetchStats(g.debug)
	col.on.Store(true)
	start := time.Now()
	g.poisson(float64(*register)/dur.Seconds(), func() { g.register(0) })
	time.Sleep(*dur)
	secs := time.Since(start).Seconds()
	col.on.Store(false)
	rep := Report{Scale: *scale, Active: nActive, Seconds: secs, Classes: col.results(secs), Counters: col.extra, SSEOpen: g.sseOpen.Load()}
	if *pid > 0 {
		t1, _ := readProc(*pid)
		rep.ServerCPU = (t1.cpuTicks - t0cpu.cpuTicks) / 100 / secs
		rep.RSSEndMB = float64(t1.rssKB) / 1024
		rep.RSSMaxMB = float64(max(rssPeak, t1.hwmKB)) / 1024
	}
	if *pid2 > 0 {
		t1, _ := readProc(*pid2)
		rep.ProxyCPU = (t1.cpuTicks - t0cpu2.cpuTicks) / 100 / secs
	}
	rep.LoadCPU = (selfCPU() - loadCPU0) / secs
	rep.LoadRSSMaxMB = float64(loadRSS) / 1024
	rep.UserHours = float64(nActive) * secs / 3600
	rep.MinFreeGB = float64(minFreeSeen) / 1e9
	if *dbPath != "" {
		rep.DBStart, rep.DBEnd = db0bytes, dbBytes(*dbPath)
		db1, _ := dbStat(*dbPath)
		rep.TableDelta, rep.RowDelta = map[string]int64{}, map[string]int64{}
		old := map[string]TableSize{}
		for _, t := range db0 {
			old[t.Name] = t
		}
		for _, t := range db1 {
			rep.TableDelta[t.Name] = t.Bytes - old[t.Name].Bytes
			rep.RowDelta[t.Name] = t.Rows - old[t.Name].Rows
		}
	}
	if st1 := fetchStats(g.debug); st1 != nil && st0 != nil {
		rep.ServerStats = map[string]float64{}
		for k, v := range st1 {
			rep.ServerStats[k] = v - st0[k]
		}
	}
	cancel()
	printReport(&rep)
	if *out != "" {
		b, _ := json.MarshalIndent(rep, "", " ")
		os.WriteFile(*out, b, 0o644)
	}
}

func printReport(r *Report) {
	fmt.Printf("\n== %.1fx: %d active clients, %.0fs measured ==\n", r.Scale, r.Active, r.Seconds)
	names := make([]string, 0, len(r.Classes))
	for k := range r.Classes {
		names = append(names, k)
	}
	sort.Strings(names)
	fmt.Printf("%-18s %7s %7s %8s %8s %8s %9s  codes\n", "class", "n", "/s", "p50ms", "p95ms", "p99ms", "max ms")
	for _, k := range names {
		c := r.Classes[k]
		fmt.Printf("%-18s %7d %7.2f %8.1f %8.1f %8.1f %9.1f  %v\n", k, c.N, c.Rate, c.P50, c.P95, c.P99, c.Max, c.Codes)
	}
	fmt.Printf("seatimed cpu %.2f cores, rss peak %.0f MB (end %.0f), proxy cpu %.2f, loadgen cpu %.2f cores rss %.0f MB, sse open %d\n",
		r.ServerCPU, r.RSSMaxMB, r.RSSEndMB, r.ProxyCPU, r.LoadCPU, r.LoadRSSMaxMB, r.SSEOpen)
	fmt.Printf("db %.1f -> %.1f MB over %.0f sim user-hours; min free disk %.2f GB\n", float64(r.DBStart)/1e6, float64(r.DBEnd)/1e6, r.UserHours, r.MinFreeGB)
	fmt.Printf("counters %v\nserver %v\ntable bytes delta %v\nrow delta %v\n", r.Counters, r.ServerStats, r.TableDelta, r.RowDelta)
}
