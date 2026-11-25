package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

const testRedirect = "http://localhost:9/cb"

var (
	clientIDRe = regexp.MustCompile(`stc_[0-9a-f]+`)
	secretRe   = regexp.MustCompile(`sts_[A-Za-z0-9_-]+`)
)

// registerApp registers a application as a signed-in developer and returns
// its client id and secret (shown once).
func registerApp(t *testing.T, e *tenv, dev *browser, redirects ...string) (string, string) {
	t.Helper()
	if len(redirects) == 0 {
		redirects = []string{testRedirect}
	}
	tok := dev.csrf("/developers")
	code, page := dev.post("/developers", url.Values{"csrf": {tok}, "name": {"Shipwright"}, "redirect_uris": {strings.Join(redirects, "\n")}})
	ids := clientIDRe.FindAllString(page, -1) // the newest application is listed last
	id, secret := "", secretRe.FindString(page)
	if len(ids) > 0 {
		id = ids[len(ids)-1]
	}
	if code != 200 || id == "" || secret == "" {
		t.Fatalf("register: %d %s", code, page)
	}
	_, again := dev.get("/developers")
	if strings.Contains(again, secret) {
		t.Fatal("the developer page shows the secret again")
	}
	if strings.Contains(again, "Hackatime") {
		t.Fatal("the developer page mentions Hackatime")
	}
	return id, secret
}

