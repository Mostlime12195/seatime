package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"seatime/internal/server/store"
	"seatime/pkg/promptcheck"
)

type Server struct {
	cfg    Config
	store  store.Store
	log    *slog.Logger
	hub    *Hub
	lim    *Limiter
	pages  *pages
	now    func() time.Time
	subSem chan struct{} // bounds concurrent segment checks
	sseIP  sync.Map      // ip -> *int32 live SSE connections
	// rechecked is when each user's pending segments were last counted
	// again on a read (apps.go recheckSoon).
	rechecked sync.Map
	cacheMu   sync.Mutex
	cache     map[string]cacheEntry
	swept     time.Time // last expiry sweep of cache
	flights   map[string]*flight
	oidcMu    sync.Mutex
	oidc      *oidcMeta
	// trustHTTP asks Hackatime for trust colours (trust.go).
	trustHTTP *http.Client
	draining  atomic.Bool // shutdown has begun (/healthz answers 503)

	// judge answers prompt checks (promptcheck.go), asked with the targets
	// in the policy's judge list.
	judge promptcheck.Judge

	// policy is the settings in force (settings.go); onPolicy run after
	// each change and at start.
	policy   atomic.Pointer[policy]
	onPolicy []func(ctx context.Context, before, after *policy) error

	healthMu sync.Mutex
	health   judgeHealth
	inflight atomic.Int64 // judge calls in flight
	// periods is the switch's history (store rule_periods), kept in memory.
	periods atomic.Pointer[[]promptcheck.Period]

	// rulesDefault is the default of setting prompt_check_rules: the JSON
	// in SEATIME_PROMPT_CHECK_RULES, else that of SEATIME_PROMPT_CHECK_FILE,
	// else the compiled default rules.
	rulesDefault string
}

type cacheEntry struct {
	at   time.Time
	ttl  time.Duration
	v    any
	busy bool // a background refresh is running
}

// maxCacheEntries bounds the memo cache. Entries past 10x their ttl are
// never served, so the sweep drops them; past the bound the oldest go first.
const maxCacheEntries = 128

