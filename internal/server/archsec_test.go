package server

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"seatime/internal/server/store"
)

// Behind a proxy the client address is the entry SEATIME_TRUSTED_HOPS
// from the right; what a client writes on the left is never used.
func TestClientIPTakesRightmostTrustedHop(t *testing.T) {
	s := &Server{cfg: Config{TrustProxy: true, TrustedHops: 1}}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")
	if got := s.clientIP(r); got != "203.0.113.9" {
		t.Fatalf("one hop: %q, want the rightmost entry", got)
	}
	s.cfg.TrustedHops = 2
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.4, 10.0.0.2")
	if got := s.clientIP(r); got != "198.51.100.4" {
		t.Fatalf("two hops: %q", got)
	}
	// Orchard's chain: a lost client shows as the node address, never as
	// what the client wrote
	r.Header.Set("X-Forwarded-For", "203.0.113.77, 10.244.1.50, 10.244.2.182")
	if got := s.clientIP(r); got != "10.244.1.50" {
		t.Fatalf("lost client: %q, want the address the ingress saw", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.77, 2.29.123.158, 10.244.1.47")
	if got := s.clientIP(r); got != "2.29.123.158" {
		t.Fatalf("known client: %q", got)
	}
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := s.clientIP(r); got != "10.0.0.1" {
		t.Fatalf("garbage header: %q, want the peer address", got)
	}
	s.cfg.TrustProxy = false
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := s.clientIP(r); got != "10.0.0.1" {
		t.Fatalf("no proxy: %q", got)
	}
}

// Dev mode refuses a public https URL.
func TestDevModeRefusesPublicURL(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New(Config{Dev: true, BaseURL: "https://seatime.example.com", Secret: "s"}, nil, log); err == nil {
		t.Fatal("dev mode started with a public https URL")
	}
	for _, ok := range []string{"http://example.test", "https://127.0.0.1:8790", "https://localhost"} {
		if !devBaseOK(ok) {
			t.Errorf("devBaseOK(%q) = false", ok)
		}
	}
}

// The debug endpoints are not on the public handler, and their
// listener must be loopback.
func TestDebugNotOnPublicHandler(t *testing.T) {
	e := newEnv(t)
	e.srv.cfg.Debug = true
	h := e.srv.Handler()
	for _, p := range []string{"/debug/stats", "/debug/pprof/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code == 200 {
			t.Errorf("%s answered 200 on the public handler", p)
		}
	}
	rec := httptest.NewRecorder()
	e.srv.DebugHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/debug/stats", nil))
	if rec.Code != 200 {
		t.Errorf("debug handler /debug/stats: %d", rec.Code)
	}
	if LoopbackAddr("0.0.0.0:6060") || !LoopbackAddr("127.0.0.1:6060") || !LoopbackAddr("[::1]:6060") {
		t.Error("LoopbackAddr")
	}
}

// Logging out ends every session of the account, so a copied cookie
// stops working too; so does a role change.
func TestLogoutEndsCopiedSessions(t *testing.T) {
	e := newEnv(t)
	a := e.browser()
	a.login("adm", "admin")
	u, _ := url.Parse(e.ts.URL)
	thief := e.browser()
	thief.c.Jar.SetCookies(u, a.c.Jar.Cookies(u))
	adminPage := func(b *browser) bool {
		code, body := b.get("/admin")
		return code == 200 && strings.Contains(body, "<h2>Prompt check</h2>")
	}
	if !adminPage(thief) {
		t.Fatal("copied cookie before logout does not open /admin")
	}
	tok := a.csrf("/settings")
	a.post("/logout", url.Values{"csrf": {tok}})
	if adminPage(thief) {
		t.Fatal("a copied cookie still opens /admin after logout")
	}

	b := e.browser()
	b.login("adm2", "admin")
	if !adminPage(b) {
		t.Fatal("admin cannot open /admin")
	}
	usr, _ := e.st.UserByName(context.Background(), "adm2")
	if err := e.st.SetRole(context.Background(), usr.ID, "user"); err != nil {
		t.Fatal(err)
	}
	if adminPage(b) {
		t.Fatal("a demoted admin's session still opens /admin")
	}
}

// Taking someone off the admin list takes the role away at once and signs
// them out; putting someone on it gives them the role without signing in
// again.
func TestAdminListRemovalDemotes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	u, err := e.st.LoginIdentity(ctx, store.Identity{Provider: "github", Subject: "4242"}, "rita", "Rita", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.setSetting(ctx, "admin_ids", ptr("github:4242,dev:boss"), "boss"); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.st.UserByID(ctx, u.ID); got.Role != store.RoleAdmin {
		t.Fatalf("listed: role %q", got.Role)
	}
	before, _ := e.st.UserByID(ctx, u.ID)
	if err := e.srv.setSetting(ctx, "admin_ids", ptr("dev:boss"), "boss"); err != nil {
		t.Fatal(err)
	}
	got, _ := e.st.UserByID(ctx, u.ID)
	if got.Role != store.RoleUser || got.SessionEpoch == before.SessionEpoch {
		t.Fatalf("removed: role %q, epoch %d -> %d", got.Role, before.SessionEpoch, got.SessionEpoch)
	}
	// a typo is refused rather than silently dropping someone
	if err := e.srv.setSetting(ctx, "admin_ids", ptr("github"), "boss"); err == nil {
		t.Fatal("a malformed id was accepted")
	}
}
