package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"seatime/internal/client/config"
	"seatime/internal/client/daemon"
	"seatime/internal/client/localauth"
	"seatime/internal/client/update"
)

func TestSelfTestCommand(t *testing.T) {
	hookEnv(t)
	cmd := seatimeCmd(t, "", "doctor", "--self-test")
	cmd.Env = append(cmd.Env, "SEATIME_TEST_VERSION=v1.2.3")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "\nself-test ok v1.2.3\n") {
		t.Fatalf("%v\n%s", err, out)
	}
	// it wrote nothing into the user's data dir
	if ents, _ := os.ReadDir(config.DataDir()); len(ents) != 0 {
		t.Fatalf("self-test left %v in the data dir", ents)
	}
}

// serveRelease signs a release of bin as version with a throwaway key.
func serveRelease(t *testing.T, version string, bin []byte) (url, pub string) {
	t.Helper()
	pk, priv, _ := ed25519.GenerateKey(rand.Reader)
	name := update.AssetName(runtime.GOOS, runtime.GOARCH)
	s := sha256.Sum256(bin)
	m, err := update.Sign(update.Manifest{Version: version, Files: map[string]string{name: hex.EncodeToString(s[:])}}, priv)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/" + update.ManifestName:
			w.Write(m)
		case "/dl/" + name:
			w.Write(bin)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts.URL + "/dl/", base64.StdEncoding.EncodeToString(pk)
}

// installedCopy puts a copy of this test binary (which runs as seatime when
// SEATIME_TEST_MAIN=1) where an install would.
func installedCopy(t *testing.T) (exe string, self []byte) {
	t.Helper()
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	exe = filepath.Join(t.TempDir(), "seatime")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if err := os.WriteFile(exe, self, 0o755); err != nil {
		t.Fatal(err)
	}
	return exe, self
}

// The whole path with real processes: the new binary is downloaded, swapped
// in and its real `doctor --self-test` decides whether it stays.
func TestUpdateEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("copies the test binary around")
	}
	hookEnv(t)
	t.Setenv("SEATIME_TEST_MAIN", "1") // children of this test run as seatime

	t.Run("good", func(t *testing.T) {
		exe, self := installedCopy(t)
		marked := append(append([]byte{}, self...), "v9.0.0"...) // a different file that runs the same
		url, pub := serveRelease(t, "v9.0.0", marked)
		t.Setenv("SEATIME_TEST_VERSION", "v9.0.0") // what the new binary reports
		u := &update.Updater{Version: "v1.0.0", Exe: exe, BaseURL: url, Dir: config.DataDir(), Key: pub}
		res, err := u.Apply(context.Background())
		if err != nil || !res.Updated {
			t.Fatalf("%+v %v", res, err)
		}
		if b, _ := os.ReadFile(exe); string(b) != string(marked) {
			t.Fatal("new binary not installed")
		}
		if b, _ := os.ReadFile(exe + ".prev"); string(b) != string(self) {
			t.Fatal("previous binary not kept")
		}
	})

	t.Run("self-test fails", func(t *testing.T) {
		exe, self := installedCopy(t)
		url, pub := serveRelease(t, "v9.0.0", append(append([]byte{}, self...), 'x'))
		t.Setenv("SEATIME_TEST_VERSION", "v8.0.0") // runs, but is not the promised release
		dir := t.TempDir()
		u := &update.Updater{Version: "v1.0.0", Exe: exe, BaseURL: url, Dir: dir, Key: pub, Notify: true}
		res, err := u.Apply(context.Background())
		if err != nil || !res.RolledBack {
			t.Fatalf("%+v %v", res, err)
		}
		if b, _ := os.ReadFile(exe); string(b) != string(self) {
			t.Fatal("not rolled back")
		}
		if n := update.TakeNotice(dir); !strings.Contains(n, "went back to v1.0.0") {
			t.Fatalf("notice %q", n)
		}
	})

	if runtime.GOOS != "windows" {
		t.Run("new binary does not even start", func(t *testing.T) {
			exe, self := installedCopy(t)
			url, pub := serveRelease(t, "v9.0.0", []byte("#!/bin/sh\necho 'segmentation fault' >&2\nexit 139\n"))
			u := &update.Updater{Version: "v1.0.0", Exe: exe, BaseURL: url, Dir: t.TempDir(), Key: pub}
			res, err := u.Apply(context.Background())
			if err != nil || !res.RolledBack || !strings.Contains(res.Detail, "segmentation fault") {
				t.Fatalf("%+v %v", res, err)
			}
			if b, _ := os.ReadFile(exe); string(b) != string(self) {
				t.Fatal("not rolled back")
			}
		})
	}
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// `seatime update` asks the running daemon to restart; on Unix it execs the
// binary at its path in place (same PID, so launchd and systemd see nothing).
func TestDaemonRestartsInPlace(t *testing.T) {
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("runs a real daemon; Windows starts a new process instead")
	}
	home := hookEnv(t)
	f := false
	cfg := config.Config{WakaPort: freePort(t), AutoHook: &f, AutoUpdate: &f, Server: "http://127.0.0.1:1"}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	cmd := seatimeCmd(t, "", "daemon")
	cmd.Env = append(cmd.Env, "SEATIME_FAKE_HOME="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	ping := func() map[string]any {
		for i := 0; i < 100; i++ {
			if m, ok := daemon.Ping(cfg); ok {
				return m
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("daemon did not answer")
		return nil
	}
	before := ping()
	time.Sleep(1100 * time.Millisecond) // so uptime tells the two runs apart

	// without the local secret: refused
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/seatime/restart", cfg.WakaPort), nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
		t.Fatalf("restart without the secret: %v %v", resp, err)
	}
	if _, err := localauth.Secret(); err != nil {
		t.Fatal(err)
	}
	msg := restartRunningDaemon(cfg, "dev")
	if !strings.Contains(msg, "restarted on dev") {
		t.Fatal(msg)
	}
	after := ping()
	if after["pid"] != before["pid"] || after["pid"] != float64(cmd.Process.Pid) {
		t.Fatalf("pid changed: %v -> %v", before["pid"], after["pid"])
	}
	if after["uptime_s"].(float64) >= 1 {
		t.Fatalf("not restarted: uptime %v", after["uptime_s"])
	}
	log, _ := os.ReadFile(config.LogPath())
	if strings.Count(string(log), "daemon starting") != 2 || !strings.Contains(string(log), "restarting on") {
		t.Fatalf("log:\n%s", log)
	}
}
