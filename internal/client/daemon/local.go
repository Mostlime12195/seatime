package daemon

import (
	"fmt"
	"io"
	"net/http"

	"seatime/internal/client/emit"
	"seatime/internal/client/localauth"
)

// guard is the outermost protection for every local endpoint: only loopback
// Host names are served (DNS-rebinding defence). See package localauth.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !localauth.HostOK(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorize writes the error response and returns false when r may not use a
// secret-protected endpoint.
func authorize(w http.ResponseWriter, r *http.Request) bool {
	switch localauth.Authorize(r.Host, r.Header.Get("Origin"), r.Header.Get("Authorization")) {
	case localauth.Allow:
		return true
	case localauth.DenyHost:
		http.Error(w, "forbidden host", http.StatusForbidden)
	case localauth.DenyOrigin:
		http.Error(w, "requests from a browser are not accepted", http.StatusForbidden)
	default:
		w.Header().Set("WWW-Authenticate", `Bearer realm="seatime"`)
		http.Error(w, "missing or wrong secret (Authorization: Bearer <secret>; see `seatime token`)", http.StatusUnauthorized)
	}
	return false
}

// handleEmit is the plugin protocol over HTTP: the same payload as `seatime emit`.
// It needs the per-install secret; browsers' web pages can never use it.
func (d *Daemon) handleEmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !authorize(w, r) {
		return
	}
	b, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	evs, err := emit.DecodeEvents(b, emit.Defaults{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, e := range evs {
		if err := d.Pipeline.Emit(e); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"accepted":%d}`, len(evs))
}