func New(cfg Config, st store.Store, log *slog.Logger) (*Server, error) {
	if cfg.Secret == "" {
		if !cfg.Dev {
			return nil, errors.New("SEATIME_SECRET must be set (cookie signing key; e.g. `openssl rand -hex 32`)")
		}
		cfg.Secret = randHex(32)
		log.Warn("SEATIME_SECRET unset; using an ephemeral dev secret")
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.Dev && !devBaseOK(cfg.BaseURL) {
		return nil, fmt.Errorf("SEATIME_DEV=1 lets anyone sign in as anyone; it runs only with a loopback or plain-http SEATIME_BASE_URL, not %q", cfg.BaseURL)
	}
	if cfg.DLUpstream != "" && !safeURL.MatchString(cfg.DLUpstream) {
		return nil, fmt.Errorf("SEATIME_DL_UPSTREAM %q is not a plain http(s) URL", cfg.DLUpstream)
	}
	s := &Server{cfg: cfg, store: st, log: log, hub: NewHub(), lim: NewLimiter(), now: time.Now,
		subSem: make(chan struct{}, 2), cache: map[string]cacheEntry{}, flights: map[string]*flight{}}
	s.trustHTTP = &http.Client{Timeout: trustTimeout}
	if s.cfg.HackatimeURL == "" {
		s.cfg.HackatimeURL = "https://hackatime.hackclub.com"
	}
	s.judge = &promptcheck.Client{Key: cfg.PromptCheckKey}
	s.rulesDefault = string(promptcheck.DefaultRules().JSON())
	if cfg.PromptCheckFile != "" {
		b, err := os.ReadFile(cfg.PromptCheckFile)
		if err != nil {
			return nil, fmt.Errorf("SEATIME_PROMPT_CHECK_FILE: %w", err)
		}
		s.rulesDefault = string(b)
	}
	if cfg.PromptCheckRules != "" {
		s.rulesDefault = cfg.PromptCheckRules
	}
	s.onPolicy = append(s.onPolicy, s.syncRoles, s.recordPeriod)
	if err := s.loadPolicy(context.Background()); err != nil {
		return nil, err
	}
	if st != nil {
		if err := s.policyChanged(context.Background(), &policy{}, s.pol()); err != nil {
			return nil, err
		}
	}
	if r := s.pol().Rules; r != nil {
		log.Info("prompt check rules", "id", r.ID(), "questions", len(r.Questions))
	}
	if s.pol().PromptCheck && cfg.PromptCheckKey == "" {
		// not "off": devices keep their prompts queued and the time pending
		log.Error("the prompt check is on but OPENROUTER_API_KEY is empty: every check answers 503 and waits on the devices until a key is set")
	}
	p, err := loadPages(s)
	if err != nil {
		return nil, err
	}
	s.pages = p
	return s, nil
}

// SetClock replaces the server's clock (checkpoint stamping, expiry, rate
// limits). Production never calls it; the simulator (internal/sim) uses it to
// run hours of activity in seconds. The client's clock is the attacker's; this
// one stays honest.
func (s *Server) SetClock(now func() time.Time) {
	s.now = now
	s.lim.mu.Lock()
	s.lim.nowFunc = now
	s.lim.mu.Unlock()
}

// Start launches background workers; they stop when ctx is cancelled.
func (s *Server) Start(ctx context.Context) {
	go s.recheckLoop(ctx)
	go s.countLoop(ctx)
	go s.showAllChecked(ctx)
	go s.purgeLoop(ctx)
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", s.handleHealth)
	m.HandleFunc("GET /install.sh", s.handleInstallScript(installSH, "text/plain; charset=utf-8"))
	m.HandleFunc("GET /install.ps1", s.handleInstallScript(installPS1, "text/plain; charset=utf-8"))
	m.HandleFunc("GET /dl/{file}", s.handleDownload)
	m.Handle("GET /static/", s.staticHandler())

	// device flow + API
	m.HandleFunc("POST /api/v1/device/start", s.handleDeviceStart)
	m.HandleFunc("POST /api/v1/device/poll", s.handleDevicePoll)
	m.HandleFunc("POST /api/v1/sync", s.handleSync)
	m.HandleFunc("POST /api/v1/records", s.handleRecords)
	m.HandleFunc("POST /api/v1/segments", s.handleSegment)
	m.HandleFunc("POST /api/v1/submissions", s.handleSubmissionsGone)
	m.HandleFunc("POST /api/v1/prompt-check", s.handlePromptCheck)
	m.HandleFunc("POST /api/v1/prompt-hashes", s.handlePromptHashes)
	m.HandleFunc("POST /api/v1/device/check-in", s.handleCheckIn)
	m.HandleFunc("POST /api/v1/device/requests/{id}", s.handleRequestResult)
	m.HandleFunc("GET /api/v1/device/today", s.handleDeviceToday)
	m.HandleFunc("GET /api/v1/users/{user}/projects/{project}/stats", s.handleStats)
	m.HandleFunc("GET /api/v1/review/users", s.handleReviewUsers)
	m.HandleFunc("GET /api/v1/review/users/{user}", s.handleReviewUser)
	m.HandleFunc("GET /api/v1/review/users/{user}/projects/{project}", s.handleReviewEvidence)
	m.HandleFunc("GET /api/v1/users/{user}/projects/{project}/ysws", s.handleYSWSExport(false))
	m.HandleFunc("GET /api/v1/users/{user}/projects/{project}/ysws.txt", s.handleYSWSExport(true))

	// Hackatime-compatible read API (see YSWS.md for exactly what is and isn't compatible)
	m.HandleFunc("GET /api/v1/users/{a}/{b}", s.handleCompatUser2)
	m.HandleFunc("GET /api/v1/users/{user}/project/{project}", s.handleCompatProject)
	m.HandleFunc("GET /api/v1/users/{user}/projects/details", s.handleCompatDetails)
	m.HandleFunc("GET /api/summary", s.handleCompatSummary)
	m.HandleFunc("GET /api/hackatime/v1/users/{id}", s.handleWakaUser)
	m.HandleFunc("GET /api/hackatime/v1/users/{id}/summaries", s.handleWakaSummaries)
	m.HandleFunc("GET /api/hackatime/v1/users/{id}/statusbar/today", s.handleWakaStatusbar)
	m.HandleFunc("GET /api/hackatime/v1/users/{id}/stats/last_7_days", s.handleWakaLast7)

	// auth
	m.HandleFunc("GET /login", s.handleLogin)
	m.HandleFunc("POST /logout", s.handleLogout)
	m.HandleFunc("POST /auth/dev", s.handleDevLogin)
	m.HandleFunc("GET /auth/hackclub", s.handleHCAStart)
	m.HandleFunc("GET /auth/hackclub/callback", s.handleHCACallback)
	m.HandleFunc("GET /auth/github", s.handleGHStart)
	m.HandleFunc("GET /auth/github/callback", s.handleGHCallback)

	// device approval page
	m.HandleFunc("GET /device", s.handleDevicePage)
	m.HandleFunc("POST /device", s.handleDevicePage)
	m.HandleFunc("POST /device/approve", s.handleDeviceApprove)

	// settings
	m.HandleFunc("GET /settings", s.handleSettings)
	m.HandleFunc("POST /settings/profile", s.handleSettingsProfile)
	m.HandleFunc("POST /settings/project", s.handleSettingsProject)
	m.HandleFunc("POST /settings/project/visibility", s.handleProjectVisibility)
	m.HandleFunc("POST /settings/device", s.handleSettingsDevice)
	m.HandleFunc("POST /settings/device/remove", s.handleDeviceRemove)
	m.HandleFunc("POST /settings/computers", s.handleSettingsComputers)
	m.HandleFunc("POST /settings/forget", s.handleForget)
	m.HandleFunc("POST /settings/delete-account", s.handleDeleteAccount)
	m.HandleFunc("POST /settings/prompt-check", s.handleSettingsPromptCheck)

	// applications (OAuth 2.0)
	m.HandleFunc("GET /oauth/authorize", s.handleAuthorize)
	m.HandleFunc("POST /oauth/authorize", s.handleAuthorizeDecision)
	m.HandleFunc("POST /oauth/token", s.handleToken)
	m.HandleFunc("POST /oauth/revoke", s.handleRevoke)
	m.HandleFunc("GET /developers", s.handleDevelopers)
	m.HandleFunc("POST /developers", s.handleDevelopersNew)
	m.HandleFunc("POST /developers/{id}", s.handleDevelopersEdit)
	m.HandleFunc("POST /settings/applications/revoke", s.handleAppRevoke)

	// admin
	m.HandleFunc("GET /admin", s.handleAdmin)
	m.HandleFunc("POST /admin/settings", s.handleAdminSettings)
	m.HandleFunc("POST /admin/prompt-pull", s.handleAdminPromptPull)

	// live
	m.HandleFunc("GET /live/global", s.handleLiveGlobal)
	m.HandleFunc("GET /live/{user}", s.handleLiveUser)

	m.HandleFunc("GET /leaderboard", s.handleLeaderboard)
	m.HandleFunc("GET /{$}", s.handleHome)
	m.HandleFunc("GET /", s.handleCatchAll)
	return s.middleware(m)
}

// ---------------- middleware & helpers ----------------

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int)           { w.code = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self' https://api.github.com; frame-ancestors 'none'; form-action 'self'")
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "path", r.URL.Path, "err", rec)
				if sw.code == 200 {
					http.Error(sw, "internal error", 500)
				}
			}
			// Successful, fast syncs are ~90% of traffic (one per client per 90 s);
			// logging each one cost ~25 KB/s at 10x peak. Errors and slow ones still log.
			quiet := r.URL.Path == "/healthz" || (r.URL.Path == "/api/v1/sync" && sw.code == 200 && time.Since(start) < 250*time.Millisecond)
			if !quiet {
				s.log.Info("req", "method", r.Method, "path", r.URL.Path, "status", sw.code, "ms", time.Since(start).Milliseconds(), "ip", s.clientIP(r))
			}
		}()
		if strings.HasPrefix(r.URL.Path, "/api/") && !s.allowIP(r, "ip", 20, 60) {
			jsonErr(sw, 429, "rate limited")
			return
		}
		next.ServeHTTP(sw, r)
	})
}

