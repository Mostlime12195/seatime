package wakaproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"seatime/pkg/event"
	"seatime/pkg/ledger"
)

type sink struct {
	mu sync.Mutex
	ev []event.Event
}

func (s *sink) Emit(e event.Event) error {
	s.mu.Lock()
	s.ev = append(s.ev, e)
	s.mu.Unlock()
	return nil
}

const ua = "wakatime/v1.90.0 (linux-6.1-x86_64) go1.22 vscode/1.85.0 vscode-wakatime/24.0.1"

func post(h http.Handler, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("User-Agent", ua)
	r.Header.Set("Authorization", "Basic abc")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestParseAndConvert(t *testing.T) {
	hbs, err := ParseHeartbeats([]byte(`[{"entity":"/home/u/p/a.go","type":"file","category":"coding","time":1700000000.5,"project":"p","is_write":true,"lines":120,"lineno":37,"cursorpos":"912","line_additions":4,"line_deletions":1},
	 {"entity":"google.com","type":"domain","category":"browsing","time":1700000001},
	 {"entity":"/x/ai.py","type":"file","category":"ai coding","time":1700000002}]`))
	if err != nil || len(hbs) != 3 {
		t.Fatal(err, hbs)
	}
	e, ok := hbs[0].ToEvent(ua)
	if !ok || e.Kind != event.Edit || e.Harness != "editor:vscode" || !e.Write || e.Lines != 120 || e.LineNo != 37 || e.CursorPos != 912 || e.Files[0] != "/home/u/p/a.go" || e.TS.Unix() != 1700000000 || e.CWD != "/home/u/p" {
		t.Fatalf("%+v ok=%v", e, ok)
	}
	if _, ok := hbs[1].ToEvent(ua); ok {
		t.Fatal("domain heartbeat should be skipped")
	}
	// An "ai coding" heartbeat is stored, as the agent's heartbeat
	// with the editor as its tool, never as a hand edit
	ai, ok := hbs[2].ToEvent(ua)
	if !ok || ai.Category != "ai coding" || ai.Harness != "editor:vscode" || ai.SessionID != "" {
		t.Fatalf("ai coding heartbeat: %+v ok=%v", ai, ok)
	}
	for _, h := range ledger.Heartbeats([]event.Event{ai}) {
		if h.Yours || h.Kept {
			t.Fatalf("an ai coding heartbeat with no hand edit counted: %+v", h)
		}
	}
	if e.LineAdditions != 4 || e.LineDeletions != 1 {
		t.Fatalf("line counts: %+v", e)
	}
	if e.Category != "coding" {
		t.Fatalf("category of a hand edit: %q", e.Category)
	}
	if one, err := ParseHeartbeats([]byte(`{"entity":"/a","time":"5","lineno":-3}`)); err != nil || len(one) != 1 || one[0].Time != 5 || one[0].LineNo != 0 {
		t.Fatal(one, err)
	}
	if _, err := ParseHeartbeats([]byte(`nope`)); err == nil {
		t.Fatal("expected error")
	}
}

func TestEditorFromUA(t *testing.T) {
	for in, want := range map[string]string{
		ua: "vscode", "wakatime/13 (windows) py3 neovim/0.9 neovim-wakatime/1.2": "neovim", "": "unknown",
	} {
		if got := EditorFromUA(in); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
}

func TestHandlerLocalOnly(t *testing.T) {
	s := &sink{}
	h := New(s, "")
	body := `[{"entity":"/p/a.go","type":"file","time":100.1},{"entity":"/p/a.go","type":"file","time":100.1},{"entity":"/p/b.go","type":"file","time":101}]`
	w := post(h, "/api/v1/users/current/heartbeats.bulk", body, nil)
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"responses"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(s.ev) != 2 { // duplicate suppressed
		t.Fatalf("got %d events", len(s.ev))
	}
	w = post(h, "/api/v1/users/current/heartbeats", `{"entity":"/p/c.go","time":102}`, nil)
	if w.Code != 201 || len(s.ev) != 3 {
		t.Fatal(w.Code, len(s.ev))
	}
	// no Authorization (drive-by browser form) and cross-origin are refused
	r := httptest.NewRequest("POST", "/api/v1/users/current/heartbeats", strings.NewReader(`{"entity":"/z"}`))
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r)
	if w2.Code != 401 || len(s.ev) != 3 {
		t.Fatal(w2.Code)
	}
	if w := post(h, "/api/v1/users/current/heartbeats", `{"entity":"/z"}`, map[string]string{"Origin": "https://evil.example"}); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := post(h, "/api/v1/users/current/heartbeats", `garbage`, nil); w.Code != 400 {
		t.Fatal(w.Code)
	}
	// with no upstream the plugin's status-bar read still gets a valid answer
	r = httptest.NewRequest("GET", "/api/users/current/statusbar/today", nil)
	r.Header.Set("Authorization", "Basic abc")
	w2 = httptest.NewRecorder()
	h.ServeHTTP(w2, r)
	var sb struct {
		Data struct {
			GrandTotal struct {
				TotalSeconds *float64 `json:"total_seconds"`
			} `json:"grand_total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &sb); w2.Code != 200 || err != nil || sb.Data.GrandTotal.TotalSeconds == nil {
		t.Fatal(w2.Code, err, w2.Body.String())
	}
}

func TestLocalOnlyNeverCallsUpstream(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer up.Close()
	s := &sink{}
	fwd := ""
	h := New(s, up.URL) // the static value is ignored once ForwardFn is set
	h.ForwardFn = func() string { return fwd }
	if w := post(h, "/api/users/current/heartbeats.bulk", `[{"entity":"/p/a.go","type":"file","time":100}]`, nil); w.Code != 202 {
		t.Fatal(w.Code)
	}
	if hits.Load() != 0 || len(s.ev) != 1 {
		t.Fatalf("hits=%d events=%d", hits.Load(), len(s.ev))
	}
	fwd = up.URL // `seatime setup --also-hackatime` re-enabled forwarding while running
	post(h, "/api/users/current/heartbeats.bulk", `[{"entity":"/p/b.go","type":"file","time":101}]`, nil)
	if hits.Load() != 1 || len(s.ev) != 2 {
		t.Fatalf("hits=%d events=%d", hits.Load(), len(s.ev))
	}
}

func TestForwardUnchanged(t *testing.T) {
	var gotPath, gotBody, gotAuth, gotUA string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody, gotAuth, gotUA = r.URL.Path, string(b), r.Header.Get("Authorization"), r.UserAgent()
		w.Header().Set("X-Up", "1")
		w.WriteHeader(202)
		w.Write([]byte(`{"responses":[]}`))
	}))
	defer up.Close()
	s := &sink{}
	h := New(s, up.URL+"/api/hackatime/v1/")
	body := `[{"entity":"/p/a.go","type":"file","time":100}]`
	w := post(h, "/api/users/current/heartbeats.bulk", body, nil)
	if w.Code != 202 || w.Header().Get("X-Up") != "1" || w.Body.String() != `{"responses":[]}` {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
	if gotPath != "/api/hackatime/v1/users/current/heartbeats.bulk" || gotBody != body || gotAuth != "Basic abc" || gotUA != ua {
		t.Fatalf("not forwarded unchanged: %q %q %q %q", gotPath, gotBody, gotAuth, gotUA)
	}
	if len(s.ev) != 1 {
		t.Fatal("event not recorded")
	}
	// upstream down: plugin sees an error (so it retries), no duplicate on retry
	up.Close()
	w = post(h, "/api/users/current/heartbeats.bulk", `[{"entity":"/p/q.go","type":"file","time":200}]`, nil)
	w = post(h, "/api/users/current/heartbeats.bulk", `[{"entity":"/p/q.go","type":"file","time":200}]`, nil)
	if w.Code != 502 || len(s.ev) != 2 {
		t.Fatal(w.Code, len(s.ev))
	}
}
