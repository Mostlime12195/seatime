package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"seatime/internal/server/store"
)

// OAuth 2.0 for applications (RFC 6749, authorization code grant, with PKCE
// S256 required, RFC 7636). An application reads a user's project names, hours
// and commits after the user approves it
// once. One scope, "read". Secrets, codes and tokens are stored hashed.

const (
	oauthScope    = "read"
	accessPrefix  = "sto_"
	refreshPrefix = "str_"
	secretPrefix  = "sts_"
	clientPrefix  = "stc_"
	codeTTL       = 60 * time.Second
	accessTTL     = time.Hour
	refreshTTL    = 30 * 24 * time.Hour
	maxRedirects  = 5
)

func randToken(prefix string) string {
	b := make([]byte, 32)
	rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// oauthUser is the user behind a valid access token, or nil.
func (s *Server) oauthUser(ctx context.Context, tok string) *store.User {
	t, err := s.store.OAuthToken(ctx, hashHex(tok))
	if err != nil || t.Kind != "access" || t.Revoked || !s.now().Before(t.Expires) {
		return nil
	}
	u, err := s.store.UserByID(ctx, t.UserID)
	if err != nil {
		return nil
	}
	return u
}

// validRedirect is a redirect URI an application may register: https, or http
// on this computer (for development); no fragment, no user info.
func validRedirect(raw string) bool {
	if len(raw) > 300 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.User != nil || u.Host == "" || u.Opaque != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return false
}

var pkceRe = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// ---------------- authorization ----------------

type consentView struct {
	Client                               *store.OAuthClient
	Developer                            string
	RedirectURI, State, Challenge, Scope string
	Err                                  string
	// Preview is what the application would read right now: the user's
	// projects as the API names them, with checked hours, commits and
	// status. More counts the rows past the first previewRows; Hidden
	// counts the projects it will not see.
	Preview      []previewRow
	More, Hidden int
}

type previewRow struct {
	Name    string
	Seconds float64
	Commits int
	Status  string
}

const previewRows = 8

// preview fills v with what the application would read for u now. It
// reads like an application's token would, but never asks u's computers
// for prompts: nothing is shared before u allows it.
func (s *Server) preview(ctx context.Context, u *store.User, v *consentView) {
	ps, err := s.store.ListProjects(ctx, u.ID)
	if err != nil {
		return
	}
	now := s.now()
	commits := map[string]int{}
	for i := range ps {
		p := &ps[i]
		if p.Hidden {
			v.Hidden++
			continue
		}
		n, _ := s.store.CommitCount(ctx, p.ID, time.Unix(0, 0), now.Add(time.Hour))
		commits[firstNonEmpty(p.Name, p.Key)] += n
	}
	aggs, err := s.aggregate(ctx, u, time.Unix(0, 0), now.Add(time.Minute), nil, false, nil, false)
	if err != nil {
		return
	}
	sortAggs(aggs, false)
	for i, a := range aggs {
		if i == previewRows {
			v.More = len(aggs) - previewRows
			break
		}
		v.Preview = append(v.Preview, previewRow{Name: a.Name, Seconds: a.Struggle, Commits: commits[a.Name], Status: a.St.Status})
	}
}

// authorizeParams are a request's validated parameters. A request whose
// client or redirect URI is wrong gets an error page and is never sent
// anywhere; any other problem goes back to the application's redirect URI.
func (s *Server) authorizeParams(w http.ResponseWriter, r *http.Request, get func(string) string) (*consentView, bool) {
	c, err := s.store.OAuthClient(r.Context(), get("client_id"))
	if err != nil {
		s.oauthPage(w, r, 400, "This application is not registered on this server.")
		return nil, false
	}
	ru := get("redirect_uri")
	exact := false
	for _, x := range c.RedirectURIs {
		if x == ru {
			exact = true
		}
	}
	if !exact {
		s.oauthPage(w, r, 400, "The application sent a redirect address that it did not register. Nothing was shared.")
		return nil, false
	}
	v := &consentView{Client: c, RedirectURI: ru, State: get("state"), Challenge: get("code_challenge"), Scope: oauthScope}
	fail := func(code, desc string) (*consentView, bool) {
		q := url.Values{"error": {code}, "error_description": {desc}}
		if v.State != "" {
			q.Set("state", v.State)
		}
		http.Redirect(w, r, withQuery(ru, q), http.StatusSeeOther)
		return nil, false
	}
	switch {
	case get("response_type") != "code":
		return fail("unsupported_response_type", "response_type must be code")
	case v.State == "" || len(v.State) > 500:
		return fail("invalid_request", "state is required")
	case get("code_challenge_method") != "S256" || !pkceRe.MatchString(v.Challenge):
		return fail("invalid_request", "PKCE is required: code_challenge with code_challenge_method=S256")
	}
	if sc := get("scope"); sc != "" && sc != oauthScope {
		return fail("invalid_scope", "the only scope is read")
	}
	if dev, err := s.store.UserByID(r.Context(), c.UserID); err == nil {
		v.Developer = dev.Username
	}
	return v, true
}

func withQuery(raw string, q url.Values) string {
	u, _ := url.Parse(raw)
	cur := u.Query()
	for k, vs := range q {
		cur[k] = vs
	}
	u.RawQuery = cur.Encode()
	return u.String()
}

func (s *Server) oauthPage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	ss, u := s.session(w, r)
	b := s.base(ss, u, "Connect an application")
	s.pages.exec(w, status, "consent", "layout", b, &consentView{Err: msg})
}

