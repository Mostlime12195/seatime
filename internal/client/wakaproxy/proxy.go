// Package wakaproxy implements a WakaTime-compatible local endpoint so every
// existing WakaTime/Hackatime editor plugin feeds SeaTime `edit` events.
// The original request is also relayed unchanged to the forward URL (the
// user's previous api_url, e.g. Hackatime) unless they opted out with
// `seatime setup --also-hackatime=false`.
package wakaproxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"seatime/pkg/event"
)

type Sink interface{ Emit(e event.Event) error }

type Handler struct {
	Sink    Sink
	Forward string // upstream api base (e.g. https://hackatime.hackclub.com/api/hackatime/v1) or ""
	// ForwardFn, when set, is consulted per request instead of Forward so a
	// long-running daemon notices `seatime setup` changing the upstream.
	ForwardFn func() string
	HTTP      *http.Client

	mu   sync.Mutex
	seen map[string]struct{}
	ring []string
	n    int
}

func New(sink Sink, forward string) *Handler {
	return &Handler{Sink: sink, Forward: strings.TrimRight(forward, "/"),
		HTTP: &http.Client{Timeout: 25 * time.Second}, seen: map[string]struct{}{}, ring: make([]string, 2048)}
}

func (h *Handler) forward() string {
	if h.ForwardFn != nil {
		return strings.TrimRight(h.ForwardFn(), "/")
	}
	return h.Forward
}

