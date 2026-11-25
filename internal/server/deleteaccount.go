package server

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// deletedGrace is how long a deleted account's computers have to remove
// SeaTime before the last of the account goes anyway.
const deletedGrace = 30 * 24 * time.Hour

// handleDeleteAccount deletes the signed-in account once its owner has
// typed the username (store.DeleteAccount). Each computer that can remove
// SeaTime from itself is asked to, with its data; the rest are
// disconnected. The Hack Club account is not touched.
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.postGuard(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	typed := strings.TrimPrefix(strings.TrimSpace(r.PostFormValue("confirm")), "@")
	if !strings.EqualFold(typed, u.Username) {
		http.Redirect(w, r, "/settings?ok="+urlEscape("Type your username to delete your account.")+"#delete", http.StatusSeeOther)
		return
	}
	devs, err := s.store.ListDevices(ctx, u.ID)
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	var uninstall []string
	for _, row := range s.deviceRows(ctx, u.ID, devs) {
		if !row.Revoked && row.CanRemove {
			uninstall = append(uninstall, row.ID)
		}
	}
	if err := s.store.DeleteAccount(ctx, u.ID, uninstall, s.now()); err != nil {
		s.log.Error("delete account", "err", err)
		http.Error(w, "internal error", 500)
		return
	}
	s.log.Info("account deleted", "user", u.ID, "computers_to_remove", len(uninstall))
	s.saveSession(w, &session{CSRF: randHex(16)})
	http.Redirect(w, r, "/?deleted=1", http.StatusSeeOther)
}

// purgeDeleted removes what is left of deleted accounts whose computers
// are done, or whose time is up.
func (s *Server) purgeDeleted(ctx context.Context) {
	if n, err := s.store.PurgeDeleted(ctx, s.now().Add(-deletedGrace)); err != nil {
		s.log.Error("purge deleted accounts", "err", err)
	} else if n > 0 {
		s.log.Info("purged deleted accounts", "n", n)
	}
}

// purgeLoop runs purgeDeleted at start and every hour.
func (s *Server) purgeLoop(ctx context.Context) {
	s.purgeDeleted(ctx)
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.purgeDeleted(ctx)
		}
	}
}