// handleAuthorize shows the consent screen, or sends a code straight back
// when the user approved this application before.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	q := r.URL.Query()
	v, ok := s.authorizeParams(w, r, q.Get)
	if !ok {
		return
	}
	ss, u, ok := s.requireLogin(w, r)
	if !ok {
		return
	}
	if g, err := s.store.OAuthGrantFor(r.Context(), u.ID, v.Client.ID); err == nil && g.Scope == oauthScope {
		s.issueCode(w, r, u, v)
		return
	}
	s.preview(r.Context(), u, v)
	s.pages.render(w, r, "consent", s.base(ss, u, "Connect "+v.Client.Name), v)
}

// handleAuthorizeDecision is the consent form's answer.
func (s *Server) handleAuthorizeDecision(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Frame-Options", "DENY")
	_, u, ok := s.postGuard(w, r)
	if !ok {
		return
	}
	v, ok := s.authorizeParams(w, r, r.PostFormValue)
	if !ok {
		return
	}
	if r.PostFormValue("approve") != "1" {
		http.Redirect(w, r, withQuery(v.RedirectURI, url.Values{"error": {"access_denied"}, "state": {v.State}}), http.StatusSeeOther)
		return
	}
	if err := s.store.PutOAuthGrant(r.Context(), u.ID, v.Client.ID, oauthScope, s.now()); err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	s.log.Info("oauth: application approved", "user", u.Username, "client", v.Client.ID)
	s.issueCode(w, r, u, v)
}

func (s *Server) issueCode(w http.ResponseWriter, r *http.Request, u *store.User, v *consentView) {
	code := randToken("")
	c := store.OAuthCode{ClientID: v.Client.ID, UserID: u.ID, RedirectURI: v.RedirectURI, Challenge: v.Challenge, Scope: oauthScope,
		Expires: s.now().Add(codeTTL), Family: randHex(12)}
	if err := s.store.PutOAuthCode(r.Context(), hashHex(code), c); err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	http.Redirect(w, r, withQuery(v.RedirectURI, url.Values{"code": {code}, "state": {v.State}}), http.StatusSeeOther)
}

// ---------------- tokens ----------------

func oauthErr(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	if status == 401 {
		w.Header().Set("WWW-Authenticate", `Basic realm="seatime"`)
	}
	jsonOut(w, status, map[string]string{"error": code, "error_description": desc})
}