// Heartbeat is the subset of a WakaTime heartbeat SeaTime uses.
type Heartbeat struct {
	Entity   string
	Type     string
	Category string
	Time     float64
	Project  string
	IsWrite  bool
	Lines    int
	LineNo   int // cursor line, 1-based ("lineno")
	Cursor   int // cursor column, 1-based ("cursorpos")
	Added    int // "line_additions"
	Removed  int // "line_deletions"
	UA       string
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

func str(v any) string { s, _ := v.(string); return s }

func parseOne(m map[string]any) Heartbeat {
	hb := Heartbeat{Entity: str(m["entity"]), Type: str(m["type"]), Category: str(m["category"]),
		Time: num(m["time"]), Project: str(m["project"]), Lines: int(num(m["lines"])), UA: str(m["user_agent"]),
		LineNo: max(0, int(num(m["lineno"]))), Cursor: max(0, int(num(m["cursorpos"]))),
		Added: max(0, int(num(m["line_additions"]))), Removed: max(0, int(num(m["line_deletions"])))}
	hb.IsWrite, _ = m["is_write"].(bool)
	return hb
}

// ParseHeartbeats accepts a single heartbeat object or an array of them.
func ParseHeartbeats(body []byte) ([]Heartbeat, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	var out []Heartbeat
	if body[0] == '[' {
		var arr []map[string]any
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, err
		}
		for _, m := range arr {
			out = append(out, parseOne(m))
		}
		return out, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return []Heartbeat{parseOne(m)}, nil
}

var humanCategories = map[string]bool{
	"": true, "coding": true, "debugging": true, "building": true, "code reviewing": true,
	"running tests": true, "writing tests": true, "manual testing": true, "writing docs": true, "designing": true,
}

// AICoding is WakaTime's category for an edit an AI made.
const AICoding = "ai coding"

// EditorFromUA extracts the editor plugin name, e.g. "vscode" from
// "wakatime/v1.90 (linux-x86_64) go1.22 vscode/1.85 vscode-wakatime/24.0".
func EditorFromUA(ua string) string {
	for _, f := range strings.Fields(ua) {
		if i := strings.Index(f, "-wakatime/"); i > 0 {
			return f[:i]
		}
	}
	if ua == "" {
		return "unknown"
	}
	fs := strings.Fields(ua)
	if len(fs) > 0 {
		name, _, _ := strings.Cut(fs[len(fs)-1], "/")
		return strings.ToLower(name)
	}
	return "unknown"
}

// ExtraEditorProject is the Extra key holding the heartbeat's project name.
const ExtraEditorProject = "editor_project"

// ToEvent converts a heartbeat to an edit event; ok=false for non-file
// activity and categories that are not editing (browsing, meetings...).
// "ai coding", an edit the plugin says an AI made, is kept with its
// category: the rule counts it as the agent's heartbeat, never as yours
// (docs/RULE.md, definition 3), with the editor as the tool.
func (hb Heartbeat) ToEvent(headerUA string) (event.Event, bool) {
	if hb.Type != "" && hb.Type != "file" {
		return event.Event{}, false
	}
	cat := strings.ToLower(hb.Category)
	if hb.Entity == "" || !humanCategories[cat] && cat != AICoding {
		return event.Event{}, false
	}
	ua := hb.UA
	if ua == "" {
		ua = headerUA
	}
	e := event.Event{
		Kind: event.Edit, Harness: "editor:" + EditorFromUA(ua),
		Files: []string{hb.Entity}, Write: hb.IsWrite, Lines: hb.Lines,
		LineNo: hb.LineNo, CursorPos: hb.Cursor, Category: cat,
		LineAdditions: hb.Added, LineDeletions: hb.Removed,
	}
	if path.IsAbs(hb.Entity) || (len(hb.Entity) > 2 && hb.Entity[1] == ':') {
		e.CWD = dirOf(hb.Entity)
	}
	if hb.Project != "" {
		// the editor's own project name: the fallback when the daemon may not
		// look inside the file's folder (macOS protected folders)
		e.Extra = map[string]any{ExtraEditorProject: hb.Project}
	}
	if hb.Time > 0 {
		e.TS = time.Unix(0, int64(hb.Time*1e9))
	}
	return e, true
}

func dirOf(p string) string {
	i := strings.LastIndexAny(p, `/\`)
	if i <= 0 {
		return p
	}
	return p[:i]
}

func (h *Handler) fresh(hb Heartbeat) bool {
	k := hb.Entity + "|" + strconv.FormatFloat(hb.Time, 'f', 3, 64)
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.seen[k]; ok {
		return false
	}
	if old := h.ring[h.n%len(h.ring)]; old != "" {
		delete(h.seen, old)
	}
	h.ring[h.n%len(h.ring)] = k
	h.n++
	h.seen[k] = struct{}{}
	return true
}

func isHeartbeatPath(p string) (bulk, ok bool) {
	p = strings.TrimRight(p, "/")
	switch {
	case strings.HasSuffix(p, "/users/current/heartbeats.bulk"):
		return true, true
	case strings.HasSuffix(p, "/users/current/heartbeats"):
		return false, true
	}
	return false, false
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Browsers can't set Authorization on a cross-site simple request, so
	// requiring it (plus no Origin) blocks drive-by forged heartbeats.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	bulk, isHB := isHeartbeatPath(r.URL.Path)
	if !isHB || r.Method != http.MethodPost {
		h.passthrough(w, r, nil)
		return
	}
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var rd io.Reader = http.MaxBytesReader(w, r.Body, 8<<20)
	raw, err := io.ReadAll(rd)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body := raw
	if r.Header.Get("Content-Encoding") == "gzip" {
		if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
			body, _ = io.ReadAll(io.LimitReader(zr, 16<<20))
		}
	}
	hbs, err := ParseHeartbeats(body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for _, hb := range hbs {
		if e, ok := hb.ToEvent(r.UserAgent()); ok && h.fresh(hb) {
			_ = h.Sink.Emit(e)
		}
	}
	if h.forward() != "" {
		h.passthrough(w, r, raw)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if bulk {
		resp := make([]any, len(hbs))
		for i := range hbs {
			resp[i] = []any{map[string]any{"data": map[string]any{"id": fmt.Sprintf("seatime-%d", time.Now().UnixNano()+int64(i))}}, 201}
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"responses": resp})
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": fmt.Sprintf("seatime-%d", time.Now().UnixNano())}})
}

// passthrough relays the request unchanged to the configured upstream. With
// none, the plugin's status-bar read gets an empty today (so the editor shows
// no error) and anything else a 404.
func (h *Handler) passthrough(w http.ResponseWriter, r *http.Request, body []byte) {
	fwd := h.forward()
	if fwd == "" {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/users/current/statusbar/today") {
			_, _ = w.Write([]byte(`{"data":{"grand_total":{"text":"","total_seconds":0},"categories":[]}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
		return
	}
	if body == nil && r.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 8<<20))
	}
	// Strip the local prefix: everything from /users/ on is appended to upstream.
	p := r.URL.Path
	if i := strings.Index(p, "/users/"); i >= 0 {
		p = p[i:]
	}
	u := fwd + p
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	for k, vs := range r.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Host", "Content-Length", "Connection", "Accept-Encoding":
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Connection", "Transfer-Encoding":
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
