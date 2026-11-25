package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"seatime/internal/server/store"
)

var authHTTP = &http.Client{Timeout: 10 * time.Second}

func (s *Server) hcaEnabled() bool { return s.cfg.HCAClientID != "" && s.cfg.HCASecret != "" }
func (s *Server) ghEnabled() bool  { return s.cfg.GHClientID != "" && s.cfg.GHSecret != "" }

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ss, u := s.session(w, r)
	next := safeNext(r.URL.Query().Get("next"))
	if u != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	ss.Next = next
	s.saveSession(w, ss)
	s.pages.render(w, r, "login", s.base(ss, nil, "Sign in"), map[string]any{
		"HCA": s.hcaEnabled(), "GH": s.ghEnabled(), "Dev": s.cfg.Dev, "Next": next})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	ss, u := s.session(w, r)
	if !s.csrfOK(r, ss) {
		http.Error(w, "bad csrf token", 403)
		return
	}
	if u != nil { // every session of this account ends, not just this cookie
		if err := s.store.EndSessions(r.Context(), u.ID); err != nil {
			s.log.Error("end sessions", "err", err)
		}
	}
	s.saveSession(w, &session{CSRF: randHex(16)})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// finishLogin rotates the session (fresh CSRF) and applies configured roles
// for the identity that just authenticated.
func (s *Server) finishLogin(w http.ResponseWriter, r *http.Request, ss *session, u *store.User, id store.Identity, slackID string) {
	s.log.Info("login", "user", u.Username, "identity", identityKey(id)) // the id SEATIME_ADMIN_IDS takes
	s.applyRoles(r.Context(), u, id, slackID)
	if fresh, err := s.store.UserByID(r.Context(), u.ID); err == nil {
		u = fresh // a role change just ended the old sessions: take the new epoch
	}
	next := safeNext(ss.Next)
	s.saveSession(w, &session{UID: u.ID, CSRF: randHex(16), Ep: u.SessionEpoch})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// identityKey is how SEATIME_ADMIN_IDS names a login:
// "provider:subject" with the provider's stable id ("hackclub:<sub>",
// "github:<numeric id>", "dev:<name>"). A Hack Club login can also be named
// "slack:<member id>", using the Slack ID Hack Club Auth returns.
func identityKey(id store.Identity) string { return id.Provider + ":" + id.Subject }

// applyRoles sets u's role from the admin list (setting admin_ids) for id, a login its provider just verified:
// listed ones are promoted, and someone no longer listed loses the role.
// Usernames never grant a role: they are first-come and derived from
// names users pick themselves.
func (s *Server) applyRoles(ctx context.Context, u *store.User, id store.Identity, slackID string) {
	keys := []string{identityKey(id)}
	if id.Provider == "hackclub" && slackID != "" { // only Hack Club Auth vouches for a Slack ID
		keys = append(keys, "slack:"+slackID)
	}
	if ids, err := s.store.UserIdentities(ctx, u.ID); err == nil {
		for _, x := range ids {
			keys = append(keys, identityKey(x))
		}
	}
	s.setRoleFromLists(ctx, u, keys)
}

// roleFor is the role the lists give a user known by keys.
func roleFor(p *policy, keys []string) string {
	in := func(set map[string]bool) bool {
		for _, k := range keys {
			if set[k] {
				return true
			}
		}
		return false
	}
	if in(p.Admins) {
		return store.RoleAdmin
	}
	return store.RoleUser
}

// setRoleFromLists gives u the role the lists name; a change also ends
// u's sessions (SetRole). With SEATIME_DEV=1 a dev login keeps the role it
// picked on the dev form.
func (s *Server) setRoleFromLists(ctx context.Context, u *store.User, keys []string) {
	want := roleFor(s.pol(), keys)
	if want == u.Role {
		return
	}
	if s.cfg.Dev && want == store.RoleUser && slices.ContainsFunc(keys, func(k string) bool { return strings.HasPrefix(k, "dev:") }) {
		return
	}
	if err := s.store.SetRole(ctx, u.ID, want); err != nil {
		s.log.Error("set role", "user", u.Username, "err", err)
		return
	}
	s.log.Info("role changed by the admin list", "user", u.Username, "from", u.Role, "to", want)
	u.Role = want
}

// syncRoles applies the lists to everyone at once: at start and whenever
// an admin changes them. Staff no longer listed lose their role at once
// (and are signed out); listed accounts that already exist gain theirs
// without signing in again.
func (s *Server) syncRoles(ctx context.Context, before, after *policy) error {
	if before.src != nil && before.src["admin_ids"] == after.src["admin_ids"] {
		return nil
	}
	seen := map[int64]bool{}
	apply := func(u *store.User) {
		if u == nil || seen[u.ID] {
			return
		}
		seen[u.ID] = true
		var keys []string
		if u.SlackID != "" { // users.slack_id only ever comes from Hack Club Auth
			keys = append(keys, "slack:"+u.SlackID)
		}
		ids, err := s.store.UserIdentities(ctx, u.ID)
		if err != nil {
			return
		}
		for _, x := range ids {
			keys = append(keys, identityKey(x))
		}
		s.setRoleFromLists(ctx, u, keys)
	}
	staff, err := s.store.StaffUsers(ctx)
	if err != nil {
		return err
	}
	for i := range staff {
		apply(&staff[i])
	}
	for k := range after.Admins {
		prov, sub, _ := strings.Cut(k, ":")
		var u *store.User
		if prov == "slack" {
			u, err = s.store.UserBySlackID(ctx, sub)
		} else {
			u, err = s.store.UserByIdentity(ctx, prov, sub)
		}
		if err == nil {
			apply(u)
		}
	}
	return nil
}

// ---------------- dev login ----------------

func (s *Server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Dev {
		http.NotFound(w, r)
		return
	}
	ss, _ := s.session(w, r)
	if !s.csrfOK(r, ss) {
		http.Error(w, "bad csrf token", 403)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("username"))
	if name == "" {
		http.Error(w, "username required", 400)
		return
	}
	id := store.Identity{Provider: "dev", Subject: strings.ToLower(name)}
	u, err := s.store.LoginIdentity(r.Context(), id, name, name, "", "", "")
	if err != nil {
		http.Error(w, "login failed", 500)
		return
	}
	if role := r.PostFormValue("role"); role == store.RoleAdmin {
		if s.store.SetRole(r.Context(), u.ID, role) == nil {
			u.Role = role
		}
	}
	ss.Next = safeNext(r.PostFormValue("next"))
	s.finishLogin(w, r, ss, u, id, "")
}

// ---------------- OAuth plumbing ----------------

func (s *Server) startOAuth(w http.ResponseWriter, r *http.Request, ss *session, u *store.User) bool {
	ss.State = randHex(16)
	ss.Link = r.URL.Query().Get("link") == "1" && u != nil
	if n := r.URL.Query().Get("next"); n != "" {
		ss.Next = safeNext(n)
	}
	return true
}

func (s *Server) checkState(w http.ResponseWriter, r *http.Request, ss *session) bool {
	q := r.URL.Query()
	if ss.State == "" || q.Get("state") != ss.State || q.Get("code") == "" {
		http.Error(w, "login failed: bad or expired state (try again)", 400)
		return false
	}
	return true
}

func postForm(ctx context.Context, endpoint string, form url.Values, hdr map[string]string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := authHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: status %d", endpoint, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func getJSON(ctx context.Context, endpoint, bearer string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "seatimed")
	resp, err := authHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: status %d", endpoint, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---------------- Hack Club Auth (OIDC, authorization code + PKCE) ----------------

type oidcMeta struct {
	Auth, Token, Userinfo string
}

func (s *Server) oidcMeta(ctx context.Context) (*oidcMeta, error) {
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()
	if s.oidc != nil {
		return s.oidc, nil
	}
	var d struct {
		Issuer string `json:"issuer"`
		Auth   string `json:"authorization_endpoint"`
		Token  string `json:"token_endpoint"`
		UI     string `json:"userinfo_endpoint"`
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(s.cfg.HCAIssuer, "/")+"/.well-known/openid-configuration", nil)
	resp, err := authHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil || d.Auth == "" || d.Token == "" || d.UI == "" {
		return nil, fmt.Errorf("bad oidc discovery document")
	}
	if strings.TrimRight(d.Issuer, "/") != strings.TrimRight(s.cfg.HCAIssuer, "/") {
		return nil, fmt.Errorf("oidc issuer mismatch")
	}
	s.oidc = &oidcMeta{d.Auth, d.Token, d.UI}
	return s.oidc, nil
}

func (s *Server) handleHCAStart(w http.ResponseWriter, r *http.Request) {
	if !s.hcaEnabled() {
		http.NotFound(w, r)
		return
	}
	ss, u := s.session(w, r)
	m, err := s.oidcMeta(r.Context())
	if err != nil {
		s.log.Error("oidc discovery", "err", err)
		http.Error(w, "Hack Club Auth is unreachable right now", 502)
		return
	}
	s.startOAuth(w, r, ss, u)
	ss.Nonce = randHex(32) // PKCE verifier
	chal := sha256.Sum256([]byte(ss.Nonce))
	s.saveSession(w, ss)
	q := url.Values{
		"client_id": {s.cfg.HCAClientID}, "redirect_uri": {s.cfg.BaseURL + "/auth/hackclub/callback"},
		"response_type": {"code"}, "scope": {s.cfg.HCAScopes}, "state": {ss.State},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(chal[:])}, "code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, m.Auth+"?"+q.Encode(), http.StatusFound)
}

func (s *Server) handleHCACallback(w http.ResponseWriter, r *http.Request) {
	if !s.hcaEnabled() {
		http.NotFound(w, r)
		return
	}
	ss, u := s.session(w, r)
	if !s.checkState(w, r, ss) {
		return
	}
	ctx := r.Context()
	m, err := s.oidcMeta(ctx)
	if err != nil {
		http.Error(w, "Hack Club Auth is unreachable right now", 502)
		return
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	err = postForm(ctx, m.Token, url.Values{
		"grant_type": {"authorization_code"}, "code": {r.URL.Query().Get("code")},
		"redirect_uri": {s.cfg.BaseURL + "/auth/hackclub/callback"}, "client_id": {s.cfg.HCAClientID},
		"client_secret": {s.cfg.HCASecret}, "code_verifier": {ss.Nonce}}, nil, &tok)
	if err != nil || tok.AccessToken == "" {
		s.log.Error("hca token", "err", err)
		http.Error(w, "login failed", 502)
		return
	}
	// The token came straight from the issuer over TLS, so userinfo is trusted.
	var ui struct {
		Sub       string `json:"sub"`
		Nickname  string `json:"nickname"`
		Name      string `json:"name"`
		GivenName string `json:"given_name"`
		SlackID   string `json:"slack_id"`
		Picture   string `json:"picture"`
	}
	if err := getJSON(ctx, m.Userinfo, tok.AccessToken, &ui); err != nil || ui.Sub == "" {
		s.log.Error("hca userinfo", "err", err)
		http.Error(w, "login failed", 502)
		return
	}
	want := firstNonEmpty(ui.Nickname, ui.GivenName, ui.Name, ui.SlackID, "hacker")
	id := store.Identity{Provider: "hackclub", Subject: ui.Sub}
	if ss.Link && u != nil {
		if err := s.store.LinkIdentity(ctx, u.ID, id, ""); err != nil {
			http.Error(w, "that Hack Club account is already linked to another SeaTime user", 409)
			return
		}
		if ui.SlackID != "" {
			s.store.SetSlackID(ctx, u.ID, ui.SlackID) // so YSWS programs can look this user up by Slack ID
		}
		s.applyRoles(ctx, u, id, ui.SlackID)
		ss.State, ss.Link = "", false
		s.saveSession(w, ss)
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	nu, err := s.store.LoginIdentity(ctx, id, want, firstNonEmpty(ui.Nickname, ui.Name), ui.Picture, ui.SlackID, "")
	if err != nil {
		s.log.Error("login", "err", err)
		http.Error(w, "login failed", 500)
		return
	}
	s.finishLogin(w, r, ss, nu, id, ui.SlackID)
}

// ---------------- GitHub OAuth ----------------

func (s *Server) handleGHStart(w http.ResponseWriter, r *http.Request) {
	if !s.ghEnabled() {
		http.NotFound(w, r)
		return
	}
	ss, u := s.session(w, r)
	s.startOAuth(w, r, ss, u)
	s.saveSession(w, ss)
	q := url.Values{"client_id": {s.cfg.GHClientID}, "redirect_uri": {s.cfg.BaseURL + "/auth/github/callback"},
		"state": {ss.State}, "scope": {""}}
	http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}

func (s *Server) handleGHCallback(w http.ResponseWriter, r *http.Request) {
	if !s.ghEnabled() {
		http.NotFound(w, r)
		return
	}
	ss, u := s.session(w, r)
	if !s.checkState(w, r, ss) {
		return
	}
	ctx := r.Context()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	err := postForm(ctx, "https://github.com/login/oauth/access_token", url.Values{
		"client_id": {s.cfg.GHClientID}, "client_secret": {s.cfg.GHSecret}, "code": {r.URL.Query().Get("code")},
		"redirect_uri": {s.cfg.BaseURL + "/auth/github/callback"}}, nil, &tok)
	if err != nil || tok.AccessToken == "" {
		s.log.Error("github token", "err", err)
		http.Error(w, "login failed", 502)
		return
	}
	var gu struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := getJSON(ctx, "https://api.github.com/user", tok.AccessToken, &gu); err != nil || gu.ID == 0 {
		s.log.Error("github user", "err", err)
		http.Error(w, "login failed", 502)
		return
	}
	id := store.Identity{Provider: "github", Subject: fmt.Sprint(gu.ID)}
	if ss.Link && u != nil {
		if err := s.store.LinkIdentity(ctx, u.ID, id, gu.Login); err != nil {
			http.Error(w, "that GitHub account is already linked to another SeaTime user", 409)
			return
		}
		s.applyRoles(ctx, u, id, "")
		ss.State, ss.Link = "", false
		s.saveSession(w, ss)
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	nu, err := s.store.LoginIdentity(ctx, id, gu.Login, firstNonEmpty(gu.Name, gu.Login), gu.AvatarURL, "", gu.Login)
	if err != nil {
		s.log.Error("login", "err", err)
		http.Error(w, "login failed", 500)
		return
	}
	s.finishLogin(w, r, ss, nu, id, "")
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
