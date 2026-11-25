package sim

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"seatime/internal/adapters/claudecode"
	"seatime/internal/client/config"
	"seatime/internal/client/daemon"
	"seatime/internal/client/emit"
	"seatime/internal/client/idle"
	"seatime/internal/client/presence"
	"seatime/internal/client/segment"
	clientstore "seatime/internal/client/store"
	"seatime/internal/client/syncer"
	srvstore "seatime/internal/server/store"
	"seatime/pkg/api"
	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

// Device is one machine running the real SeaTime client: event store + hash
// chain, emit pipeline, the daemon's local HTTP handler, and the syncer.
type Device struct {
	W       *World
	Name    string
	User    string
	Home    string // fake home; projects live in Home/code/<name>
	DataDir string // SEATIME_HOME for this device

	Store   *clientstore.Store
	Pipe    *emit.Pipeline
	Daemon  *daemon.Daemon
	handler http.Handler
	Auth    config.Auth
	Syncer  *syncer.Syncer

	// Skew is the client clock's offset from true time (attacker controlled).
	Skew func(trueNow time.Time) time.Duration
	// SyncEvery is the daemon sync cadence; 0 = the daemon isn't syncing
	// (offline, daemon killed). Sync errors are recorded, not fatal.
	SyncEvery time.Duration
	nextSync  time.Time
	SyncErrs  []string

	hostOverride string
	syncFn       func() // pseudo-devices (scripts on a timer) run this instead of syncing

	// The OS idle timer, read once a minute by the daemon's real recorder
	// (internal/client/idle) while the device syncs. lastInput is the last
	// key press or mouse move of a person at the machine (Actor.touch);
	// scripts never touch it.
	idleRec   *idle.Recorder
	nextIdle  time.Time
	nextSlice time.Time
	lastInput time.Time

	// Remote: the person works over SSH, so their keys reach the tool's
	// terminal and the device's own idle timer never sees them.
	Remote bool
	// the simulated git checkouts: the current commit and when it was made
	head     map[string]string
	headTime map[string]time.Time
}

// Touch is a key press or mouse move at the machine now.
func (d *Device) Touch() {
	if !d.Remote {
		d.lastInput = d.W.Now()
	}
}

// projectActive is the daemon's test for 5 s sampling: a hand edit or a
// prompt in the last 10 minutes.
func (d *Device) projectActive() bool {
	evs, err := d.Store.Events(clientstore.Query{Kinds: []event.Kind{event.Edit, event.Prompt}, Desc: true, Limit: 1})
	return err == nil && len(evs) == 1 && d.Now().Sub(evs[0].TS) <= ledger.Window
}

func (d *Device) sliceTick() {
	d.nextSlice = d.nextSlice.Add(idle.SliceEvery)
	if !d.nextSlice.After(d.W.Now()) { // after a sleep: no catching up
		d.nextSlice = d.W.Now().Add(idle.SliceEvery)
	}
	if d.idleRec != nil && d.projectActive() {
		d.idleRec.Slice(context.Background())
	}
}

// repo is git's view of a simulated checkout at an agent stop: the files
// and lines the agent wrote there since the last commit, and that commit.
func (d *Device) repo(root string) *event.GitFacts {
	key := d.Pipe.Res.ForDir(root).Key
	evs, err := d.Store.Events(clientstore.Query{Project: key, Since: d.headTime[root], Kinds: []event.Kind{event.Tool}})
	if err != nil {
		return nil
	}
	g := &event.GitFacts{Head: d.head[root]}
	files := map[string]bool{}
	for _, e := range evs {
		if e.Write {
			g.Insertions += e.Lines
			for _, f := range e.Files {
				files[f] = true
			}
		}
	}
	g.FilesChanged = len(files)
	return g
}

func (d *Device) idleTick() {
	d.nextIdle = d.nextIdle.Add(time.Minute)
	if d.idleRec != nil {
		d.idleRec.Tick(context.Background())
	}
}

