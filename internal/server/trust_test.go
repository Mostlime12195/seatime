package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"seatime/internal/server/store"
)

// fakeHackatime answers trust_factor like Hackatime: a colour for known
// Slack IDs, 404 for others, 500 while broken is set.
func fakeHackatime(t *testing.T, colours map[string]string) (*httptest.Server, *atomic.Int32, *atomic.Bool) {
	var calls atomic.Int32
	var broken atomic.Bool
	values := map[string]int{"blue": 0, "red": 1, "green": 2}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/users/"), "/trust_factor")
		if broken.Load() {
			http.Error(w, "down", 500)
			return
		}
		c, known := colours[id]
		if !ok || !known {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":"User not found"}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"trust_level": c, "trust_value": values[c]})
	}))
	t.Cleanup(ts.Close)
	return ts, &calls, &broken
}

func setSlack(t *testing.T, e *tenv, name, slack string) *store.User {
	t.Helper()
	u, err := e.st.UserByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetSlackID(context.Background(), u.ID, slack); err != nil {
		t.Fatal(err)
	}
	u.SlackID = slack
	return u
}

type trustOut struct {
	Level  string `json:"trust_level"`
	Value  int    `json:"trust_value"`
	Source string `json:"trust_source"`
}

// Hackatime says "red": SeaTime's trust_factor and its stats say red. With
// the switch off nothing is sent and the answer is "blue" as before.
func TestHackatimeTrustRedPassesThrough(t *testing.T) {
	e := newEnv(t)
	hk, calls, _ := fakeHackatime(t, map[string]string{"U0RED0001": "red"})
	d, _ := e.enroll("lena")
	setSlack(t, e, "lena", "U0RED0001")
	sendLocal(t, e, d, "local:myapp")

	// off (the default): blue, and Hackatime is never asked
	e.srv.cfg.HackatimeURL = hk.URL
	_, got := apiJSON[trustOut](t, e, "/api/v1/users/lena/trust_factor", "")
	if got.Level != "blue" || got.Source != TrustOff || calls.Load() != 0 {
		t.Fatalf("switch off: %+v, %d calls to Hackatime", got, calls.Load())
	}

	e.srv.cfg.HackatimeTrust = true
	_, got = apiJSON[trustOut](t, e, "/api/v1/users/lena/trust_factor", "")
	if got != (trustOut{"red", 1, TrustFromHackatime}) {
		t.Fatalf("trust_factor: %+v", got)
	}
	_, stats := apiJSON[struct {
		Trust trustOut `json:"trust_factor"`
	}](t, e, "/api/v1/users/lena/stats", "")
	if stats.Trust.Level != "red" {
		t.Fatalf("stats trust_factor: %+v", stats.Trust)
	}
	// two reads inside a minute: Hackatime was asked once
	if calls.Load() != 1 {
		t.Errorf("Hackatime asked %d times, want 1", calls.Load())
	}
}

// When Hackatime fails, a red it served earlier stays red for a week
// ("stale"); with nothing earlier the answer is blue, marked "unavailable".
// Hackatime's 404 is blue, marked "not_on_hackatime"; an account with no
// Slack ID is blue, marked "no_slack_id".
func TestHackatimeTrustFailures(t *testing.T) {
	e := newEnv(t)
	hk, calls, broken := fakeHackatime(t, map[string]string{"U0RED0001": "red"})
	e.srv.cfg.HackatimeTrust, e.srv.cfg.HackatimeURL = true, hk.URL
	now := time.Now()
	e.srv.SetClock(func() time.Time { return now })
	e.enroll("lena")
	e.enroll("sam")
	e.enroll("kim")
	setSlack(t, e, "lena", "U0RED0001")
	setSlack(t, e, "sam", "U0NOBODY1")
	ctx := context.Background()
	user := func(n string) *store.User { u, _ := e.st.UserByName(ctx, n); return u }

	if got := e.srv.trustFor(ctx, user("lena")); got.Level != "red" || got.Source != TrustFromHackatime {
		t.Fatalf("first answer: %+v", got)
	}
	if got := e.srv.trustFor(ctx, user("sam")); got.Level != "blue" || got.Source != TrustNotOnHackatime {
		t.Fatalf("unknown on Hackatime: %+v", got)
	}
	if got := e.srv.trustFor(ctx, user("kim")); got.Level != "blue" || got.Source != TrustNoSlackID {
		t.Fatalf("no Slack ID: %+v", got)
	}
	broken.Store(true)
	// 20 min later the answer is past trustTTL (15 min); Hackatime fails; the red stays
	now = now.Add(20 * time.Minute)
	if got := e.srv.trustFor(ctx, user("lena")); got.Level != "red" || got.Source != TrustStale {
		t.Fatalf("Hackatime down, earlier red: %+v", got)
	}
	// 8 days later the red is past trustStale (7 days)
	now = now.Add(8 * 24 * time.Hour)
	if got := e.srv.trustFor(ctx, user("lena")); got.Level != "blue" || got.Source != TrustUnavailable {
		t.Fatalf("Hackatime down, nothing usable: %+v", got)
	}
	if calls.Load() != 4 { // lena, sam, lena (failed), lena (failed); kim was never asked
		t.Errorf("Hackatime asked %d times, want 4", calls.Load())
	}
}

// The privacy line for the Hackatime lookup shows on /settings only when
// its switch is on.
func TestSettingsPrivacyLines(t *testing.T) {
	e := newEnv(t)
	_, br := e.enroll("lena")
	_, page := br.get("/settings")
	if strings.Contains(page, "What this server looks up") {
		t.Fatal("privacy section shown with the lookup off")
	}
	e.srv.cfg.HackatimeTrust = true
	if _, page = br.get("/settings"); !strings.Contains(page, "asks Hackatime for your account's trust status") {
		t.Fatal("Hackatime line missing")
	}
}