// DebugHandler serves /debug/stats and /debug/pprof/ (SEATIME_DEBUG=1).
// seatimed serves it only on its own loopback listener (SEATIME_DEBUG_ADDR),
// never on the public port: it has no login.
func (s *Server) DebugHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /debug/stats", s.handleDebugStats)
	m.HandleFunc("GET /debug/pprof/", pprof.Index)
	m.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	m.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	return m
}

// LoopbackAddr reports whether a listen address is on a loopback interface.
func LoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// devBaseOK reports whether dev mode may run with this public URL: plain
// http, or https on a loopback host.
func devBaseOK(base string) bool {
	if base == "" {
		return true // no public URL at all (tests)
	}
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	if u.Scheme == "http" {
		return true
	}
	h := u.Hostname()
	ip := net.ParseIP(h)
	return h == "localhost" || ip != nil && ip.IsLoopback()
}

// clientIP is the address rate limits and the per-IP caps key on. Behind a
// proxy (SEATIME_TRUST_PROXY=1) it is the X-Forwarded-For entry
// SEATIME_TRUSTED_HOPS from the right: each trusted proxy appends the
// address it saw, so anything further left was written by the client.
//
// On Orchard the chain is "<client-written>, <what the ingress saw>, <the
// ingress pod>", so the setting is 2. The ingress often sees a node address
// (10.x) instead of the client's, because the load balancer rewrites the
// source on nodes without an ingress pod: the client is then unknown, and
// clientIP returns that private address rather than anything the client
// wrote. allowIP gives unknown clients a shared, much larger budget.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if v := r.Header.Values("X-Forwarded-For"); len(v) > 0 {
			parts := strings.Split(strings.Join(v, ","), ",")
			hops := max(s.cfg.TrustedHops, 1)
			i := max(len(parts)-hops, 0)
			if ip := net.ParseIP(strings.TrimSpace(parts[i])); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// unknownIPFactor scales a per-address limit when the address is a proxy's
