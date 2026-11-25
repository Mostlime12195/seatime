package server

import (
	"net/http"

	"seatime/internal/server/store"
)

// adminOnly lets an admin through; anyone else gets a 404.
func (s *Server) adminOnly(w http.ResponseWriter, r *http.Request) (*session, *store.User, bool) {
	ss, u, ok := s.requireLogin(w, r)
	if !ok {
		return nil, nil, false
	}
	if u.Role != store.RoleAdmin {
		s.notFound(w, r)
		return nil, nil, false
	}
	return ss, u, true
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ss, u, ok := s.adminOnly(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	b := s.base(ss, u, "Admin")
	b.Flash = r.URL.Query().Get("ok")
	s.pages.render(w, r, "admin", b, map[string]any{
		"Check": s.checkStatus(ctx), "Settings": s.settingRows(ctx), "Log": s.settingsLog(ctx)})
}
