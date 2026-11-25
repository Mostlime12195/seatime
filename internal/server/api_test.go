package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"seatime/internal/server/store"
	"seatime/internal/server/store/storetest"
	"seatime/pkg/api"
	"seatime/pkg/audit"
	"seatime/pkg/chain"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

type tenv struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
	st  *store.DB
}

func newEnv(t *testing.T) *tenv {
	t.Helper()
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := New(Config{Dev: true, BaseURL: "http://example.test", Secret: "test-secret",
		Admins: map[string]bool{}}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &tenv{t, srv, ts, st}
}

// browser is a cookie-carrying client that logs in through the dev form.
type browser struct {
	e *tenv
	c *http.Client
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func (e *tenv) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{e, &http.Client{Jar: jar}}
}

func (b *browser) get(path string) (int, string) {
	b.e.t.Helper()
	resp, err := b.c.Get(b.e.ts.URL + path)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (b *browser) csrf(path string) string {
	b.e.t.Helper()
	_, body := b.get(path)
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		b.e.t.Fatalf("no csrf token on %s:\n%s", path, body)
	}
	return m[1]
}

func (b *browser) post(path string, form url.Values) (int, string) {
	b.e.t.Helper()
	resp, err := b.c.PostForm(b.e.ts.URL+path, form)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (b *browser) login(name, role string) {
	b.e.t.Helper()
	tok := b.csrf("/login")
	code, body := b.post("/auth/dev", url.Values{"csrf": {tok}, "username": {name}, "role": {role}})
	if code != 200 { // follows redirect to "/"
		b.e.t.Fatalf("login: %d %s", code, body)
	}
}

// device is a fake SeaTime client.
type device struct {
	e     *tenv
	pub   ed25519.PublicKey
	priv  ed25519.PrivateKey
	token string
	id    string
	user  string
}

func (e *tenv) apiDo(method, path, token string, in any) (int, []byte) {
	e.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// enroll runs the whole device-code flow for a fresh dev user.
func (e *tenv) enroll(username string) (*device, *browser) {
	e.t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	code, out := e.apiDo("POST", "/api/v1/device/start", "", api.DeviceStartReq{DeviceName: "laptop", OS: "linux", PublicKey: base64.StdEncoding.EncodeToString(pub)})
	if code != 200 {
		e.t.Fatalf("start: %d %s", code, out)
	}
	var st api.DeviceStartResp
	json.Unmarshal(out, &st)
	if code, _ := e.apiDo("POST", "/api/v1/device/poll", "", api.DevicePollReq{DeviceCode: st.DeviceCode}); code != 202 {
		e.t.Fatalf("pending poll should be 202, got %d", code)
	}
	br := e.browser()
	br.login(username, "user")
	tok := br.csrf("/device?code=" + st.UserCode)
	if c, body := br.post("/device", url.Values{"csrf": {tok}, "code": {st.UserCode}}); c != 200 || !strings.Contains(body, "laptop") {
		e.t.Fatalf("confirm page: %d %s", c, body)
	}
	if c, body := br.post("/device/approve", url.Values{"csrf": {tok}, "code": {st.UserCode}}); c != 200 || !strings.Contains(body, "Approved") {
		e.t.Fatalf("approve: %d %s", c, body)
	}
	code, out = e.apiDo("POST", "/api/v1/device/poll", "", api.DevicePollReq{DeviceCode: st.DeviceCode})
	if code != 200 {
		e.t.Fatalf("poll: %d %s", code, out)
	}
	var pr api.DevicePollResp
	json.Unmarshal(out, &pr)
	if pr.Username != username || !strings.HasPrefix(pr.Token, "grt_") {
		e.t.Fatalf("%+v", pr)
	}
	// the code is single use
	if code, _ := e.apiDo("POST", "/api/v1/device/poll", "", api.DevicePollReq{DeviceCode: st.DeviceCode}); code != 404 {
		e.t.Fatalf("reused code: %d", code)
	}
	return &device{e, pub, priv, pr.Token, pr.DeviceID, username}, br
}

func (d *device) sign(cp chain.Checkpoint) string {
	msg := fmt.Sprintf("seatime-checkpoint\n%s\n%d\n%s", d.id, cp.Seq, cp.Hash)
	return base64.StdEncoding.EncodeToString(ed25519.Sign(d.priv, []byte(msg)))
}

func (d *device) sync(cp chain.Checkpoint, bs []ledger.Bucket, extra func(*api.SyncReq)) (int, string) {
	req := api.SyncReq{DeviceID: d.id, ClientVersion: "test", ScoreVersion: ledger.Version, Checkpoint: cp, CheckpointSig: d.sign(cp), Buckets: bs}
	if extra != nil {
		extra(&req)
	}
	code, out := d.e.apiDo("POST", "/api/v1/sync", d.token, req)
	return code, string(out)
}

func minute(ago int) time.Time {
	return time.Now().UTC().Truncate(time.Minute).Add(-time.Duration(ago) * time.Minute)
}

func h(i int) string { return fmt.Sprintf("%064x", i) }

func TestDeviceFlowAndRevoke(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("alice")
	if code, out := d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, nil, nil); code != 200 {
		t.Fatalf("sync: %d %s", code, out)
	}
	if code, _ := e.apiDo("POST", "/api/v1/sync", "", api.SyncReq{}); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	tok := br.csrf("/settings")
	if c, _ := br.post("/settings/device", url.Values{"csrf": {tok}, "id": {d.id}}); c != 200 {
		t.Fatalf("revoke: %d", c)
	}
	if code, _ := d.sync(chain.Checkpoint{Seq: 2, Hash: h(2)}, nil, nil); code != 401 {
		t.Fatalf("revoked token should be rejected, got %d", code)
	}
	// CSRF is enforced on settings forms
	if c, _ := br.post("/settings/profile", url.Values{"display_name": {"x"}}); c != 403 {
		t.Fatalf("missing csrf should be 403, got %d", c)
	}
}

func TestSyncIdempotentAndStats(t *testing.T) {
	e := newEnv(t)
	d, br := e.enroll("bob")
	m := minute(5)
	bs := []ledger.Bucket{
		{Project: "github.com/bob/app", Start: m, CodingSeconds: 40, AINotCountedSeconds: 10, Prompts: 2},
		{Project: "github.com/bob/app", Start: m.Add(time.Minute), CodingSeconds: 30},
	}
	meta := func(r *api.SyncReq) {
		r.Projects = []api.ProjectMeta{{Key: "github.com/bob/app", Name: "app", Remote: "https://github.com/bob/app"}}
		r.Commits = []api.CommitRef{{Project: "github.com/bob/app", SHA: strings.Repeat("a", 40), TS: m, Ins: 3, Del: 1}}
	}
	for i := 0; i < 3; i++ { // same payload repeatedly must not double count
		if code, out := d.sync(chain.Checkpoint{Seq: 10, Hash: h(10)}, bs, meta); code != 200 {
			t.Fatalf("sync %d: %d %s", i, code, out)
		}
	}
	stats := func() api.ProjectStats {
		code, out := e.apiDo("GET", "/api/v1/users/bob/projects/app/stats", "", nil)
		if code != 200 {
			t.Fatalf("stats: %d %s", code, out)
		}
		var s api.ProjectStats
		json.Unmarshal(out, &s)
		return s
	}
	// synced time is not checked yet: applications see it as pending, not as hours
	if s := stats(); s.Status != api.StatusPending || s.PendingSeconds != 70 || s.HumanSeconds != 0 || s.Commits != 1 || len(s.CommitList) != 1 {
		t.Fatalf("%+v", s)
	}
	// recomputed bucket replaces the old one
	bs[0].CodingSeconds, bs[0].AINotCountedSeconds = 50, 0
	d.sync(chain.Checkpoint{Seq: 11, Hash: h(11)}, bs[:1], nil)
	if s := stats(); s.PendingSeconds != 80 {
		t.Fatalf("%+v", s)
	}
	// invalid buckets are skipped, not stored
	code, out := d.sync(chain.Checkpoint{Seq: 12, Hash: h(12)}, []ledger.Bucket{{Project: "x", Start: m, CodingSeconds: 9999}}, nil)
	if code != 200 || !strings.Contains(out, "skipped") {
		t.Fatalf("%d %s", code, out)
	}
	// pages render with the data
	for path, want := range map[string]string{
		"/@bob":        "bob/app",
		"/@bob/app":    "aaaaaaaa",
		"/":            "SeaTime",
		"/leaderboard": "@bob",
		"/settings":    "bob/app",
	} {
		c, body := br.get(path)
		if c != 200 || !strings.Contains(body, want) {
			t.Errorf("%s: %d, missing %q", path, c, want)
		}
	}
	// the 80 s are not checked yet, and the pages say so
	for _, path := range []string{"/", "/@bob", "/@bob/app"} {
		if _, body := br.get(path); !strings.Contains(body, fmtDur(80)+" unchecked") && !strings.Contains(body, "<dt>Unchecked</dt><dd>"+fmtDur(80)) {
			t.Errorf("%s does not label the unchecked time", path)
		}
	}
	// live fragment endpoint
	if c, body := br.get("/@bob?frag=1"); c != 200 || strings.Contains(body, "<html") || !strings.Contains(body, "<span>today</span>") {
		t.Errorf("frag: %d %q", c, body[:min(80, len(body))])
	}
	// privacy toggle: profile disappears for anonymous visitors, stats too
	tok := br.csrf("/settings")
	br.post("/settings/profile", url.Values{"csrf": {tok}, "display_name": {"Bob"}, "tz": {"0"}})
	anon := e.browser()
	if c, _ := anon.get("/@bob"); c != 404 {
		t.Errorf("private profile visible: %d", c)
	}
	if code, _ := e.apiDo("GET", "/api/v1/users/bob/projects/app/stats", "", nil); code != 403 {
		t.Errorf("private stats visible: %d", code)
	}
	if c, _ := br.get("/@bob"); c != 200 {
		t.Errorf("owner can't see own private profile: %d", c)
	}
}

func TestCheckpointRegressionRejected(t *testing.T) {
	e := newEnv(t)
	d, _ := e.enroll("carol")
	if code, out := d.sync(chain.Checkpoint{Seq: 100, Hash: h(100)}, nil, nil); code != 200 {
		t.Fatal(code, out)
	}
	if code, _ := d.sync(chain.Checkpoint{Seq: 100, Hash: h(100)}, nil, nil); code != 200 {
		t.Fatal("identical resend must be idempotent")
	}
	if code, _ := d.sync(chain.Checkpoint{Seq: 100, Hash: h(999)}, nil, nil); code != 409 {
		t.Fatalf("same seq different hash: %d", code)
	}
	if code, _ := d.sync(chain.Checkpoint{Seq: 50, Hash: h(50)}, nil, nil); code != 409 {
		t.Fatalf("seq regression: %d", code)
	}
	// forged signature
	req := api.SyncReq{DeviceID: d.id, Checkpoint: chain.Checkpoint{Seq: 200, Hash: h(200)}, CheckpointSig: d.sign(chain.Checkpoint{Seq: 201, Hash: h(200)})}
	if code, _ := e.apiDo("POST", "/api/v1/sync", d.token, req); code != 400 {
		t.Fatalf("bad sig: %d", code)
	}
	// another user's device id with my token
	req = api.SyncReq{DeviceID: "someoneelse", Checkpoint: chain.Checkpoint{Seq: 300, Hash: h(300)}}
	if code, _ := e.apiDo("POST", "/api/v1/sync", d.token, req); code != 403 {
		t.Fatalf("device mismatch: %d", code)
	}
}

// buildChain makes n events and returns them with every head hash (heads[i] is the head after seq i+1).
func buildChain(t *testing.T, n int) ([]event.Event, []string) {
	t.Helper()
	base := time.Now().UTC().Add(-time.Duration(n+5) * time.Minute).Truncate(time.Millisecond)
	prev := chain.Genesis
	var evs []event.Event
	var heads []string
	for i := 1; i <= n; i++ {
		e := event.Event{Seq: int64(i), TS: base.Add(time.Duration(i) * time.Minute), Kind: event.Prompt, Harness: "claude-code",
			SessionID: "s1", Project: "github.com/dan/proj", Text: fmt.Sprintf("why does thing %d fail with a nil pointer?", i), Words: 9, Chars: 40, Novelty: 0.8}
		if i%4 == 0 {
			e.Kind, e.Text = event.Commit, ""
			e.SHA = strings.Repeat("b", 40)
		}
		var err error
		if prev, err = chain.Next(prev, e); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
		heads = append(heads, prev)
	}
	return evs, heads
}

func (d *device) bundle(evs []event.Event, startHash string, hide func(i int) bool) api.Bundle {
	var entries []chain.Entry
	var shown []event.Event
	for i := range evs {
		e := evs[i]
		if hide != nil && hide(i) {
			l, _ := chain.Leaf(e)
			entries = append(entries, chain.Entry{Seq: e.Seq, LeafHash: l})
		} else {
			entries = append(entries, chain.Entry{Seq: e.Seq, Event: &e})
			shown = append(shown, e)
		}
	}
	b := api.Bundle{DeviceID: d.id, Project: "github.com/dan/proj", StartHash: startHash, Entries: entries,
		ScoreVersion: ledger.Version, Claimed: audit.Claim(shown, "github.com/dan/proj")}
	d.resign(&b)
	return b
}

func (d *device) resign(b *api.Bundle) {
	dg := audit.Digest(*b)
	b.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(d.priv, dg[:]))
}

func TestSegmentVerifyAndTamper(t *testing.T) {
	e := newEnv(t)
	d, _ := e.enroll("dan")
	evs, heads := buildChain(t, 12)
	// the client synced checkpoints at seq 6 and 12 in real time
	for _, s := range []int{6, 12} {
		if code, out := d.sync(chain.Checkpoint{Seq: int64(s), Hash: heads[s-1]}, nil, nil); code != 200 {
			t.Fatal(code, out)
		}
	}
	send := func(b api.Bundle) (int, api.SegmentResp) {
		code, out := e.apiDo("POST", "/api/v1/segments", d.token, b)
		var r api.SegmentResp
		json.Unmarshal(out, &r)
		if code != 200 {
			t.Logf("segment: %d %s", code, out)
		}
		return code, r
	}

	// good: some entries hidden (leaf only), the rest revealed
	good := d.bundle(evs, chain.Genesis, func(i int) bool { return i == 2 || i == 7 })
	code, r := send(good)
	if code != 200 || r.Status != api.StatusVerified || len(r.Problems) != 0 {
		t.Fatalf("expected verified: %d %+v", code, r)
	}
	// tamper: edit a revealed prompt, re-sign. Chain no longer matches the checkpoints.
	bad := d.bundle(evs, chain.Genesis, nil)
	bad.Entries[4].Event.Text = "totally different prompt text"
	d.resign(&bad)
	code, r = send(bad)
	if code != 200 || r.Status != api.StatusFailed || r.Hours != 0 || !strings.Contains(strings.Join(r.Problems, "|"), "checkpoint mismatch") {
		t.Fatalf("tamper must fail: %d %+v", code, r)
	}
	// stolen/forged signature is rejected outright
	forged := d.bundle(evs, chain.Genesis, nil)
	forged.Entries[0].Event.Text = "edited after signing"
	if code, _ := send(forged); code != 400 {
		t.Fatalf("bad bundle sig: %d", code)
	}
	// a range holding no server checkpoint can't be verified (events 7..11 only)
	nocp := d.bundle(evs[6:11], heads[5], nil)
	code, r = send(nocp)
	if code != 200 || r.Status != api.StatusFailed || !strings.Contains(strings.Join(r.Problems, "|"), "no server-held checkpoint") {
		t.Fatalf("no-checkpoint range: %d %+v", code, r)
	}
	// wrong start hash
	badStart := d.bundle(evs[6:12], h(5), nil)
	if _, r = send(badStart); r.Status != api.StatusFailed {
		t.Fatalf("bad start hash accepted")
	}
	// anti-backfill: events dated after the server first saw the covering checkpoint
	late := make([]event.Event, len(evs))
	copy(late, evs)
	prev := chain.Genesis
	var lateHeads []string
	for i := range late {
		late[i].TS = time.Now().UTC().Add(3 * time.Hour)
		prev, _ = chain.Next(prev, late[i])
		lateHeads = append(lateHeads, prev)
	}
	d2, _ := e.enroll("erin")
	d2.sync(chain.Checkpoint{Seq: 12, Hash: lateHeads[11]}, nil, nil)
	b2 := d2.bundle(late, chain.Genesis, nil)
	_, out2 := e.apiDo("POST", "/api/v1/segments", d2.token, b2)
	var r2 api.SegmentResp
	json.Unmarshal(out2, &r2)
	if r2.Status != api.StatusFailed || !strings.Contains(strings.Join(r2.Problems, "|"), "backfilled") {
		t.Fatalf("future-dated events must be flagged: %+v", r2)
	}
	// the project shows failed, with the reason, to anyone who reads it
	_, st := apiJSON[api.ProjectStats](t, e, "/api/v1/users/erin/projects/proj/stats", "")
	if st.Status != api.StatusFailed || !strings.Contains(st.StatusReason, "backfilled") || st.HumanSeconds != 0 {
		t.Fatalf("project after a failed segment: %+v", st)
	}
}

// Submissions are gone: an older client gets a plain error, and the old
// read paths are not there.
func TestSubmissionsGone(t *testing.T) {
	e := newEnv(t)
	d, _ := e.enroll("ana")
	code, out := e.apiDo("POST", "/api/v1/submissions", d.token, api.Bundle{})
	if code != 410 || !strings.Contains(string(out), "no longer takes submissions") {
		t.Fatalf("old submit: %d %s", code, out)
	}
	for _, p := range []string{"/api/v1/submissions/abc", "/api/v1/submissions/abc/ysws", "/api/v1/submissions/abc/audit", "/s/abc"} {
		if code, _ := e.apiDo("GET", p, "", nil); code == 200 {
			t.Errorf("%s still answers", p)
		}
	}
}

func TestSSELiveUpdate(t *testing.T) {
	e := newEnv(t)
	d, _ := e.enroll("frank")
	req, _ := http.NewRequest("GET", e.ts.URL+"/live/@frank", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc string
		for {
			n, err := resp.Body.Read(buf)
			acc += string(buf[:n])
			if strings.Contains(acc, "event: sync") {
				got <- acc
				return
			}
			if err != nil {
				return
			}
		}
	}()
	time.Sleep(100 * time.Millisecond) // let the subscription register
	d.sync(chain.Checkpoint{Seq: 1, Hash: h(1)}, []ledger.Bucket{{Project: "p", Start: minute(1), CodingSeconds: 30}}, nil)
	select {
	case s := <-got:
		if !strings.Contains(s, `"type":"sync"`) {
			t.Fatalf("%q", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no live event")
	}
}

func TestHubDropsWhenFull(t *testing.T) {
	hub := NewHub()
	s := hub.Subscribe("t")
	for i := 0; i < subBuffer*3; i++ {
		hub.Publish("t", []byte("x")) // must never block
	}
	if len(s.C) != subBuffer {
		t.Fatalf("buffer %d", len(s.C))
	}
	s.Close()
	if hub.Subscribers() != 0 {
		t.Fatal("leak")
	}
}

func TestRateLimiter(t *testing.T) {
	l := NewLimiter()
	now := time.Now()
	l.nowFunc = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !l.Allow("k", 1, 3) {
			t.Fatal("burst should pass")
		}
	}
	if l.Allow("k", 1, 3) {
		t.Fatal("should be limited")
	}
	now = now.Add(2 * time.Second)
	if !l.Allow("k", 1, 3) {
		t.Fatal("should refill")
	}
}