// (the client's was lost upstream), since every such client shares it.
const unknownIPFactor = 50

// allowIP is a per-address rate limit that stays per-client when the client
// is known and becomes a shared, larger budget when it is not.
func (s *Server) allowIP(r *http.Request, prefix string, rate float64, burst int) bool {
	ip := s.clientIP(r)
	if s.cfg.TrustProxy && unknownClient(ip) {
		return s.lim.Allow(prefix+":unknown", rate*unknownIPFactor, burst*unknownIPFactor)
	}
	return s.lim.Allow(prefix+":"+ip, rate, burst)
}

// unknownClient: an address only a proxy inside the network can have.
func unknownClient(ip string) bool {
	p := net.ParseIP(ip)
	return p != nil && internalIP(p)
}

// internalIP is an address only a proxy inside the network can have.
func internalIP(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func jsonErr(w http.ResponseWriter, code int, msg string) {
	jsonOut(w, code, map[string]string{"error": msg})
}

// handleHealth is the container health check: cheap (one trivial read with
// a one-second budget, no logging, no rate limit) and honest about the
// database. It answers 503 while the database does not answer and once
// shutdown has begun, so the load balancer stops sending traffic first.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.draining.Load() {
		jsonErr(w, 503, "shutting down")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		jsonErr(w, 503, "db unavailable")
		return
	}
	jsonOut(w, 200, map[string]any{"ok": true, "db": "ok"})
}

// Drain marks the server as shutting down: /healthz answers 503 from now on.
func (s *Server) Drain() { s.draining.Store(true) }

