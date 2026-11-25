package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"seatime/internal/server/store"
)

// TestLifecycle runs seatimed configured by environment only, on a SQLite
// file in a directory that does not exist yet. It logs JSON lines to stdout
// and nothing to stderr, answers /healthz, and on SIGTERM drains,
// checkpoints the write-ahead log and exits 0. Without SEATIME_SECRET it
// refuses to start.
func TestLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "seatimed")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	dbPath := filepath.Join(dir, "data", "seatime.db") // the directory does not exist yet
	env := []string{"SEATIME_ADDR=127.0.0.1:0", "SEATIME_DB=" + dbPath, "HOME=" + dir}

	nosecret := exec.Command(bin)
	nosecret.Env = env
	if out, err := nosecret.CombinedOutput(); err == nil || !strings.Contains(string(out), "SEATIME_SECRET must be set") {
		t.Fatalf("started without SEATIME_SECRET: %v\n%s", err, out)
	}

	run := func() (stdout string) {
		t.Helper()
		cmd := exec.Command(bin)
		cmd.Env = append(env, "SEATIME_SECRET=0123456789abcdef0123456789abcdef")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		sc := bufio.NewScanner(pipe)
		addr := ""
		for addr == "" && sc.Scan() {
			line := sc.Text()
			out.WriteString(line + "\n")
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) != nil {
				t.Fatalf("not a JSON log line on stdout: %q", line)
			}
			if m["msg"] == "seatimed listening" {
				addr, _ = m["addr"].(string)
			}
		}
		if addr == "" {
			cmd.Process.Kill()
			t.Fatalf("never listened; stdout:\n%s\nstderr:\n%s", out.String(), stderr.String())
		}
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), `"db":"ok"`) {
			t.Fatalf("healthz: %d %s", resp.StatusCode, body)
		}
		if resp, err := http.Get("http://" + addr + "/"); err != nil || resp.StatusCode != 200 {
			t.Fatalf("home page: %v %v", resp, err)
		} else {
			resp.Body.Close()
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			for sc.Scan() {
				out.WriteString(sc.Text() + "\n")
			}
			done <- cmd.Wait()
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("exit after SIGTERM: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), stderr.String())
			}
		case <-time.After(20 * time.Second):
			cmd.Process.Kill()
			t.Fatal("still running 20 s after SIGTERM")
		}
		if stderr.Len() != 0 {
			t.Fatalf("wrote to stderr: %s", stderr.String())
		}
		return out.String()
	}

	if log := run(); !strings.Contains(log, "write-ahead log checkpointed") {
		t.Fatalf("log:\n%s", log)
	}
	if fi, err := os.Stat(dbPath + "-wal"); err == nil && fi.Size() != 0 {
		t.Fatalf("write-ahead log left with %d bytes", fi.Size())
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Ping(context.Background()); err != nil {
		t.Fatalf("database after shutdown: %v", err)
	}
	st.Close()
}
