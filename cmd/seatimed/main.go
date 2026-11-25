// seatimed is the SeaTime server: accounts, public profiles, live stats, checked hours for applications.
//
// It is configured by environment only (docs/SERVER.md). SEATIME_DB is a
// postgres:// URL, or the path of a SQLite file for local use. It logs
// JSON lines to stdout and answers /healthz. On SIGTERM it stops taking
// traffic and drains requests before it exits.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata" // zone data in the binary, not from the image

	"seatime/internal/server"
	"seatime/internal/server/store"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "prompts" {
		os.Exit(runPrompts(os.Args[2:], os.Stdout))
	}
	os.Exit(run())
}

// redactDB is SEATIME_DB fit for a log line: a Postgres URL without its password.
func redactDB(dsn string) string {
	if !store.IsPostgres(dsn) {
		return dsn
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "postgres://(unparsable)"
	}
	return u.Redacted()
}

func run() int {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := server.ConfigFromEnv()
	pg := store.IsPostgres(cfg.DB)
	if dir := filepath.Dir(cfg.DB); dir != "" && !pg {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Error("data directory", "path", dir, "err", err)
			return 1
		}
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		log.Error("open db", "db", redactDB(cfg.DB), "err", err)
		return 1
	}
	srv, err := server.New(cfg, st, log)
	if err != nil {
		st.Close()
		log.Error("init", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv.Start(ctx)

	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	hs := &http.Server{
		BaseContext: func(net.Listener) context.Context { return baseCtx },
		Addr:        cfg.Addr, Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 90 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
		MaxHeaderBytes: 32 << 10,
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		st.Close()
		log.Error("listen", "addr", cfg.Addr, "err", err)
		return 1
	}
	if cfg.Debug {
		if !server.LoopbackAddr(cfg.DebugAddr) {
			st.Close()
			log.Error("SEATIME_DEBUG_ADDR must be a loopback address (the debug endpoints have no login)", "addr", cfg.DebugAddr)
			return 1
		}
		dbg := &http.Server{Addr: cfg.DebugAddr, Handler: srv.DebugHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			log.Info("debug endpoints listening", "addr", cfg.DebugAddr)
			if err := dbg.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("debug listener", "err", err)
			}
		}()
		defer dbg.Close()
	}
	served := make(chan error, 1)
	go func() {
		log.Info("seatimed listening", "addr", ln.Addr().String(), "base_url", cfg.BaseURL, "db", redactDB(cfg.DB), "dev", cfg.Dev, "dl_dir", cfg.DLDir, "dl_upstream", cfg.DLUpstream)
		served <- hs.Serve(ln)
	}()
	code := 0
	select {
	case <-ctx.Done():
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			code = 1
		}
	}
	log.Info("shutting down")
	srv.Drain()  // /healthz answers 503: no new traffic
	cancelBase() // ends long-lived SSE streams so Shutdown can drain
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil {
		log.Warn("drain", "err", err)
	}
	if err := st.Close(); err != nil {
		log.Error("close db", "err", err)
		return 1
	}
	if pg {
		log.Info("bye: database closed")
	} else {
		log.Info("bye: database closed, write-ahead log checkpointed")
	}
	return code
}
