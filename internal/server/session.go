package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"seatime/internal/server/store"
)

// session is the signed-cookie payload. It exists for anonymous visitors too
// (UID 0) so every form can carry a CSRF token.
type session struct {
	UID   int64  `json:"u,omitempty"`
	CSRF  string `json:"c"`
	State string `json:"s,omitempty"` // oauth state
	Nonce string `json:"n,omitempty"` // oidc PKCE verifier
	Next  string `json:"x,omitempty"`
	Link  bool   `json:"l,omitempty"` // oauth flow is linking, not logging in
	Ep    int64  `json:"p,omitempty"` // the user's session epoch when signed in (users.session_epoch)
	Exp   int64  `json:"e"`
}

const cookieName = "seatime_session"
const sessionTTL = 30 * 24 * time.Hour

func (s *Server) sign(payload []byte) string {
	m := hmac.New(sha256.New, []byte(s.cfg.Secret))
	m.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) unsign(v string) (*session, bool) {
	i := strings.IndexByte(v, '.')
	if i < 0 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(v[:i])
	if err != nil {
		return nil, false
	}
	want := s.sign(payload)
	if subtle.ConstantTimeCompare([]byte(want), []byte(v)) != 1 {
		return nil, false
	}
	var ss session
	if json.Unmarshal(payload, &ss) != nil || ss.Exp < s.now().Unix() {
		return nil, false
	}
	return &ss, true
}

func (s *Server) saveSession(w http.ResponseWriter, ss *session) {
	ss.Exp = s.now().Add(sessionTTL).Unix()
	b, _ := json.Marshal(ss)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: s.sign(b), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: strings.HasPrefix(s.cfg.BaseURL, "https://"), MaxAge: int(sessionTTL.Seconds()),
	})
}

// session returns the request's session (creating an anonymous one and setting
// the cookie if needed) and the logged-in user, if any.
func (s *Server) session(w http.ResponseWriter, r *http.Request) (*session, *store.User) {
	var ss *session
	if c, err := r.Cookie(cookieName); err == nil {
		ss, _ = s.unsign(c.Value)
	}
	if ss == nil {
		ss = &session{CSRF: randHex(16)}
		s.saveSession(w, ss)
	}
	if ss.UID == 0 {
		return ss, nil
	}
	u, err := s.store.UserByID(r.Context(), ss.UID)
	if err != nil {
		return ss, nil
	}
	if u.SessionEpoch != ss.Ep { // logged out elsewhere, or the role changed
		ss = &session{CSRF: randHex(16)}
		s.saveSession(w, ss)
		return ss, nil
	}
	return ss, u
}

func (s *Server) csrfOK(r *http.Request, ss *session) bool {
	got := r.PostFormValue("csrf")
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(ss.CSRF)) == 1
}

func safeNext(n string) string {
	if n == "" || n[0] != '/' || strings.HasPrefix(n, "//") || strings.ContainsAny(n, "\\\r\n") {
		return "/"
	}
	return n
}
