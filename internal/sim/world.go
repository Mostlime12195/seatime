// Package sim is seatime's end-to-end simulator. It drives the real client
// pipeline (harness hook handlers, the daemon's local HTTP surface, the emit
// pipeline, the SQLite event store and hash chain, the syncer and the submit
// bundle builder) against a real seatimed (internal/server over httptest) under
// a fake clock, so a six-hour working day runs in about a second.
//
// Two clocks matter. World.Clock is true time: the server stamps checkpoints
// with it and the attacker cannot touch it. Each Device has its own clock
// (true time plus an optional, attacker-controlled skew) that the client
// pipeline, the syncer and the web-chat translator use.
//
// Personas (honest users and attacks) are in personas.go and attacks.go.
package sim

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"seatime/internal/server"
	srvstore "seatime/internal/server/store"
	"seatime/pkg/api"
)

// Clock is the simulation's true time.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *Clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// Epoch is where every simulation starts: 09:00 UTC tomorrow. It must not be
// in the real past, because a few server store paths (device-code expiry)
// compare against the real clock.
var Epoch = time.Now().UTC().Truncate(24 * time.Hour).Add(33 * time.Hour)

// World is one seatimed plus the devices talking to it.
type World struct {
	Dir   string
	Clock *Clock
	Srv   *server.Server
	HTTP  *httptest.Server
	Store *srvstore.DB

	devices []*Device
	users   map[string]*http.Client // dev-login browser per username
	nDev    int

	approvals map[string]int // device approvals per user (seatimed rate-limits them)
}

// WorldOpts tweak a World (the demo-database builder uses them).
type WorldOpts struct {
	Start time.Time // the fake clock's start (default Epoch)
	// ReviewKey, when set, is a key the server's review API takes.
	ReviewKey string
}

// UserAlias, when set, renames every simulated person (account name and the
// owner in project keys) as the personas create them. The demo builder uses
// it so accounts are called demo-<persona>. It must be idempotent.
var UserAlias func(string) string

func aliasUser(u string) string {
	if UserAlias != nil {
		return UserAlias(u)
	}
	return u
}

// NewWorld starts a seatimed (dev mode, GitHub disabled) under dir.
func NewWorld(dir string) (*World, error) { return NewWorldWith(dir, WorldOpts{}) }

// NewWorldWith is NewWorld with options.
func NewWorldWith(dir string, wo WorldOpts) (*World, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	st, err := srvstore.Open(filepath.Join(dir, "seatimed.db"))
	if err != nil {
		return nil, err
	}
	start := wo.Start
	if start.IsZero() {
		start = Epoch
	}
	w := &World{Dir: dir, Clock: &Clock{t: start}, Store: st, users: map[string]*http.Client{}}
	cfg := server.Config{Dev: true, BaseURL: "http://seatime.sim", Secret: "sim-secret", Admins: map[string]bool{}}
	if wo.ReviewKey != "" {
		cfg.ReviewKeys = []string{wo.ReviewKey}
	}
	srv, err := server.New(cfg,
		st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		st.Close()
		return nil, err
	}
	srv.SetClock(w.Clock.Now)
	w.Srv = srv
	w.HTTP = httptest.NewServer(srv.Handler())
	return w, nil
}

func (w *World) Close() {
	for _, d := range w.devices {
		if d.Store != nil {
			d.Store.Close()
		}
	}
	w.HTTP.Close()
	w.Store.Close()
}

// Now is true (server) time.
func (w *World) Now() time.Time { return w.Clock.Now() }

// Advance moves true time forward by d, running each online device's sync
// loop (every SyncEvery, like the daemon's 90 s loop) on the way.
func (w *World) Advance(d time.Duration) {
	end := w.Now().Add(d)
	for {
		next := end
		var due *Device
		idleDue, sliceDue := false, false
		for _, dv := range w.devices {
			if dv.SyncEvery > 0 && dv.nextSync.Before(next) {
				next, due, idleDue, sliceDue = dv.nextSync, dv, false, false
			}
			// the daemon reads the idle timer once a minute while it runs,
			// and every 5 s while a project is active
			if dv.SyncEvery > 0 && dv.syncFn == nil && dv.idleRec != nil && dv.nextIdle.Before(next) {
				next, due, idleDue, sliceDue = dv.nextIdle, dv, true, false
			}
			if dv.SyncEvery > 0 && dv.syncFn == nil && dv.idleRec != nil && dv.nextSlice.Before(next) {
				next, due, idleDue, sliceDue = dv.nextSlice, dv, false, true
			}
		}
		if due == nil {
			w.Clock.set(end)
			return
		}
		if next.After(w.Now()) {
			w.Clock.set(next)
		}
		switch {
		case sliceDue:
			due.sliceTick()
		case idleDue:
			due.idleTick()
		case due.syncFn != nil:
			due.nextSync = next.Add(due.SyncEvery)
			due.syncFn()
		default:
			due.nextSync = next.Add(due.SyncEvery)
			due.syncQuiet()
		}
	}
}

