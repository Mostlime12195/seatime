package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/store"
	"seatime/internal/client/syncer"
	"seatime/pkg/api"
)

// fakeServer answers check-ins with what the test puts in resp and keeps
// what the device sent.
type fakeServer struct {
	mu      sync.Mutex
	resp    api.CheckInResp
	got     []api.CheckIn
	results map[string]api.RequestResult
	start   []api.DeviceStartReq
	poll    func() (int, any)
}

func (f *fakeServer) handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/v1/device/check-in", func(w http.ResponseWriter, r *http.Request) {
		var ci api.CheckIn
		json.NewDecoder(r.Body).Decode(&ci)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.got = append(f.got, ci)
		json.NewEncoder(w).Encode(f.resp)
		f.resp.Requests = nil
	})
	m.HandleFunc("POST /api/v1/device/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		var res api.RequestResult
		json.NewDecoder(r.Body).Decode(&res)
		f.mu.Lock()
		f.results[r.PathValue("id")] = res
		f.mu.Unlock()
	})
	m.HandleFunc("POST /api/v1/device/start", func(w http.ResponseWriter, r *http.Request) {
		var req api.DeviceStartReq
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.start = append(f.start, req)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(api.DeviceStartResp{DeviceCode: "dc", UserCode: "ABCD-EFGH", VerificationURL: "http://x/device?code=ABCD-EFGH", Interval: 1, ExpiresIn: 900})
	})
	m.HandleFunc("POST /api/v1/device/poll", func(w http.ResponseWriter, r *http.Request) {
		code, v := f.poll()
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	})
	return m
}

func remoteEnv(t *testing.T) (*Daemon, *fakeServer, config.Auth) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	t.Setenv("SEATIME_HOME", filepath.Join(home, "seatime"))
	os.MkdirAll(config.DataDir(), 0o700)
	st, err := store.Open(config.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fakeServer{results: map[string]api.RequestResult{}}
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	priv, pub, _ := syncer.NewKey()
	a := config.Auth{Token: "grt_x", DeviceID: "dev1", Username: "pia", Server: ts.URL, PrivateKey: priv, PublicKey: pub}
	if err := config.SaveAuth(a); err != nil {
		t.Fatal(err)
	}
	return &Daemon{Store: st, Version: "9.9.9"}, f, a
}

// Settings from the web are applied once per change: a change made on the
// computer afterwards stays until the web setting changes again.
func TestCheckInAppliesSettingsOnce(t *testing.T) {
	d, f, _ := remoteEnv(t)
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	f.resp.Settings = []api.AccountSetting{{Key: api.SettingAutoUpdate, Value: false, Updated: t0}}
	if err := d.CheckInOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if config.Load().AutoUpdateEnabled() {
		t.Fatal("automatic updates still on after the web turned them off")
	}
	on := true
	cfg := config.Load()
	cfg.AutoUpdate = &on // `seatime update on` on this computer
	config.Save(cfg)
	d.CheckInOnce(context.Background())
	if !config.Load().AutoUpdateEnabled() {
		t.Fatal("an unchanged web setting undid a change made on the computer")
	}
	f.resp.Settings[0].Updated = t0.Add(time.Minute)
	d.CheckInOnce(context.Background())
	if config.Load().AutoUpdateEnabled() {
		t.Fatal("a new change on the web was not applied")
	}
	if len(f.got) == 0 || f.got[0].ClientVersion != "9.9.9" || !strings.Contains(strings.Join(f.got[0].Can, ","), api.RequestForget) {
		t.Fatalf("check-in: %+v", f.got)
	}
}