// NewDevice enrolls a device for user through the real device-code flow
// (POST /api/v1/device/start, browser approval, poll) and wires the client.
func (w *World) NewDevice(user, name string) (*Device, error) {
	user = aliasUser(user)
	w.nDev++
	dir := filepath.Join(w.Dir, fmt.Sprintf("dev%02d-%s", w.nDev, name))
	d := &Device{W: w, Name: name, User: user, Home: filepath.Join(dir, "home"), DataDir: filepath.Join(dir, "seatime"), SyncEvery: 90 * time.Second}
	if err := os.MkdirAll(d.Home, 0o700); err != nil {
		return nil, err
	}
	st, err := clientstore.Open(filepath.Join(d.DataDir, "seatime.db"))
	if err != nil {
		return nil, err
	}
	d.Store = st
	d.Pipe = emit.New(st, d.Home)
	d.Pipe.Now = d.Now
	d.Daemon = &daemon.Daemon{Store: st, Pipeline: d.Pipe, Started: d.Now(), Version: "sim"}
	d.handler = d.Daemon.Handler(config.Config{})

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	d.Auth = config.Auth{PublicKey: base64.StdEncoding.EncodeToString(pub), PrivateKey: base64.StdEncoding.EncodeToString(priv), Server: w.HTTP.URL}
	if err := d.enroll(); err != nil {
		st.Close()
		return nil, err
	}
	// every simulated user has linked their own GitHub account (same login),
	// so seatimed can tell their repos and pushes from someone else's
	if u, err := w.Store.UserByName(context.Background(), user); err == nil {
		if err := w.Store.LinkIdentity(context.Background(), u.ID, srvstore.Identity{Provider: "github", Subject: "gh-" + user}, user); err != nil {
			st.Close()
			return nil, fmt.Errorf("link github: %w", err)
		}
	}
	d.Syncer = &syncer.Syncer{Store: st, Version: "sim", Now: d.Now, Auth: &d.Auth}
	d.nextSync = w.Now().Add(d.SyncEvery)
	d.idleRec = &idle.Recorder{Emit: d.Pipe.Emit, Now: d.Now, Src: idle.Func{N: "sim", F: func(context.Context) (time.Duration, error) {
		if d.lastInput.IsZero() {
			return 24 * time.Hour, nil // untouched since long before
		}
		return d.W.Now().Sub(d.lastInput), nil
	}}}
	d.idleRec.Presence = func(ok bool) string { return presence.Level(ok, false, false, false) }
	d.idleRec.Active = d.projectActive
	d.nextIdle = w.Now().Add(time.Minute)
	d.nextSlice = w.Now().Add(idle.SliceEvery)
	// what the hook records at a prompt: the OS idle timer, which a person
	// typing the prompt has just touched and a script never has
	d.Pipe.Presence = func() presence.Facts {
		return presence.Probe{Getenv: func(string) string { return "" }, Idle: d.idleRec.Src}.AtPrompt(context.Background())
	}
	d.head, d.headTime = map[string]string{}, map[string]time.Time{}
	d.Pipe.Repo = d.repo
	w.devices = append(w.devices, d)
	return d, nil
}

func (d *Device) enroll() error {
	c := syncer.NewClient(d.W.HTTP.URL, "", "sim")
	ctx := context.Background()
	st, err := c.DeviceStart(ctx, d.Name, d.Auth.PublicKey)
	if err != nil {
		return fmt.Errorf("device start: %w", err)
	}
	if err := d.W.approve(d.User, st.UserCode); err != nil {
		return err
	}
	pr, err := c.DevicePoll(ctx, st.DeviceCode)
	if err != nil {
		return fmt.Errorf("device poll: %w", err)
	}
	d.Auth.Token, d.Auth.DeviceID, d.Auth.Username = pr.Token, pr.DeviceID, pr.Username
	return nil
}

// Now is the device's clock (true time + skew).
func (d *Device) Now() time.Time {
	t := d.W.Now()
	if d.Skew != nil {
		t = t.Add(d.Skew(t))
	}
	return t
}

// Project creates a git checkout Home/code/<name> whose origin is
// github.com/<owner>/<name> (owner "" = the device's user) and returns its root.
func (d *Device) Project(owner, name string) string {
	if owner == "" {
		owner = d.User
	}
	root := filepath.Join(d.Home, "code", name)
	_ = os.MkdirAll(filepath.Join(root, ".git"), 0o700)
	cfg := fmt.Sprintf("[core]\n\tbare = false\n[remote \"origin\"]\n\turl = https://github.com/%s/%s.git\n", owner, name)
	_ = os.WriteFile(filepath.Join(root, ".git", "config"), []byte(cfg), 0o600)
	return root
}

// ProjectKey is the local (and, for public GitHub remotes, server) key.
func ProjectKey(owner, name string) string {
	owner = aliasUser(owner)
	return "github.com/" + strings.ToLower(owner) + "/" + strings.ToLower(name)
}

