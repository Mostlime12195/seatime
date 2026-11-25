package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/emit"
	"seatime/internal/client/store"
)

// fakePeer answers /seatime/ping like a running daemon on a random port.
func fakePeer(t *testing.T) (port int, stop func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"name": "seatime", "pid": 1})
	})}
	go srv.Serve(ln)
	return ln.Addr().(*net.TCPAddr).Port, func() { srv.Close() }
}

// Under launchd (KeepAlive) a second daemon must not exit and be respawned
// every few seconds: it waits for the first one and takes over its port.
func TestServiceDaemonWaitsForPeerThenTakesOver(t *testing.T) {
	sandbox(t)
	port, stopPeer := fakePeer(t)
	f := false
	config.Save(config.Config{WakaPort: port, AutoHook: &f})
	st, err := store.Open(config.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := &Daemon{Version: "test", Store: st, Pipeline: emit.New(st, config.Home()), Service: true, RetryListen: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("service daemon exited while a peer was running: %v", err)
	default:
	}
	stopPeer()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if m, ok := Ping(config.Load()); ok && m["version"] == "test" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiting daemon never took over the port")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Started by hand, a second daemon says so and exits.
func TestManualDaemonRefusesWhenPeerRuns(t *testing.T) {
	sandbox(t)
	port, stopPeer := fakePeer(t)
	defer stopPeer()
	config.Save(config.Config{WakaPort: port})
	st, err := store.Open(config.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := &Daemon{Store: st, Pipeline: emit.New(st, config.Home())}
	if err := d.Run(context.Background()); err == nil {
		t.Fatal("want 'already running'")
	}
}