// tokenClient authenticates the application at the token endpoint: HTTP Basic,
// or client_id and client_secret in the form.
func (s *Server) tokenClient(r *http.Request) (*store.OAuthClient, bool) {
	id, secret, ok := r.BasicAuth()
	if ok {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	}
	if id == "" || secret == "" {
		return nil, false
	}
	c, err := s.store.OAuthClient(r.Context(), id)
	if err != nil {
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(hashHex(secret)), []byte(c.SecretHash)) != 1 {
		return nil, false
	}
	return c, true
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if !s.allowIP(r, "oauth-token", 5, 60) {
		oauthErr(w, 429, "slow_down", "too many requests")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthErr(w, 400, "invalid_request", "bad form")
		return
	}
	c, ok := s.tokenClient(r)
	if !ok {
		oauthErr(w, 401, "invalid_client", "unknown application or wrong secret")
		return
	}
	ctx := r.Context()
	now := s.now()
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		code, err := s.store.UseOAuthCode(ctx, hashHex(r.PostFormValue("code")))
		switch {
		case err != nil:
			oauthErr(w, 400, "invalid_grant", "unknown code")
			return
		case code.Used:
			// a code used twice: someone else may hold it; revoke what it gave
			_ = s.store.RevokeOAuthFamily(ctx, code.Family)
			s.log.Warn("oauth: authorization code used twice; its tokens are revoked", "client", c.ID)
			oauthErr(w, 400, "invalid_grant", "code already used")
			return
		case code.ClientID != c.ID:
			oauthErr(w, 400, "invalid_grant", "code was issued to another application")
			return
		case !now.Before(code.Expires):
			oauthErr(w, 400, "invalid_grant", "code expired")
			return
		case r.PostFormValue("redirect_uri") != code.RedirectURI:
			oauthErr(w, 400, "invalid_grant", "redirect_uri does not match the authorization request")
			return
		}
		ver := r.PostFormValue("code_verifier")
		sum := sha256.Sum256([]byte(ver))
		if !pkceRe.MatchString(ver) || subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(code.Challenge)) != 1 {
			oauthErr(w, 400, "invalid_grant", "code_verifier does not match the code_challenge")
			return
		}
		if _, err := s.store.OAuthGrantFor(ctx, code.UserID, c.ID); err != nil {
			oauthErr(w, 400, "invalid_grant", "the user revoked this application")
			return
		}
		s.issueTokens(w, r, c.ID, code.UserID, code.Family)
	case "refresh_token":
		hash := hashHex(r.PostFormValue("refresh_token"))
		t, err := s.store.OAuthToken(ctx, hash)
		switch {
		case err != nil || t.Kind != "refresh":
			oauthErr(w, 400, "invalid_grant", "unknown refresh token")
			return
		case t.ClientID != c.ID:
			oauthErr(w, 400, "invalid_grant", "refresh token was issued to another application")
			return
		case t.Revoked:
			// a refresh token used after it was replaced: someone else may
			// hold it; revoke everything issued from that approval
			_ = s.store.RevokeOAuthFamily(ctx, t.Family)
			oauthErr(w, 400, "invalid_grant", "refresh token revoked")
			return
		case !now.Before(t.Expires):
			oauthErr(w, 400, "invalid_grant", "refresh token expired")
			return
		}
		if _, err := s.store.OAuthGrantFor(ctx, t.UserID, c.ID); err != nil {
			oauthErr(w, 400, "invalid_grant", "the user revoked this application")
			return
		}
		if err := s.store.RevokeOAuthToken(ctx, hash); err != nil {
			oauthErr(w, 500, "server_error", "internal error")
			return
		}
		s.issueTokens(w, r, c.ID, t.UserID, t.Family)
	default:
		oauthErr(w, 400, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (s *Server) issueTokens(w http.ResponseWriter, r *http.Request, clientID string, userID int64, family string) {
	now := s.now()
	access, refresh := randToken(accessPrefix), randToken(refreshPrefix)
	for _, t := range []struct {
		tok, kind string
		ttl       time.Duration
	}{{access, "access", accessTTL}, {refresh, "refresh", refreshTTL}} {
		if err := s.store.PutOAuthToken(r.Context(), hashHex(t.tok), store.OAuthToken{Kind: t.kind, ClientID: clientID, UserID: userID,
			Scope: oauthScope, Family: family, Expires: now.Add(t.ttl)}, now); err != nil {
			oauthErr(w, 500, "server_error", "internal error")
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonOut(w, 200, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": int(accessTTL.Seconds()),
		"refresh_token": refresh, "scope": oauthScope})
}

// handleRevoke is RFC 7009: an application gives up a token.
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthErr(w, 400, "invalid_request", "bad form")
		return
	}
	c, ok := s.tokenClient(r)
	if !ok {
		oauthErr(w, 401, "invalid_client", "unknown application or wrong secret")
		return
	}
	hash := hashHex(r.PostFormValue("token"))
	if t, err := s.store.OAuthToken(r.Context(), hash); err == nil && t.ClientID == c.ID {
		_ = s.store.RevokeOAuthToken(r.Context(), hash)
	}
	w.WriteHeader(200)
}

// ---------------- the developer page ----------------

type developersView struct {
	Apps []store.OAuthClient
	// Secret is shown once, right after it is made, for Shown.
	Secret, Shown string
	Err           string
	BaseURL       string
}