// Hook delivers a Claude Code hook payload through `seatime hook claude-code <event>`.
func (d *Device) Hook(hookEvent string, payload map[string]any) {
	b, _ := json.Marshal(payload)
	if _, err := claudecode.New().HandleHook(hookEvent, b, d.Pipe); err != nil {
		d.SyncErrs = append(d.SyncErrs, "hook: "+err.Error())
	}
}

// Local sends a request to the daemon's local HTTP surface (127.0.0.1:47823).
func (d *Device) Local(method, path, origin, authz, body string) (int, string) {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "127.0.0.1:47823"
	if d.hostOverride != "" {
		r.Host = d.hostOverride
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	d.withEnv(func() { d.handler.ServeHTTP(rec, r) })
	return rec.Code, rec.Body.String()
}

// withEnv points config.DataDir() (local secret file) at this device.
func (d *Device) withEnv(f func()) {
	old, had := os.LookupEnv("SEATIME_HOME")
	os.Setenv("SEATIME_HOME", d.DataDir)
	defer func() {
		if had {
			os.Setenv("SEATIME_HOME", old)
		} else {
			os.Unsetenv("SEATIME_HOME")
		}
	}()
	f()
}

// Heartbeat posts WakaTime heartbeats as an editor plugin does (the plugin's
// api_url points at the daemon). Time is the device clock unless hb has "time".
func (d *Device) Heartbeat(hbs ...map[string]any) {
	for _, hb := range hbs {
		if _, ok := hb["time"]; !ok {
			hb["time"] = float64(d.Now().UnixMilli()) / 1000
		}
	}
	b, _ := json.Marshal(hbs)
	if code, body := d.Local("POST", "/api/v1/users/current/heartbeats.bulk", "", "Basic c2ltOg==", string(b)); code >= 300 {
		d.SyncErrs = append(d.SyncErrs, fmt.Sprintf("heartbeat: %d %s", code, body))
	}
}

// EmitCLI is `seatime emit` (plugin protocol over stdin; no secret needed: it
// writes the local database directly).
func (d *Device) EmitCLI(payload string, def emit.Defaults) (int, error) {
	evs, err := emit.DecodeEvents([]byte(payload), def)
	if err != nil {
		return 0, err
	}
	for _, e := range evs {
		if err := d.Pipe.Emit(e); err != nil {
			return 0, err
		}
	}
	return len(evs), nil
}

// Commit records a git commit the way the daemon's gitscan does (it reads
// `git log` and emits a commit event with the committer timestamp). at is the
// commit's committer date (zero = now on the device clock).
func (d *Device) Commit(root string, at time.Time, ins, files int) {
	if at.IsZero() {
		at = d.Now()
	}
	sha := simSHA(root, at)
	info := d.Pipe.Res.ForDir(root)
	if fresh, _ := d.Store.MarkCommit(sha, info.Key); !fresh {
		return
	}
	d.head[root], d.headTime[root] = sha, at
	_, _ = d.Pipe.EmitEvent(event.Event{Kind: event.Commit, Harness: "git", Project: info.Key, CWD: root, SHA: sha, TS: at,
		Insertions: ins, FilesChanged: files})
}

// simSHA is a commit SHA that looks like git's (40 hex digits, all of them
// varying) and is the same every run with the same seed: the SHA-1 of the
// repo folder's name and the commit time.
func simSHA(root string, at time.Time) string {
	h := sha1.Sum([]byte(filepath.Base(root) + "\x00" + at.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(h[:])
}

func (d *Device) syncQuiet() {
	if _, err := d.Syncer.SyncOnce(context.Background()); err != nil {
		d.SyncErrs = append(d.SyncErrs, "sync: "+err.Error())
	}
}

// SyncNow runs one sync immediately.
func (d *Device) SyncNow() error {
	_, err := d.Syncer.SyncOnce(context.Background())
	return err
}

// SubResult is one segment as the server checked it: a stretch of a
// project's records the device uploaded (as the daemon does once the
// stretch is over), and what an application then reads for it.
type SubResult struct {
	ID       string
	Project  string
	Verified bool // the segment did not fail (verified, or pending a prompt check)
	Status   string
	SegHours float64 // the hours the segment credits (0 when it failed)
	Problems []string
	Scored   ledger.Bucket
	Claimed  ledger.Bucket
	Export   *api.YSWSExport
	Err      string        // the server refused the upload outright
	Events   []event.Event // what the bundle revealed (for what-if rescoring)
	Dev      *Device       // the device that built the bundle
}

// Hours is what the segment credits (0 if refused or failed).
func (s *SubResult) Hours() float64 {
	if s == nil || s.Err != "" || s.Status == "failed" {
		return 0
	}
	return s.Scored.Seconds() / 3600
}

// SubmitOpts tweak an upload of one segment.
type SubmitOpts struct {
	Since, Until time.Time
	NoSync       bool                // don't sync first (default: sync, then cap at the synced seq)
	Tamper       func(b *api.Bundle) // edit the bundle before upload
	Resign       bool                // re-sign after Tamper (the attacker holds the device key)
	SignWith     *config.Auth        // sign with another key
	UploadAs     *Device             // upload with another device's token
}

// Submit uploads the project's records as one segment, as the daemon does
// once a stretch of work is over, and fetches the project's YSWS export.
func (d *Device) Submit(projectKey string, o SubmitOpts) *SubResult {
	r := &SubResult{Project: projectKey, Dev: d}
	d.W.Advance(31 * time.Second)
	so := segment.Options{Project: projectKey, Since: o.Since, Until: o.Until}
	if !o.NoSync {
		if err := d.SyncNow(); err != nil {
			r.Err = "sync before submit: " + err.Error()
			return r
		}
		ss, _ := d.Store.SyncState()
		so.MaxSeq = ss.Seq
	}
	prep, err := segment.Build(d.Store, d.Auth, so)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	b := prep.Bundle
	r.Claimed = b.Claimed
	if o.Tamper != nil {
		o.Tamper(&b)
		if o.Resign {
			_ = syncer.SignBundle(d.Auth, &b)
		}
	}
	if o.SignWith != nil {
		_ = syncer.SignBundle(*o.SignWith, &b)
	}
	for _, en := range b.Entries {
		if en.Event != nil {
			r.Events = append(r.Events, *en.Event)
		}
	}
	up := d
	if o.UploadAs != nil {
		up = o.UploadAs
	}
	resp, err := syncer.NewClient(d.W.HTTP.URL, up.Auth.Token, "sim").Segment(context.Background(), b)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.setResp(resp)
	if x, err := d.W.Export(d.User, b.Project); err == nil {
		r.Export = x
	}
	return r
}

func (r *SubResult) setResp(resp api.SegmentResp) {
	r.ID, r.Status, r.Problems, r.Scored, r.SegHours = resp.ID, resp.Status, resp.Problems, resp.Scored, resp.Hours
	r.Verified = resp.Status != "failed"
}

// Sleep closes the laptop for dur: the daemon writes its idle window and
// stops, nothing syncs, and it starts again on wake.
func (d *Device) Sleep(dur time.Duration) {
	if d.idleRec != nil {
		d.idleRec.Flush()
	}
	every := d.SyncEvery
	d.SyncEvery = 0
	d.W.Advance(dur)
	d.SyncEvery = every
	d.nextSync = d.W.Now().Add(every)
	d.nextIdle = d.W.Now().Add(time.Minute)
}

// SubmitAll uploads a project of any length: sync, plan back-to-back
// ranges (segment.Ranges), build and upload each part. It returns one result per part.
func (d *Device) SubmitAll(projectKey string) ([]*SubResult, error) {
	d.W.Advance(31 * time.Second)
	if err := d.SyncNow(); err != nil {
		return nil, err
	}
	ss, _ := d.Store.SyncState()
	ranges, err := segment.Ranges(d.Store, segment.Options{Project: projectKey, MaxSeq: ss.Seq})
	if err != nil {
		return nil, err
	}
	c := syncer.NewClient(d.W.HTTP.URL, d.Auth.Token, "sim")
	var out []*SubResult
	for _, ro := range ranges {
		prep, err := segment.Build(d.Store, d.Auth, ro)
		if err != nil {
			return out, err
		}
		r := &SubResult{Project: projectKey, Claimed: prep.Bundle.Claimed, Dev: d}
		for _, en := range prep.Bundle.Entries {
			if en.Event != nil {
				r.Events = append(r.Events, *en.Event)
			}
		}
		resp, err := segment.Upload(context.Background(), c, prep.Bundle, d.W.Advance)
		if err != nil {
			return out, err
		}
		r.setResp(resp)
		if x, err := d.W.Export(d.User, prep.Bundle.Project); err == nil {
			r.Export = x
		}
		out = append(out, r)
	}
	return out, nil
}

// rawPost is a raw authenticated API call (attackers bypassing the client).
func (d *Device) rawPost(path string, v any) (int, string) {
	b, _ := json.Marshal(v)
	req, _ := http.NewRequest("POST", d.W.HTTP.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+d.Auth.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}