func pkcePair() (verifier, challenge string) {
	verifier = randToken("")
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// noFollow is the browser's cookie jar without following redirects.
func noFollow(br *browser) *http.Client {
	return &http.Client{Jar: br.c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func authQuery(clientID, challenge string) url.Values {
	return url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {testRedirect}, "state": {"st8"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "scope": {"read"}}
}

// authorize sends the user to the consent screen; when it shows, it
// approves. It returns where the server sent the browser.
func authorize(t *testing.T, e *tenv, user *browser, q url.Values) *url.URL {
	t.Helper()
	c := noFollow(user)
	resp, err := c.Get(e.ts.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 {
		if !strings.Contains(string(body), "will be able to see your project names, hours, and commit list") || !strings.Contains(string(body), "<h1>Shipwright</h1>") || strings.Contains(string(body), "Hackatime") {
			t.Fatalf("consent screen: %s", body)
		}
		form := url.Values{"csrf": {csrfRe.FindStringSubmatch(string(body))[1]}, "approve": {"1"}}
		for k := range q {
			form.Set(k, q.Get(k))
		}
		if resp, err = c.PostForm(e.ts.URL+"/oauth/authorize", form); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("authorize: %d %s", resp.StatusCode, body)
	}
	u, _ := url.Parse(resp.Header.Get("Location"))
	return u
}

func tokenReq(t *testing.T, e *tenv, form url.Values) (int, map[string]any) {
	t.Helper()
	resp, err := http.PostForm(e.ts.URL+"/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// connect runs the whole flow and returns the access and refresh tokens.
func connect(t *testing.T, e *tenv, user *browser, id, secret string) (string, string) {
	t.Helper()
	ver, ch := pkcePair()
	loc := authorize(t, e, user, authQuery(id, ch))
	code := loc.Query().Get("code")
	if code == "" || loc.Query().Get("state") != "st8" || !strings.HasPrefix(loc.String(), testRedirect) {
		t.Fatalf("redirect: %s", loc)
	}
	st, m := tokenReq(t, e, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
		"client_id": {id}, "client_secret": {secret}, "code_verifier": {ver}})
	if st != 200 || m["token_type"] != "Bearer" {
		t.Fatalf("token: %d %v", st, m)
	}
	return m["access_token"].(string), m["refresh_token"].(string)
}

func readCurrent(e *tenv, tok string) int {
	code, _ := e.apiDo("GET", "/api/v1/users/current/stats", tok, nil)
	return code
}

// A application connects once with the user's approval, reads their hours,
// keeps working without asking again, and stops the moment they revoke it.
func TestOAuthFlow(t *testing.T) {
	e := newEnv(t)
	dev := e.browser()
	dev.login("devon", "user")
	id, secret := registerApp(t, e, dev)
	_, user := e.enroll("ines")
	access, _ := connect(t, e, user, id, secret)
	if c := readCurrent(e, access); c != 200 {
		t.Fatalf("read with the token: %d", c)
	}
	// a private profile is readable with the owner's approval, not without
	tok := user.csrf("/settings")
	user.post("/settings/profile", url.Values{"csrf": {tok}, "display_name": {"Ines"}, "tz": {"0"}})
	if c, _ := e.apiDo("GET", "/api/v1/users/ines/stats", access, nil); c != 200 {
		t.Fatalf("private profile with the application's token: %d", c)
	}
	if c, _ := e.apiDo("GET", "/api/v1/users/ines/stats", "", nil); c != 403 {
		t.Fatalf("private profile without a token: %d", c)
	}
	// approved once: the next authorization sends a code with no consent screen
	_, ch := pkcePair()
	c := noFollow(user)
	resp, _ := c.Get(e.ts.URL + "/oauth/authorize?" + authQuery(id, ch).Encode())
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "code=") {
		t.Fatalf("second authorization asked again: %d", resp.StatusCode)
	}
	// Connected applications on Settings, and revoking it
	if _, page := user.get("/settings"); !strings.Contains(page, "Connected applications") || !strings.Contains(page, "Shipwright") {
		t.Fatal("Settings does not list the application")
	}
	if c, _ := user.post("/settings/applications/revoke", url.Values{"csrf": {tok}, "client_id": {id}}); c != 200 {
		t.Fatal(c)
	}
	if c := readCurrent(e, access); c != 401 {
		t.Fatalf("revoked token still reads: %d", c)
	}
}

// Every way the authorization request can be wrong.
func TestOAuthAuthorizeErrors(t *testing.T) {
	e := newEnv(t)
	dev := e.browser()
	dev.login("devon", "user")
	id, _ := registerApp(t, e, dev, testRedirect, "https://app.example/cb")
	_, user := e.enroll("jo")
	_, ch := pkcePair()
	get := func(q url.Values) *http.Response {
		resp, err := noFollow(user).Get(e.ts.URL + "/oauth/authorize?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	// wrong redirect: an error page, never a redirect to the given address
	for _, ru := range []string{"http://localhost:9/cb/", "http://localhost:9/cb?x=1", "https://evil.example/cb", "https://app.example/cb#x", ""} {
		q := authQuery(id, ch)
		q.Set("redirect_uri", ru)
		if r := get(q); r.StatusCode != 400 || r.Header.Get("Location") != "" {
			t.Errorf("redirect %q: %d to %q", ru, r.StatusCode, r.Header.Get("Location"))
		}
	}
	// unknown application
	q := authQuery("stc_nope", ch)
	if r := get(q); r.StatusCode != 400 || r.Header.Get("Location") != "" {
		t.Errorf("unknown client: %d", r.StatusCode)
	}
	// missing or bad PKCE, missing state, wrong response type: back to the application with an error
	for name, edit := range map[string]func(url.Values){
		"no challenge":  func(q url.Values) { q.Del("code_challenge") },
		"plain method":  func(q url.Values) { q.Set("code_challenge_method", "plain") },
		"short":         func(q url.Values) { q.Set("code_challenge", "abc") },
		"no state":      func(q url.Values) { q.Del("state") },
		"token flow":    func(q url.Values) { q.Set("response_type", "token") },
		"another scope": func(q url.Values) { q.Set("scope", "write") },
	} {
		q := authQuery(id, ch)
		edit(q)
		r := get(q)
		loc, _ := url.Parse(r.Header.Get("Location"))
		if r.StatusCode != http.StatusSeeOther || loc == nil || loc.Query().Get("error") == "" || loc.Query().Get("code") != "" {
			t.Errorf("%s: %d %s", name, r.StatusCode, r.Header.Get("Location"))
		}
	}
	// the consent page cannot be framed
	r, _ := noFollow(user).Get(e.ts.URL + "/oauth/authorize?" + authQuery(id, ch).Encode())
	r.Body.Close()
	if r.Header.Get("X-Frame-Options") != "DENY" {
		t.Error("consent page may be framed")
	}
	// cancel goes back with access_denied
	_, body := user.get("/oauth/authorize?" + authQuery(id, ch).Encode())
	form := authQuery(id, ch)
	form.Set("csrf", csrfRe.FindStringSubmatch(body)[1])
	form.Set("approve", "0")
	resp, _ := noFollow(user).PostForm(e.ts.URL+"/oauth/authorize", form)
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "error=access_denied") {
		t.Errorf("cancel: %s", resp.Header.Get("Location"))
	}
}

// Every way the code exchange can be wrong, and a code used twice.
func TestOAuthTokenErrors(t *testing.T) {
	e := newEnv(t)
	dev := e.browser()
	dev.login("devon", "user")
	id, secret := registerApp(t, e, dev)
	id2, secret2 := registerApp(t, e, dev)
	_, user := e.enroll("kai")
	fresh := func() (string, string) {
		ver, ch := pkcePair()
		return authorize(t, e, user, authQuery(id, ch)).Query().Get("code"), ver
	}
	form := func(code, ver string) url.Values {
		return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {testRedirect},
			"client_id": {id}, "client_secret": {secret}, "code_verifier": {ver}}
	}
	cases := map[string]func(f url.Values){
		"wrong redirect":      func(f url.Values) { f.Set("redirect_uri", "http://localhost:9/other") },
		"no verifier":         func(f url.Values) { f.Del("code_verifier") },
		"wrong verifier":      func(f url.Values) { v, _ := pkcePair(); f.Set("code_verifier", v) },
		"another application": func(f url.Values) { f.Set("client_id", id2); f.Set("client_secret", secret2) },
		"unknown code":        func(f url.Values) { f.Set("code", "nope") },
		"wrong grant type":    func(f url.Values) { f.Set("grant_type", "password") },
	}
	for name, edit := range cases {
		code, ver := fresh()
		f := form(code, ver)
		edit(f)
		if st, m := tokenReq(t, e, f); st != 400 || m["access_token"] != nil {
			t.Errorf("%s: %d %v", name, st, m)
		}
	}
	// a wrong secret is invalid_client
	code, ver := fresh()
	f := form(code, ver)
	f.Set("client_secret", "sts_wrong")
	if st, m := tokenReq(t, e, f); st != 401 || m["error"] != "invalid_client" {
		t.Errorf("wrong secret: %d %v", st, m)
	}
	// a code used twice: the second use fails and the first use's tokens die
	code, ver = fresh()
	st, m := tokenReq(t, e, form(code, ver))
	if st != 200 {
		t.Fatalf("first use: %d %v", st, m)
	}
	access := m["access_token"].(string)
	if readCurrent(e, access) != 200 {
		t.Fatal("token does not read")
	}
	if st, _ := tokenReq(t, e, form(code, ver)); st != 400 {
		t.Fatalf("second use: %d", st)
	}
	if c := readCurrent(e, access); c != 401 {
		t.Fatalf("tokens of a reused code still read: %d", c)
	}
	// HTTP Basic works as well as the form
	code, ver = fresh()
	f = form(code, ver)
	f.Del("client_id")
	f.Del("client_secret")
	req, _ := http.NewRequest("POST", e.ts.URL+"/oauth/token", strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(id, secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("basic auth: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
	// an expired code (codes live 60 s)
	code, ver = fresh()
	e.srv.SetClock(func() time.Time { return time.Now().Add(2 * time.Minute) })
	if st, _ := tokenReq(t, e, form(code, ver)); st != 400 {
		t.Errorf("expired code: %d", st)
	}
}

// Refresh tokens rotate; an old one used again revokes the family; an
// expired one is refused.
func TestOAuthRefresh(t *testing.T) {
	e := newEnv(t)
	dev := e.browser()
	dev.login("devon", "user")
	id, secret := registerApp(t, e, dev)
	_, user := e.enroll("lee")
	_, refresh := connect(t, e, user, id, secret)
	rf := func(tok string) (int, map[string]any) {
		return tokenReq(t, e, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok}, "client_id": {id}, "client_secret": {secret}})
	}
	st, m := rf(refresh)
	if st != 200 || m["refresh_token"] == refresh {
		t.Fatalf("refresh: %d %v", st, m)
	}
	access2, refresh2 := m["access_token"].(string), m["refresh_token"].(string)
	// the old refresh token again: refused, and everything from this approval dies
	if st, _ := rf(refresh); st != 400 {
		t.Fatalf("old refresh token: %d", st)
	}
	if readCurrent(e, access2) != 401 {
		t.Fatal("a replayed refresh token left the new access token alive")
	}
	if st, _ := rf(refresh2); st != 400 {
		t.Fatal("a replayed refresh token left the new refresh token alive")
	}
	// an access token expires after an hour, a refresh token after 30 days
	access, refresh := connect(t, e, user, id, secret)
	e.srv.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	if c := readCurrent(e, access); c != 401 {
		t.Fatalf("expired access token: %d", c)
	}
	e.srv.SetClock(func() time.Time { return time.Now().Add(31 * 24 * time.Hour) })
	if st, m := rf(refresh); st != 400 || !strings.Contains(m["error_description"].(string), "expired") {
		t.Fatalf("expired refresh token: %d %v", st, m)
	}
}

// A token for user A reads A's private data and nothing private of B's;
// hidden projects stay hidden from applications.
func TestOAuthTokenIsOneUser(t *testing.T) {
	e := newEnv(t)
	dev := e.browser()
	dev.login("devon", "user")
	id, secret := registerApp(t, e, dev)
	_, alice := e.enroll("alice")
	bob, bobBr := e.enroll("bob")
	access, _ := connect(t, e, alice, id, secret)
	sendLocal(t, e, bob, "local:bobs")
	tok := bobBr.csrf("/settings")
	bobBr.post("/settings/profile", url.Values{"csrf": {tok}, "display_name": {"Bob"}, "tz": {"0"}}) // private
	for _, p := range []string{"/api/v1/users/bob/stats", "/api/v1/users/bob/projects", "/api/hackatime/v1/users/bob/stats/last_7_days"} {
		if c, _ := e.apiDo("GET", p, access, nil); c != 403 {
			t.Errorf("%s with alice's token: %d", p, c)
		}
	}
	// "current" is alice, whatever the path says elsewhere
	code, out := e.apiDo("GET", "/api/v1/users/current/stats", access, nil)
	if code != 200 || !strings.Contains(string(out), `"username":"alice"`) {
		t.Fatalf("current: %d %s", code, out)
	}
	// a device token is not a application token and the other way round
	if c, _ := e.apiDo("POST", "/api/v1/sync", access, nil); c != 401 {
		t.Errorf("a application token syncs: %d", c)
	}
}