func parseRedirects(v string) ([]string, bool) {
	var out []string
	for _, l := range strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == '\r' || r == ' ' || r == ',' }) {
		if !validRedirect(l) {
			return nil, false
		}
		out = append(out, l)
	}
	return out, len(out) > 0 && len(out) <= maxRedirects
}

func (s *Server) renderDevelopers(w http.ResponseWriter, r *http.Request, ss *session, u *store.User, v developersView) {
	v.Apps, _ = s.store.UserOAuthClients(r.Context(), u.ID)
	v.BaseURL = s.cfg.BaseURL
	s.pages.render(w, r, "developers", s.base(ss, u, "Applications you made"), v)
}

func (s *Server) handleDevelopers(w http.ResponseWriter, r *http.Request) {
	ss, u, ok := s.requireLogin(w, r)
	if !ok {
		return
	}
	s.renderDevelopers(w, r, ss, u, developersView{})
}

const redirectRule = "Each redirect address must start with https://, or with http://localhost or http://127.0.0.1 while you test. Up to 5."

// handleDevelopersNew registers an application and shows its secret once.
func (s *Server) handleDevelopersNew(w http.ResponseWriter, r *http.Request) {
	ss, u, ok := s.postGuard(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	uris, okURIs := parseRedirects(r.PostFormValue("redirect_uris"))
	switch {
	case name == "" || len([]rune(name)) > 60:
		s.renderDevelopers(w, r, ss, u, developersView{Err: "Give the application a name of up to 60 characters."})
		return
	case !okURIs:
		s.renderDevelopers(w, r, ss, u, developersView{Err: redirectRule})
		return
	}
	if cs, _ := s.store.UserOAuthClients(r.Context(), u.ID); len(cs) >= 20 {
		s.renderDevelopers(w, r, ss, u, developersView{Err: "You have 20 applications, the most one account may have."})
		return
	}
	secret := randToken(secretPrefix)
	c := store.OAuthClient{ID: clientPrefix + randHex(12), UserID: u.ID, Name: name, RedirectURIs: uris, SecretHash: hashHex(secret), Created: s.now()}
	if err := s.store.CreateOAuthClient(r.Context(), c); err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	s.renderDevelopers(w, r, ss, u, developersView{Secret: secret, Shown: c.ID})
}

// ownClient is one of u's applications, by the path's id.
func (s *Server) ownClient(r *http.Request, u *store.User) (*store.OAuthClient, bool) {
	c, err := s.store.OAuthClient(r.Context(), r.PathValue("id"))
	if err != nil || c.UserID != u.ID {
		return nil, false
	}
	return c, true
}

func (s *Server) handleDevelopersEdit(w http.ResponseWriter, r *http.Request) {
	ss, u, ok := s.postGuard(w, r)
	if !ok {
		return
	}
	c, ok := s.ownClient(r, u)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch r.PostFormValue("do") {
	case "secret":
		secret := randToken(secretPrefix)
		c.SecretHash = hashHex(secret)
		if err := s.store.UpdateOAuthClient(r.Context(), *c); err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		s.renderDevelopers(w, r, ss, u, developersView{Secret: secret, Shown: c.ID})
		return
	case "delete":
		if err := s.store.DeleteOAuthClient(r.Context(), u.ID, c.ID); err != nil {
			http.Error(w, "internal error", 500)
			return
		}
	default:
		name := strings.TrimSpace(r.PostFormValue("name"))
		uris, okURIs := parseRedirects(r.PostFormValue("redirect_uris"))
		if name == "" || len([]rune(name)) > 60 || !okURIs {
			s.renderDevelopers(w, r, ss, u, developersView{Err: "Give the application a name of up to 60 characters. " + redirectRule})
			return
		}
		c.Name, c.RedirectURIs = name, uris
		if err := s.store.UpdateOAuthClient(r.Context(), *c); err != nil {
			http.Error(w, "internal error", 500)
			return
		}
	}
	http.Redirect(w, r, "/developers", http.StatusSeeOther)
}

// handleAppRevoke takes back an application's access to the user's hours.
func (s *Server) handleAppRevoke(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.postGuard(w, r)
	if !ok {
		return
	}
	err := s.store.RevokeOAuthGrant(r.Context(), u.ID, r.PostFormValue("client_id"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	http.Redirect(w, r, "/settings?ok="+urlEscape("The application can no longer read your hours.")+"#applications", http.StatusSeeOther)
}
