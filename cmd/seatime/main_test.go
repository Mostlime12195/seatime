package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestMain lets tests run the real CLI as a child process: with
// SEATIME_TEST_MAIN=1 the test binary behaves like `seatime`.
func TestMain(m *testing.M) {
	if os.Getenv("SEATIME_TEST_MAIN") == "1" {
		if v := os.Getenv("SEATIME_TEST_VERSION"); v != "" {
			version = v
		}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func seatimeCmd(t *testing.T, stdin string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "SEATIME_TEST_MAIN=1")
	cmd.Stdin = strings.NewReader(stdin)
	return cmd
}

// The hook is on the harness's critical path: p95 under 50 ms, exit 0, and
// nothing on stdout for an event that needs no answer. Measured with real
// process spawns against a store that grows as it goes.
func TestHookLatencyP95(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns hundreds of processes")
	}
	home := hookEnv(t)
	t.Setenv("SEATIME_FAKE_HOME", home)
	cwd := filepath.Join(home, "code", "app")
	os.MkdirAll(filepath.Join(cwd, ".git"), 0o755)
	payload := `{"session_id":"s1","cwd":"` + cwd + `","hook_event_name":"UserPromptSubmit","prompt":"make the login form remember the email"}`
	n := 300
	if os.Getenv("SEATIME_HOOK_BENCH_N") == "1000" {
		n = 1000
	}
	var ds []time.Duration
	for i := 0; i < n; i++ {
		cmd := seatimeCmd(t, payload, "hook", "claude-code", "UserPromptSubmit")
		start := time.Now()
		out, err := cmd.Output()
		ds = append(ds, time.Since(start))
		if err != nil || len(out) != 0 {
			t.Fatalf("run %d: err=%v out=%q", i, err, out)
		}
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	p50, p95 := ds[n/2], ds[n*95/100]
	t.Logf("hook over %d runs: p50 %s, p95 %s, max %s", n, p50.Round(100*time.Microsecond), p95.Round(100*time.Microsecond), ds[n-1].Round(time.Millisecond))
	if p95 > 50*time.Millisecond {
		t.Errorf("hook p95 %s is over the 50 ms budget", p95)
	}
	if c := countEvents(t); c != int64(2*n) {
		t.Errorf("stored %d events, want %d", c, 2*n)
	}
}
