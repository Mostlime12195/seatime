package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"seatime/internal/client/canary"
	"seatime/internal/client/config"
	"seatime/internal/client/localauth"
	"seatime/internal/client/setup"
	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/internal/client/tray"
	"seatime/internal/client/update"
	"seatime/pkg/api"
	"seatime/pkg/event"
)

// trayServer is a SeaTime server as far as the icon needs one.
type trayServer struct {
	today     atomic.Pointer[api.Today] // nil: an older server (404)
	todayHits atomic.Int32
	syncs     atomic.Int32
	seq       atomic.Int64
	status    atomic.Int32 // when set, every answer has this status
}

func (f *trayServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s := f.status.Load(); s != 0 {
		http.Error(w, `{"error":"no"}`, int(s))
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /api/v1/device/today":
		f.todayHits.Add(1)
		td := f.today.Load()
		if td == nil {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(td)
	case "POST /api/v1/sync":
		var req api.SyncReq
		json.NewDecoder(r.Body).Decode(&req)
		f.seq.Store(req.Checkpoint.Seq)
		f.syncs.Add(1)
		json.NewEncoder(w).Encode(api.SyncResp{OK: true, ServerTime: time.Now()})
	case "POST /api/v1/records":
		var req api.RecordsReq
		json.NewDecoder(r.Body).Decode(&req)
		resp := api.RecordsResp{Next: f.seq.Load()}
		if n := len(req.Entries); n > 0 {
			resp = api.RecordsResp{Head: req.Entries[n-1].Seq}
		}
		json.NewEncoder(w).Encode(resp)
	default:
		http.NotFound(w, r)
	}
}

// trayEnv is a daemon signed in to a trayServer.
func trayEnv(t *testing.T) (*Daemon, *trayServer) {
	t.Helper()
	sandbox(t)
	os.MkdirAll(config.DataDir(), 0o700)
	st, err := store.Open(config.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &trayServer{}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	priv, pub, _ := syncer.NewKey()
	if err := config.SaveAuth(config.Auth{Token: "grt_x", DeviceID: "dev1", Username: "pia", Server: ts.URL, PrivateKey: priv, PublicKey: pub}); err != nil {
		t.Fatal(err)
	}
	return &Daemon{Store: st, Version: "v1.2.3", Started: time.Now()}, f
}

// The icon's endpoints take the local secret and no browser, as every
// other local endpoint that says or changes anything does.
func TestTrayEndpointsNeedTheSecret(t *testing.T) {
	d, _ := trayEnv(t)
	h := d.handler(config.Config{})
	secret, _ := localauth.Secret()
	for _, path := range []string{"/seatime/tray", "/seatime/tray/send", "/seatime/tray/update"} {
		method := "POST"
		if path == "/seatime/tray" {
			method = "GET"
		}
		if w := do(h, req{method: method, path: path}); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without the secret: %d", path, w.Code)
		}
		if w := do(h, req{method: method, path: path, auth: "Bearer " + secret, origin: "https://evil.example"}); w.Code != http.StatusForbidden {
			t.Errorf("%s from a web page: %d", path, w.Code)
		}
		if w := do(h, req{method: method, path: path, auth: "Bearer " + secret, host: "evil.example"}); w.Code != http.StatusForbidden {
			t.Errorf("%s under another host name: %d", path, w.Code)
		}
	}
	if w := do(h, req{method: "POST", path: "/seatime/tray", auth: "Bearer " + secret}); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /seatime/tray: %d", w.Code)
	}
	w := do(h, req{method: "GET", path: "/seatime/tray", auth: "Bearer " + secret})
	var st tray.Status
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Version != "v1.2.3" || st.Account != "pia" {
		t.Fatalf("GET /seatime/tray: %d %s", w.Code, w.Body)
	}
}