// Every runs f every interval of true time (a script on a timer, e.g. an
// attacker's mirror). It returns a stop function.
func (w *World) Every(interval time.Duration, f func()) (stop func()) {
	p := &Device{W: w, Name: "timer", SyncEvery: interval, nextSync: w.Now().Add(interval), syncFn: f}
	w.devices = append(w.devices, p)
	return func() { p.SyncEvery = 0 }
}

// ---- dev-login browser (device approval) ----

var csrfRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func (w *World) browser(user string) (*http.Client, error) {
	if c := w.users[user]; c != nil {
		return c, nil
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	tok, err := w.csrf(c, "/login")
	if err != nil {
		return nil, err
	}
	if code, body, err := w.post(c, "/auth/dev", url.Values{"csrf": {tok}, "username": {user}, "role": {"user"}}); err != nil || code != 200 {
		return nil, fmt.Errorf("dev login %s: %d %v %.200s", user, code, err, body)
	}
	w.users[user] = c
	return c, nil
}

func (w *World) csrf(c *http.Client, path string) (string, error) {
	resp, err := c.Get(w.HTTP.URL + path)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	m := csrfRe.FindSubmatch(b)
	if m == nil {
		return "", fmt.Errorf("no csrf token on %s", path)
	}
	return string(m[1]), nil
}

func (w *World) post(c *http.Client, path string, form url.Values) (int, string, error) {
	resp, err := c.PostForm(w.HTTP.URL+path, form)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

// approve logs user in (dev login) and approves a device user code, as the
// person does in their browser during `seatime login`.
func (w *World) approve(user, userCode string) error {
	// seatimed rate-limits device-code attempts per user (burst 8, then one per
	// 5 s; the confirm page and the approval each take one)
	if w.approvals == nil {
		w.approvals = map[string]int{}
	}
	if w.approvals[user]++; w.approvals[user] > 4 {
		w.Advance(10 * time.Second)
	}
	c, err := w.browser(user)
	if err != nil {
		return err
	}
	tok, err := w.csrf(c, "/device?code="+userCode)
	if err != nil {
		return err
	}
	if code, body, err := w.post(c, "/device/approve", url.Values{"csrf": {tok}, "code": {userCode}}); err != nil || code != 200 || !strings.Contains(body, "Approved") {
		return fmt.Errorf("approve: %d %v %.200s", code, err, body)
	}
	return nil
}

// ---- public reads (what a YSWS program sees) ----

func (w *World) getJSON(path string, v any) (int, error) {
	w.Advance(200 * time.Millisecond) // stay under the per-IP API rate limit
	resp, err := http.Get(w.HTTP.URL + path)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return resp.StatusCode, fmt.Errorf("GET %s: %d %.200s", path, resp.StatusCode, b)
	}
	return 200, json.Unmarshal(b, v)
}

// Export fetches the YSWS export of a user's project (server key).
func (w *World) Export(user, project string) (*api.YSWSExport, error) {
	var x api.YSWSExport
	if _, err := w.getJSON("/api/v1/users/"+aliasUser(user)+"/projects/"+url.PathEscape(project)+"/ysws", &x); err != nil {
		return nil, err
	}
	return &x, nil
}

// CompatHours is what an application using the Hackatime-compatible stats API sees
// for a user's project over all time (struggle hours, the default metric).
func (w *World) CompatHours(user, projectName string) float64 {
	user = aliasUser(user)
	var out struct {
		Data struct {
			TotalSeconds float64 `json:"total_seconds"`
		} `json:"data"`
	}
	q := url.Values{"features": {"projects"}, "start_date": {"2020-01-01"}, "end_date": {"2030-01-01"}, "filter_by_project": {projectName}}
	if _, err := w.getJSON("/api/v1/users/"+user+"/stats?"+q.Encode(), &out); err != nil {
		return -1
	}
	return out.Data.TotalSeconds / 3600
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ = context.Background

// Sandbox isolates the process from the real machine: no real ~/.claude,
// ~/.wakatime.cfg or SeaTime data dir is ever touched, and the harness
// environment of whoever runs the simulator (e.g. a `claude -p` agent, which
// the Claude Code adapter would drop as headless) doesn't leak into hooks.
func Sandbox(dir string) {
	_ = os.MkdirAll(dir, 0o700)
	for k, v := range map[string]string{
		"SEATIME_FAKE_HOME": dir, "HOME": dir, "SEATIME_HOME": filepath.Join(dir, "seatime"), "XDG_DATA_HOME": filepath.Join(dir, "data"),
		"XDG_CONFIG_HOME": filepath.Join(dir, "config"),
	} {
		os.Setenv(k, v)
	}
	for _, k := range []string{"SEATIME_SERVER", "SEATIME_IGNORE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ATTENDED"} {
		os.Unsetenv(k)
	}
}

// SegmentRaw is a segment exactly as the server stores it (the bundle the
// device uploaded, as JSON).
func (w *World) SegmentRaw(d *Device, id string) ([]byte, error) {
	gz, err := w.Store.SegmentBundle(context.Background(), id)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}