// Turning Hackatime off on the web stops the relay; on brings back the
// plugin's original server.
func TestCheckInHackatimeRelay(t *testing.T) {
	d, f, _ := remoteEnv(t)
	cfg := config.Load()
	cfg.Waka = config.WakaProxy{Enabled: true, HadAPIURL: true, OriginalAPIURL: "https://hackatime.example/api/hackatime/v1"}
	cfg.ForwardURL = cfg.Waka.OriginalAPIURL
	config.Save(cfg)
	t0 := time.Now().UTC()
	f.resp.Settings = []api.AccountSetting{{Key: api.SettingAlsoHackatime, Value: false, Updated: t0}}
	d.CheckInOnce(context.Background())
	if config.Load().ForwardURL != "" {
		t.Fatal("still relaying to Hackatime")
	}
	f.resp.Settings = []api.AccountSetting{{Key: api.SettingAlsoHackatime, Value: true, Updated: t0.Add(time.Second)}}
	d.CheckInOnce(context.Background())
	if got := config.Load().ForwardURL; got != "https://hackatime.example/api/hackatime/v1" {
		t.Fatalf("relay %q", got)
	}
}

// Requests are carried out and answered; removal runs after the answer.
func TestCheckInRequests(t *testing.T) {
	d, f, _ := remoteEnv(t)
	var purged *bool
	d.Uninstall = func(p bool) { purged = &p }
	f.resp.Requests = []api.DeviceRequest{{ID: 4, Kind: api.RequestForget}, {ID: 5, Kind: "paint_it_blue"}, {ID: 6, Kind: api.RequestForget, Project: "local:nope"}}
	d.CheckInOnce(context.Background())
	if r := f.results["4"]; !r.OK || r.DeviceID != "dev1" {
		t.Fatalf("forget: %+v", r)
	}
	if r := f.results["5"]; r.OK || r.Error != "unsupported" {
		t.Fatalf("unknown kind: %+v", r)
	}
	if r := f.results["6"]; r.OK || r.Error != "unknown_project" {
		t.Fatalf("unknown project: %+v", r)
	}
	f.resp.Requests = []api.DeviceRequest{{ID: 7, Kind: api.RequestUninstall, Purge: true}}
	d.CheckInOnce(context.Background())
	if r := f.results["7"]; !r.OK || purged == nil || !*purged {
		t.Fatalf("uninstall: %+v, purged %v", r, purged)
	}
}

// When the server stops accepting this computer, the daemon starts a
// sign-in signed with its key, opens the page once (not when the owner
// disconnected it), and saves the new token when it is approved.
func TestSignInByItself(t *testing.T) {
	d, f, a := remoteEnv(t)
	approved := false
	f.poll = func() (int, any) {
		if !approved {
			return 202, map[string]string{"error": "authorization_pending"}
		}
		return 200, api.DevicePollResp{Token: "grt_new", DeviceID: "dev1", Username: "pia"}
	}
	var opened []string
	d.OpenBrowser = func(u string) bool { opened = append(opened, u); return true }
	now := time.Now()
	if _, done := d.signInStep(context.Background(), false, now); done {
		t.Fatal("done before approval")
	}
	if len(f.start) != 1 || f.start[0].PrevDeviceID != "dev1" || f.start[0].PublicKey != a.PublicKey || f.start[0].PrevSig == "" {
		t.Fatalf("start: %+v", f.start)
	}
	if len(opened) != 1 {
		t.Fatalf("opened %v", opened)
	}
	if v, _ := d.Store.GetMeta(MetaSyncBlocked); !strings.Contains(v, "ABCD-EFGH") {
		t.Fatalf("status: %q", v)
	}
	d.signInStep(context.Background(), false, now.Add(5*time.Second))
	if len(f.start) != 1 || len(opened) != 1 {
		t.Fatal("started or opened twice")
	}
	approved = true
	if _, done := d.signInStep(context.Background(), false, now.Add(10*time.Second)); !done {
		t.Fatal("not done after approval")
	}
	got, _ := config.LoadAuth()
	if got.Token != "grt_new" || got.PrivateKey != a.PrivateKey || got.DeviceID != "dev1" {
		t.Fatalf("auth %+v", got)
	}
	// disconnected on purpose: a sign-in starts, but no page opens
	d2, f2, _ := remoteEnv(t)
	f2.poll = func() (int, any) { return 202, nil }
	d2.OpenBrowser = func(u string) bool { opened = append(opened, u); return true }
	d2.signInStep(context.Background(), true, time.Now())
	if len(f2.start) != 1 || len(opened) != 1 {
		t.Fatalf("revoked: %d starts, opened %v", len(f2.start), opened)
	}
}
