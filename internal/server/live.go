package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const (
	maxSSE      = 3000
	maxSSEPerIP = 20
	heartbeat   = 20 * time.Second
)

func (s *Server) acquireSSE(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	if s.hub.Subscribers() >= maxSSE {
		http.Error(w, "too many live connections", 503)
		return nil, false
	}
	ip := s.clientIP(r)
	limit := int32(maxSSEPerIP)
	if s.cfg.TrustProxy && unknownClient(ip) {
		ip, limit = "unknown", maxSSEPerIP*unknownIPFactor // shared by every client whose address was lost
	}
	v, _ := s.sseIP.LoadOrStore(ip, new(int32))
	c := v.(*int32)
	if atomic.AddInt32(c, 1) > limit {
		atomic.AddInt32(c, -1)
		http.Error(w, "too many live connections from your address", 429)
		return nil, false
	}
	return func() {
		if atomic.AddInt32(c, -1) <= 0 {
			s.sseIP.Delete(ip)
		}
	}, true
}

type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (e sseWriter) send(event string, data []byte) error {
	e.rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	var b strings.Builder
	if event != "" {
		b.WriteString("event: " + event + "\n")
	}
	b.WriteString("data: ")
	b.Write(data)
	b.WriteString("\n\n")
	if _, err := e.w.Write([]byte(b.String())); err != nil {
		return err
	}
	return e.rc.Flush()
}

func (e sseWriter) comment(c string) error {
	e.rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := e.w.Write([]byte(": " + c + "\n\n")); err != nil {
		return err
	}
	return e.rc.Flush()
}

func (s *Server) sseStart(w http.ResponseWriter) sseWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // proxies must not buffer
	w.WriteHeader(200)
	e := sseWriter{w, http.NewResponseController(w)}
	w.Write([]byte("retry: 4000\n"))
	e.comment("ok")
	return e
}

func (s *Server) handleLiveUser(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.PathValue("user"), "@")
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, viewer := s.session(w, r)
	u, err := s.store.UserByName(r.Context(), name)
	if err != nil || !s.visible(u, viewer) {
		http.NotFound(w, r)
		return
	}
	release, ok := s.acquireSSE(w, r)
	if !ok {
		return
	}
	defer release()
	sub := s.hub.Subscribe("user:" + strings.ToLower(u.Username))
	defer sub.Close()
	e := s.sseStart(w)
	hb := time.NewTicker(heartbeat)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-sub.C:
			if e.send("sync", m) != nil {
				return
			}
		case <-hb.C:
			if e.comment("ping") != nil {
				return
			}
		}
	}
}

// globalJSON is the live-ticker payload. It is built once per second for all
// watchers.
func (s *Server) globalJSON(r *http.Request) []byte {
	v, _ := s.cached("global-json", time.Second, func(ctx context.Context) (any, error) {
		g := s.globalStats(ctx)
		b, _ := json.Marshal(map[string]any{
			"today": fmtHours(g.TodayStruggle), "week": fmtHours(g.WeekStruggle), "all": fmtHours(g.AllStruggle),
			"active": g.ActiveNow, "users": g.Users})
		return b, nil
	})
	b, _ := v.([]byte)
	return b
}

func (s *Server) handleLiveGlobal(w http.ResponseWriter, r *http.Request) {
	release, ok := s.acquireSSE(w, r)
	if !ok {
		return
	}
	defer release()
	sub := s.hub.Subscribe("global")
	defer sub.Close()
	e := s.sseStart(w)
	if e.send("stats", s.globalJSON(r)) != nil {
		return
	}
	hb := time.NewTicker(heartbeat)
	tick := time.NewTicker(2 * time.Second)
	defer hb.Stop()
	defer tick.Stop()
	dirty := false
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.C:
			dirty = true // coalesce bursts of syncs into one push per tick
		case <-tick.C:
			if dirty {
				dirty = false
				if e.send("stats", s.globalJSON(r)) != nil {
					return
				}
			}
		case <-hb.C:
			if e.comment("ping") != nil {
				return
			}
		}
	}
}