// The status carries the server's numbers, the project being worked on
// and at most one problem.
func TestTrayStatus(t *testing.T) {
	d, f := trayEnv(t)
	ctx := context.Background()
	f.today.Store(&api.Today{Seconds: 8100, UncheckedSeconds: 720, UncheckedPrompts: 3})

	st := d.TrayStatus(ctx)
	if st.Problem != "" || st.Now != "" || st.Off || st.Account != "pia" || st.Site == "" || st.Updates {
		t.Fatalf("signed in, idle: %+v", st)
	}
	if st.Today == nil || *st.Today != (tray.Totals{Seconds: 8100, UncheckedSeconds: 720, UncheckedPrompts: 3}) {
		t.Fatalf("today: %+v", st.Today)
	}

	// work going on: the project by its folder's name
	now := time.Now()
	d.Store.TouchProject(store.Project{Key: "local:0123abcd", Label: "github.com/pia/app"}, now)
	d.Store.Append(event.Event{Kind: event.Edit, Harness: "editor:vscode", Project: "local:0123abcd", TS: now})
	if st = d.TrayStatus(ctx); st.Now != "app" {
		t.Fatalf("working: %+v", st)
	}

	// the numbers are kept: a second look does not ask the server again
	if n := f.todayHits.Load(); n != 1 {
		t.Fatalf("the server was asked %d times", n)
	}
	// once they are old, the server is asked again, in the background
	f.today.Store(&api.Today{Seconds: 8160})
	d.trayTotals.mu.Lock()
	d.trayTotals.asked = time.Now().Add(-2 * totalsActive)
	d.trayTotals.mu.Unlock()
	d.TrayStatus(ctx)
	for i := 0; i < 200 && f.todayHits.Load() < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if st = d.TrayStatus(ctx); st.Today == nil || st.Today.Seconds != 8160 {
		t.Fatalf("after a minute: %+v", st.Today)
	}

	// one problem at a time, the one the person can act on first
	d.Store.SetMeta("last_sync_error", "dial tcp: no route")
	if st = d.TrayStatus(ctx); st.Problem != tray.ProblemServer {
		t.Fatalf("sync failing: %+v", st)
	}
	d.Store.SetMeta("last_sync_error", "")
	b, _ := json.Marshal([]canary.Silent{{Harness: "cursor", Label: "Cursor"}})
	d.Store.SetMeta(MetaSilent, string(b))
	if st = d.TrayStatus(ctx); st.Problem != tray.ProblemSilent || st.Tool != "Cursor" {
		t.Fatalf("silent hooks: %+v", st)
	}
	d.Store.SetMeta(MetaSilent, "")
	d.keepHookResults([]setup.HookResult{{Name: "cursor", Status: "absent"}})
	if st = d.TrayStatus(ctx); st.Problem != tray.ProblemNoTools {
		t.Fatalf("nothing connected: %+v", st)
	}
	d.keepHookResults([]setup.HookResult{{Name: "cursor", Status: "ok"}})
	if st = d.TrayStatus(ctx); st.Problem != "" {
		t.Fatalf("a tool connected: %+v", st)
	}

	// the server stopped accepting this computer: signed out, with the page to approve it on
	d.Store.SetMeta(MetaSyncBlocked, "the server no longer accepts this computer")
	d.Store.PutJSON(MetaSignIn, pendingSignIn{DeviceCode: "dc", URL: "https://s/device?code=ABCD", Expires: time.Now().Add(time.Hour)})
	if st = d.TrayStatus(ctx); st.Problem != tray.ProblemSignedOut || st.SignIn != "https://s/device?code=ABCD" || st.Today != nil {
		t.Fatalf("signed out: %+v", st)
	}

	// turned off
	off := false
	config.Save(config.Config{Tray: &off})
	if st = d.TrayStatus(ctx); !st.Off {
		t.Fatal("the icon is off and the status does not say so")
	}
}

// Never signed in: say so, and ask the server nothing.
func TestTrayStatusSignedOut(t *testing.T) {
	d, f := trayEnv(t)
	os.Remove(config.AuthPath())
	st := d.TrayStatus(context.Background())
	if st.Problem != tray.ProblemSignedOut || st.Account != "" || st.Site != "" || st.Today != nil || f.todayHits.Load() != 0 {
		t.Fatalf("%+v, %d requests", st, f.todayHits.Load())
	}
}