// handleDebugStats exposes counters for capacity testing (SEATIME_DEBUG=1 only).
func (s *Server) handleDebugStats(w http.ResponseWriter, r *http.Request) {
	out := map[string]float64{"sse_subscribers": float64(s.hub.Subscribers()), "goroutines": float64(runtime.NumGoroutine())}
	if sp, ok := s.store.(interface{ Stats() map[string]float64 }); ok {
		for k, v := range sp.Stats() {
			out[k] = v
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out["heap_alloc_mb"], out["heap_sys_mb"], out["gc_cycles"] = float64(ms.HeapAlloc)/1e6, float64(ms.HeapSys)/1e6, float64(ms.NumGC)
	out["gc_pause_total_ms"] = float64(ms.PauseTotalNs) / 1e6
	jsonOut(w, 200, out)
}

type flight struct {
	done chan struct{}
	v    any
	err  error
}

// cached memoizes f for ttl under key. Concurrent misses share one computation
// (singleflight), and an entry up to 10x past its ttl is served stale while a
// single background refresh runs, so an expensive aggregate is never computed
// on a request path by more than one caller and never stampedes. f gets its own
// context (not the request's) because the refresh outlives the request.
// Entries expire (10x ttl) and the cache holds at most maxCacheEntries; a
// panic in f becomes an error instead of killing seatimed or stranding waiters.
func (s *Server) cached(key string, ttl time.Duration, f func(ctx context.Context) (any, error)) (any, error) {
	compute := func() (v any, err error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in cached computation", "key", key, "err", rec, "stack", string(debug.Stack()))
				v, err = nil, fmt.Errorf("internal error computing %s", key)
			}
		}()
		return f(ctx)
	}
	s.cacheMu.Lock()
	e, ok := s.cache[key]
	age := s.now().Sub(e.at)
	if ok && age < ttl {
		s.cacheMu.Unlock()
		return e.v, nil
	}
	if ok && age < 10*ttl {
		if !e.busy {
			e.busy = true
			s.cache[key] = e
			go func() {
				v, err := compute()
				s.cacheMu.Lock()
				if err == nil {
					s.putCacheLocked(key, cacheEntry{at: s.now(), ttl: ttl, v: v})
				} else if cur, live := s.cache[key]; live && cur.at.Equal(e.at) { // (unless evicted or replaced meanwhile)
					e.busy = false
					s.cache[key] = e
				}
				s.cacheMu.Unlock()
			}()
		}
		s.cacheMu.Unlock()
		return e.v, nil
	}
	if fl, ok := s.flights[key]; ok {
		s.cacheMu.Unlock()
		<-fl.done
		return fl.v, fl.err
	}
	fl := &flight{done: make(chan struct{})}
	s.flights[key] = fl
	s.cacheMu.Unlock()
	fl.v, fl.err = compute()
	s.cacheMu.Lock()
	delete(s.flights, key)
	if fl.err == nil {
		s.putCacheLocked(key, cacheEntry{at: s.now(), ttl: ttl, v: fl.v})
	}
	s.cacheMu.Unlock()
	close(fl.done)
	return fl.v, fl.err
}

// putCacheLocked stores e and keeps the cache bounded: at most once a minute
// (or whenever it is full) it drops entries too old to be served, then the
// oldest until there is room. Callers hold cacheMu.
func (s *Server) putCacheLocked(key string, e cacheEntry) {
	s.cache[key] = e
	now := s.now()
	if len(s.cache) <= maxCacheEntries && now.Sub(s.swept) < time.Minute {
		return
	}
	s.swept = now
	for k, x := range s.cache {
		if !x.busy && now.Sub(x.at) >= 10*x.ttl {
			delete(s.cache, k)
		}
	}
	for len(s.cache) > maxCacheEntries {
		oldest, first := "", true
		for k, x := range s.cache {
			if k != key && (first || x.at.Before(s.cache[oldest].at)) {
				oldest, first = k, false
			}
		}
		if first {
			break
		}
		delete(s.cache, oldest)
	}
}
