package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/emit"
	"seatime/internal/client/localauth"
	"seatime/internal/client/store"
	"seatime/pkg/event"
)

const extA = "chrome-extension://abcdefghijklmnopabcdefghijklmnop"

func testDaemon(t *testing.T) (*Daemon, http.Handler) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("SEATIME_FAKE_HOME", home)
	t.Setenv("SEATIME_HOME", filepath.Join(home, "seatime"))
	st, err := store.Open(filepath.Join(home, "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := &Daemon{Store: st, Pipeline: emit.New(st, home), Started: time.Now(), Version: "test"}
	return d, d.handler(config.Config{})
}

type req struct {
	method, path, body string
	host, origin, auth string
}

func do(h http.Handler, q req) *httptest.ResponseRecorder {
	r := httptest.NewRequest(orStr(q.method, "POST"), q.path, strings.NewReader(q.body))
	r.Host = orStr(q.host, "127.0.0.1:47823")
	if q.origin != "" {
		r.Header.Set("Origin", q.origin)
	}
	if q.auth != "" {
		r.Header.Set("Authorization", q.auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

const emitBody = `[{"kind":"edit","harness":"plugin","project":"p","files":["/x/a.go"],"write":true}]`

func TestEmitRequiresSecret(t *testing.T) {
	d, h := testDaemon(t)
	secret, err := localauth.Secret()
	if err != nil {
		t.Fatal(err)
	}
	bearer := "Bearer " + secret
	cases := []struct {
		name string
		q    req
		want int
	}{
		{"no credentials", req{path: "/seatime/emit", body: emitBody}, 401},
		{"old X-Seatime header alone is not enough", req{path: "/seatime/emit", body: emitBody, auth: ""}, 401},
		{"wrong secret", req{path: "/seatime/emit", body: emitBody, auth: "Bearer nope"}, 401},
		{"basic scheme", req{path: "/seatime/emit", body: emitBody, auth: "Basic " + secret}, 401},
		{"secret, no origin (curl, plugins)", req{path: "/seatime/emit", body: emitBody, auth: bearer}, 202},
		{"GET", req{method: "GET", path: "/seatime/emit", auth: bearer}, 405},
		{"web page origin with the right secret", req{path: "/seatime/emit", body: emitBody, auth: bearer, origin: "https://evil.example"}, 403},
		{"null origin (sandboxed iframe)", req{path: "/seatime/emit", body: emitBody, auth: bearer, origin: "null"}, 403},
		{"http localhost page", req{path: "/seatime/emit", body: emitBody, auth: bearer, origin: "http://127.0.0.1:47823"}, 403},
		{"browser extension origin", req{path: "/seatime/emit", body: emitBody, auth: bearer, origin: extA}, 403},
		{"DNS rebinding host", req{path: "/seatime/emit", body: emitBody, auth: bearer, host: "evil.example:47823"}, 403},
		{"DNS rebinding host, no auth", req{path: "/seatime/emit", body: emitBody, host: "127.0.0.1.evil.example"}, 403},
		{"bad kind with valid auth", req{path: "/seatime/emit", body: `[{"kind":"nope"}]`, auth: bearer}, 400},
	}
	for _, c := range cases {
		if got := do(h, c.q).Code; got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
	evs, _ := d.Store.Events(store.Query{})
	if len(evs) != 1 {
		t.Fatalf("only the one authorised request may store events, got %d", len(evs))
	}
}

func TestOtherEndpointsKeepWorking(t *testing.T) {
	d, h := testDaemon(t)
	// ping (used by the CLI) and the WakaTime-compatible endpoint (no Origin, loopback host)
	if w := do(h, req{method: "GET", path: "/seatime/ping"}); w.Code != 200 || !strings.Contains(w.Body.String(), `"seatime"`) {
		t.Fatalf("ping: %d %s", w.Code, w.Body)
	}
	hb := `[{"entity":"/home/u/p/main.go","type":"file","time":` + nowSec() + `,"project":"p","is_write":true,"user_agent":"wakatime/1 vscode/1 vscode-wakatime/1"}]`
	if w := do(h, req{path: "/api/v1/users/current/heartbeats.bulk", body: hb, auth: "Basic a2V5"}); w.Code >= 400 {
		t.Fatalf("wakatime heartbeat: %d %s", w.Code, w.Body)
	}
	if evs, _ := d.Store.Events(store.Query{Kinds: []event.Kind{event.Edit}}); len(evs) != 1 {
		t.Fatalf("heartbeat should create one edit event, got %d", len(evs))
	}
	// browsers cannot forge heartbeats, nor can a rebound host
	if w := do(h, req{path: "/api/v1/users/current/heartbeats.bulk", body: hb, auth: "Basic a2V5", origin: "https://evil.example"}); w.Code < 400 {
		t.Fatalf("web origin heartbeat accepted: %d", w.Code)
	}
	if w := do(h, req{path: "/api/v1/users/current/heartbeats.bulk", body: hb, auth: "Basic a2V5", host: "evil.example"}); w.Code != 403 {
		t.Fatalf("rebound heartbeat: %d", w.Code)
	}
}

func nowSec() string { return strconv.FormatInt(time.Now().Unix(), 10) }

// The browser-extension endpoints are gone; old extensions get a 404 and
// nothing is stored.
func TestBrowserExtensionEndpointsGone(t *testing.T) {
	d, h := testDaemon(t)
	secret, _ := localauth.Secret()
	for _, p := range []string{"/seatime/ext/pair", "/seatime/ext/event", "/seatime/ext/status"} {
		w := do(h, req{path: p, body: `{"type":"prompt","site":"claude","text":"x"}`, auth: "Bearer " + secret})
		if w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if evs, _ := d.Store.Events(store.Query{}); len(evs) != 0 {
		t.Fatalf("stored %d events", len(evs))
	}
}