// An older server has no numbers for the icon: none are shown, and it is
// not asked again each time.
func TestTrayOlderServer(t *testing.T) {
	d, f := trayEnv(t)
	for i := 0; i < 3; i++ {
		if st := d.TrayStatus(context.Background()); st.Today != nil || st.Problem != "" {
			t.Fatalf("%+v", st)
		}
	}
	if n := f.todayHits.Load(); n != 1 {
		t.Fatalf("an older server was asked %d times", n)
	}
}

// "Send now" syncs and sends the records, and says what happened.
func TestSendNow(t *testing.T) {
	d, f := trayEnv(t)
	ctx := context.Background()
	now := time.Now()
	d.Store.TouchProject(store.Project{Key: "local:app", Name: "app"}, now)
	d.Store.Append(event.Event{Kind: event.Edit, Harness: "editor:vscode", Project: "local:app", TS: now})
	if got := d.SendNow(ctx); got != tray.Sent || f.syncs.Load() == 0 {
		t.Fatalf("%s, %d syncs", got, f.syncs.Load())
	}
	head, _, _ := d.Store.Head()
	if ss, _ := d.Store.SyncState(); ss.Seq != head {
		t.Fatalf("synced up to %d of %d", ss.Seq, head)
	}
	d.Store.Append(event.Event{Kind: event.Edit, Harness: "editor:vscode", Project: "local:app", TS: time.Now()})
	f.status.Store(500)
	if got := d.SendNow(ctx); got != tray.Failed {
		t.Fatalf("server down: %s", got)
	}
	f.status.Store(401)
	if got := d.SendNow(ctx); got != tray.SignedOut {
		t.Fatalf("token refused: %s", got)
	}
	os.Remove(config.AuthPath())
	if got := d.SendNow(ctx); got != tray.SignedOut {
		t.Fatalf("never signed in: %s", got)
	}
}

// "Check for updates" installs the newest release even with automatic
// updates off, and the daemon then starts again on it.
func TestTrayUpdateNow(t *testing.T) {
	d, _ := trayEnv(t)
	if got := d.UpdateNow(context.Background()); got.Result != tray.Failed {
		t.Fatalf("no updater: %+v", got)
	}
	pk, priv, _ := ed25519.GenerateKey(rand.Reader)
	bin := []byte("new")
	s := sha256.Sum256(bin)
	m, _ := update.Sign(update.Manifest{Version: "v1.3.0", Files: map[string]string{
		update.AssetName("linux", "amd64"): hex.EncodeToString(s[:])}}, priv)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filepath.Base(r.URL.Path) == update.ManifestName {
			w.Write(m)
		} else {
			w.Write(bin)
		}
	}))
	defer ts.Close()
	exe := filepath.Join(t.TempDir(), "seatime")
	os.WriteFile(exe, []byte("old"), 0o755)
	d.restart = make(chan struct{}, 1)
	d.Update = &update.Updater{Version: "v1.2.3", Exe: exe, Dir: config.DataDir(), Key: base64.StdEncoding.EncodeToString(pk),
		GOOS: "linux", GOARCH: "amd64",
		SelfTest: func(context.Context, string) (string, error) { return update.SelfTestLine("v1.3.0"), nil }}
	off := false
	config.Save(config.Config{AutoUpdate: &off, UpdateURL: ts.URL})

	secret, _ := localauth.Secret()
	w := do(d.handler(config.Config{}), req{method: "POST", path: "/seatime/tray/update", auth: "Bearer " + secret})
	var rep tray.Reply
	if json.Unmarshal(w.Body.Bytes(), &rep); rep.Result != tray.Updated || rep.Version != "v1.3.0" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatal("not installed")
	}
	select {
	case <-d.restart:
	default:
		t.Fatal("no restart after the update")
	}
	// the running daemon is on the new version now: nothing newer
	d.Update.Version = "v1.3.0"
	if got := d.UpdateNow(context.Background()); got.Result != tray.Current {
		t.Fatalf("already newest: %+v", got)
	}
}
